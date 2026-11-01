package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/agent/protocol"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
)

type listedContainer struct {
	ID     string `json:"Id"`
	Labels map[string]string
	State  string
}

func (d *Driver) listed(ctx context.Context) ([]listedContainer, error) {
	filters := fmt.Sprintf(`{"label":[%q]}`, instanceLabel+"="+d.instance)
	var out []listedContainer
	err := d.engine.do(ctx, http.MethodGet, "/containers/json?"+url.Values{"all": {"true"}, "filters": {filters}}.Encode(), nil, &out)
	return out, err
}

// A missing state file never makes a live allocation disappear. Unknown owned containers are
// reported active until the agent parks them; that also keeps every GPU unavailable meanwhile.
func (d *Driver) orphans(ctx context.Context, known []state) ([]protocol.Status, error) {
	containers, err := d.listed(ctx)
	if err != nil {
		return nil, err
	}
	ids := map[uuid.UUID]bool{}
	for _, st := range known {
		ids[st.Desired.ID] = true
	}
	var out []protocol.Status
	for _, container := range containers {
		if container.Labels[instanceLabel] != d.instance {
			return nil, fmt.Errorf("docker returned a container outside this agent's ownership filter")
		}
		id, err := uuid.Parse(container.Labels[workloadLabel])
		if err != nil {
			return nil, fmt.Errorf("owned Docker container has an invalid workload label")
		}
		attempt, err := parseAttempt(container.Labels[attemptLabel])
		if err != nil || attempt < 1 {
			return nil, fmt.Errorf("owned Docker container has an invalid attempt label")
		}
		if !ids[id] {
			state := protocol.StateStopped
			if container.State == "running" || container.State == "paused" || container.State == "restarting" {
				state = protocol.StateProgressing
			}
			out = append(out, protocol.Status{ID: id, Attempt: attempt, State: state, Code: protocol.CodeDriverProblem})
		}
	}
	return out, nil
}

func (d *Driver) stopOrphans(ctx context.Context, id uuid.UUID, remove bool) error {
	containers, err := d.listed(ctx)
	if err != nil {
		return err
	}
	for _, container := range containers {
		if container.Labels[instanceLabel] != d.instance || container.Labels[workloadLabel] != id.String() {
			continue
		}
		info, err := d.engine.inspect(ctx, container.ID)
		if isHTTP(err, http.StatusNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Config.Labels[instanceLabel] != d.instance || info.Config.Labels[workloadLabel] != id.String() {
			return fmt.Errorf("refusing to stop an unknown Docker container")
		}
		if info.State.Running || info.State.Paused || info.State.Restarting {
			if err := d.engine.containerAction(ctx, http.MethodPost, container.ID, "/stop?t=5"); err != nil && !isHTTP(err, http.StatusNotModified) {
				return err
			}
			info, err = d.engine.inspect(ctx, container.ID)
			if err != nil && !isHTTP(err, http.StatusNotFound) {
				return err
			}
			if info.State.Running || info.State.Paused || info.State.Restarting {
				return fmt.Errorf("unknown Docker workload has not stopped")
			}
		}
		if remove {
			// Keep the stopped container's run label discoverable until its files are gone.
			if err := d.removeOrphanRun(container.Labels[runLabel], id); err != nil {
				return err
			}
			if err := d.engine.containerAction(ctx, http.MethodDelete, container.ID, "?v=true"); err != nil && !isHTTP(err, http.StatusNotFound) {
				return err
			}
		}
	}
	if remove {
		name := d.network(id)
		want := map[string]string{instanceLabel: d.instance, workloadLabel: id.String()}
		if err := d.engine.network(ctx, name, want, false); err == nil {
			if err := d.engine.do(ctx, http.MethodDelete, "/networks/"+name, nil, nil); err != nil && !isHTTP(err, http.StatusNotFound) {
				return err
			}
		} else if !isHTTP(err, http.StatusNotFound) {
			return err
		}
		return removeWorkloadDirectory(d.dir(id))
	}
	return nil
}

// An orphan's final container is not its only deletion record: record removal first so a failed
// network/filesystem cleanup remains visible and retryable after that container is gone.
func (d *Driver) removalState(ctx context.Context, id uuid.UUID) (state, error) {
	containers, err := d.listed(ctx)
	if err != nil {
		return state{}, err
	}
	st := state{Desired: protocol.Desired{ID: id, Attempt: 1, Kind: domain.KindNotebook}, Phase: removing}
	for _, container := range containers {
		if container.Labels[instanceLabel] != d.instance || container.Labels[workloadLabel] != id.String() {
			continue
		}
		attempt, err := parseAttempt(container.Labels[attemptLabel])
		if err != nil || attempt < 1 {
			return state{}, fmt.Errorf("invalid owned container attempt")
		}
		st.Desired.Attempt, st.Spec = attempt, container.Labels[specLabel]
		run := container.Labels[runLabel]
		if _, err := hex.DecodeString(run); err == nil && len(run) == 8 {
			var owner map[string]string
			if err := readJSON(filepath.Join(d.runs, run+".owner.json"), &owner); err == nil && owner["instance"] == d.instance && owner["workload"] == id.String() {
				st.Run = run
			}
		}
		break
	}
	if st.Run == "" {
		st.Run, err = reserveRun(d.runs, d.uid, d.gid)
		if err != nil {
			return state{}, err
		}
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return state{}, err
	}
	st.Token = hex.EncodeToString(random[:])
	if err := os.MkdirAll(d.dir(id), 0o700); err != nil {
		return state{}, err
	}
	if err := d.write(st); err != nil {
		return state{}, err
	}
	return st, nil
}

// Keep the tombstone until everything else is gone. RemoveAll on the entire directory could
// unlink state.json and then fail on a data file, leaving no record for the next retry.
func removeWorkloadDirectory(path string) error {
	entries, err := os.ReadDir(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == "state.json" || entry.Name() == "state.json.tmp" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(path, entry.Name())); err != nil {
			return err
		}
	}
	for _, name := range []string{"state.json.tmp", "state.json"} {
		if err := os.Remove(filepath.Join(path, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return os.Remove(path)
}

func (d *Driver) removeOrphanRun(run string, id uuid.UUID) error {
	if len(run) != 8 {
		return nil
	}
	if _, err := hex.DecodeString(run); err != nil {
		return fmt.Errorf("invalid owned run label: %w", err)
	}
	var owner map[string]string
	path := filepath.Join(d.runs, run+".owner.json")
	if err := readJSON(path, &owner); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if owner["instance"] != d.instance || owner["workload"] != id.String() {
		return fmt.Errorf("refusing another workload's run directory")
	}
	if err := os.RemoveAll(filepath.Join(d.runs, run)); err != nil {
		return err
	}
	return os.Remove(path)
}
