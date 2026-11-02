package docker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/agent"
	"github.com/krishnanand20/einsora/controlplane/internal/agent/drivers/pythonruntime"
	"github.com/krishnanand20/einsora/controlplane/internal/agent/protocol"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/redact"
)

type Options struct {
	Root   string // private persistent state; never mounted into a workload
	Runs   string // short parent for per-workload Unix sockets; empty means Root/run
	Socket string // local rootful Docker Engine socket; empty means /var/run/docker.sock
	Log    *slog.Logger
}

const (
	preparing = "preparing"
	running   = "running"
	stopping  = "stopping"
	stopped   = "stopped"
	removing  = "removing"
	failed    = "failed"
)

type state struct {
	Desired  protocol.Desired `json:"desired"`
	Phase    string           `json:"phase"`
	Run      string           `json:"run"`
	Token    string           `json:"token"`
	Devices  []string         `json:"devices"`
	Spec     string           `json:"spec"`
	Policy   string           `json:"policy"`
	Code     protocol.Code    `json:"code,omitempty"`
	Verified bool             `json:"verified"` // create had no warnings and effective protections were checked
}

type Driver struct {
	opts               Options
	engine             *engine
	instance           string
	runs, bundle, user string
	uid, gid           int
	devices            []Device
	ctx                context.Context
	cancel             context.CancelFunc
	mu                 sync.Mutex
	locks              map[uuid.UUID]*sync.Mutex
	jobs               map[uuid.UUID]context.CancelFunc
	closed             bool
	wg                 sync.WaitGroup
	alloc              sync.Mutex
}

func newDriver(opts Options, e *engine, devices []Device, uid, gid int) (*Driver, error) {
	root, err := privateDirectory(opts.Root, os.Getuid())
	if err != nil {
		return nil, err
	}
	opts.Root = root
	for _, sub := range []string{"workloads", "runtimes"} {
		if _, err := privateDirectory(filepath.Join(root, sub), os.Getuid()); err != nil {
			return nil, err
		}
	}
	runs := opts.Runs
	if runs == "" {
		runs = filepath.Join(root, "run")
	}
	runs, err = privateDirectory(runs, os.Getuid())
	if err != nil {
		return nil, err
	}
	if len(runs)+1+8+len("/server.sock") > protocol.MaxSocketPath {
		return nil, fmt.Errorf("docker Unix socket paths are too long; set a shorter EINSORA_AGENT_RUN_DIR")
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	source := pythonruntime.Bytes()
	sum := sha256.Sum256(source)
	bundle := filepath.Join(root, "runtimes", hex.EncodeToString(sum[:])+".zip")
	// The bundle contains public source only. A non-root container must be able to read it even
	// when its agent is root; the bind mount itself is read-only.
	if err := writeArtifact(bundle, source); err != nil {
		return nil, err
	}
	instance := sha256.Sum256([]byte(root))
	ctx, cancel := context.WithCancel(context.Background())
	d := &Driver{opts: opts, engine: e, instance: hex.EncodeToString(instance[:8]), runs: runs, bundle: bundle,
		user: fmt.Sprintf("%d:%d", uid, gid), uid: uid, gid: gid, devices: slices.Clone(devices), ctx: ctx, cancel: cancel,
		locks: map[uuid.UUID]*sync.Mutex{}, jobs: map[uuid.UUID]context.CancelFunc{}}
	if len(devices) == 0 {
		opts.Log.Info("Docker offers CPU only; NVIDIA needs the container toolkit and physical device UUIDs")
	}
	return d, nil
}

func (d *Driver) Name() string { return domain.DriverDocker }
func (d *Driver) Accelerators() []domain.AcceleratorKind {
	if len(d.devices) > 0 {
		return []domain.AcceleratorKind{domain.AcceleratorCPU, domain.AcceleratorNVIDIA}
	}
	return []domain.AcceleratorKind{domain.AcceleratorCPU}
}
func (d *Driver) Close() {
	d.mu.Lock()
	d.closed = true
	d.cancel()
	d.mu.Unlock()
	d.wg.Wait()
	d.engine.close()
}
func (d *Driver) lock(id uuid.UUID) *sync.Mutex {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.locks[id] == nil {
		d.locks[id] = &sync.Mutex{}
	}
	return d.locks[id]
}
func (d *Driver) dir(id uuid.UUID) string {
	return filepath.Join(d.opts.Root, "workloads", id.String())
}
func (d *Driver) run(st state) string    { return filepath.Join(d.runs, st.Run) }
func (d *Driver) socket(st state) string { return filepath.Join(d.run(st), "server.sock") }
func (d *Driver) name(st state) string {
	return fmt.Sprintf("einsora-%s-%s-%d", d.instance, st.Desired.ID, st.Desired.Attempt)
}
func (d *Driver) network(id uuid.UUID) string { return "einsora-" + d.instance + "-" + id.String() }
func (d *Driver) policy(kind domain.WorkloadKind) string {
	text := "docker-isolation-v1"
	if kind == domain.KindEndpoint {
		text += d.bundle
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
func (d *Driver) write(st state) error {
	return writeJSON(filepath.Join(d.dir(st.Desired.ID), "state.json"), st)
}
func (d *Driver) read(id uuid.UUID) (state, bool, error) {
	var st state
	err := readJSON(filepath.Join(d.dir(id), "state.json"), &st)
	if errors.Is(err, os.ErrNotExist) {
		return st, false, nil
	}
	if err != nil {
		return st, false, err
	}
	_, bad := hex.DecodeString(st.Run)
	if st.Desired.ID != id || len(st.Run) != 8 || bad != nil || st.Desired.Attempt < 1 || len(st.Token) != 64 {
		return state{}, false, fmt.Errorf("invalid Docker workload state")
	}
	return st, true, nil
}
func (d *Driver) states() ([]state, error) {
	entries, err := os.ReadDir(filepath.Join(d.opts.Root, "workloads"))
	if err != nil {
		return nil, err
	}
	var out []state
	for _, entry := range entries {
		id, err := uuid.Parse(entry.Name())
		if err != nil || !entry.IsDir() {
			continue
		}
		lock := d.lock(id)
		lock.Lock()
		st, known, err := d.read(id)
		lock.Unlock()
		if err != nil {
			return nil, err
		}
		if known {
			out = append(out, st)
		}
	}
	return out, nil
}

// schedule never blocks on an image pull. A canceled preparation remains recorded for the next
// agent to resume; stopping/removing intent is persisted before canceling its job.
func (d *Driver) schedule(st state) {
	d.mu.Lock()
	if d.closed || d.jobs[st.Desired.ID] != nil {
		d.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(d.ctx, 30*time.Minute)
	d.jobs[st.Desired.ID] = cancel
	d.wg.Add(1)
	d.mu.Unlock()
	go func() {
		defer d.wg.Done()
		defer cancel()
		defer func() { d.mu.Lock(); delete(d.jobs, st.Desired.ID); d.mu.Unlock() }()
		if st.Phase == preparing {
			d.prepare(ctx, st)
		} else {
			d.finishStop(ctx, st)
		}
	}()
}
func (d *Driver) cancelJob(id uuid.UUID) {
	d.mu.Lock()
	if cancel := d.jobs[id]; cancel != nil {
		cancel()
	}
	d.mu.Unlock()
}

func (d *Driver) Run(ctx context.Context, w protocol.Desired) error {
	if err := validateDesired(w, len(d.devices) > 0); err != nil {
		return agent.WithCode(protocol.CodeInvalid, err)
	}
	// Device reservations and the corresponding state write are serialized across workloads.
	d.alloc.Lock()
	defer d.alloc.Unlock()
	all, err := d.states()
	if err != nil {
		return err
	}
	used := map[string]bool{}
	for _, st := range all {
		if st.Desired.ID != w.ID && activePhase(st.Phase) {
			if w.Devices > 0 && st.Desired.Accelerator == "" {
				return agent.WithCode(protocol.CodeDevicesBusy, fmt.Errorf("an unknown Docker allocation is still being removed"))
			}
			for _, id := range st.Devices {
				used[id] = true
			}
		}
	}
	if w.Devices > 0 {
		orphans, err := d.orphans(ctx, all)
		if err != nil {
			return err
		}
		for _, orphan := range orphans {
			if orphan.State == protocol.StateProgressing || orphan.State == protocol.StateReady {
				return agent.WithCode(protocol.CodeDevicesBusy, fmt.Errorf("unlisted Docker workloads must stop before allocating GPUs"))
			}
		}
	}
	lock := d.lock(w.ID)
	lock.Lock()
	defer lock.Unlock()
	previous, known, err := d.read(w.ID)
	if err != nil {
		return err
	}
	if known && previous.Desired.Attempt == w.Attempt {
		if previous.Phase == preparing {
			d.schedule(previous)
		}
		return nil // attempts are never restarted without a new admission
	}
	if known && activePhase(previous.Phase) {
		return agent.WithCode(protocol.CodeDevicesBusy, fmt.Errorf("the previous attempt has not stopped"))
	}
	if known {
		if err := d.removeContainer(ctx, previous); err != nil {
			return err
		}
	}
	ids, ok := selectDevices(w, d.devices, used)
	if !ok {
		return agent.WithCode(protocol.CodeDevicesBusy, fmt.Errorf("no unused physical GPUs match the requested class"))
	}
	data := filepath.Join(d.dir(w.ID), "data")
	if fi, err := os.Lstat(data); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("workload data must be a real directory")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else if known || w.Kind == domain.KindNotebook && w.Attempt > 1 {
		return agent.WithCode(protocol.CodeDiskMissing, fmt.Errorf("its saved notebook data is missing"))
	}
	if err := os.MkdirAll(d.dir(w.ID), 0o700); err != nil {
		return err
	}
	for _, sub := range []string{"data", "home"} {
		if err := makeWorkDirectory(filepath.Join(d.dir(w.ID), sub), d.uid, d.gid); err != nil {
			return err
		}
	}
	run := previous.Run
	if run == "" {
		run, err = reserveRun(d.runs, d.uid, d.gid)
		if err != nil {
			return err
		}
	}
	if err := writeJSON(filepath.Join(d.runs, run+".owner.json"), map[string]string{"instance": d.instance, "workload": w.ID.String()}); err != nil {
		return err
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	st := state{Desired: w, Phase: preparing, Run: run, Token: hex.EncodeToString(random[:]), Devices: ids, Policy: d.policy(w.Kind)}
	if err := d.write(st); err != nil {
		return err
	}
	d.schedule(st)
	return nil
}

func activePhase(phase string) bool {
	return phase == preparing || phase == running || phase == stopping || phase == removing
}

func (d *Driver) prepare(ctx context.Context, st state) {
	image, err := d.engine.ensureImage(ctx, st.Desired.Runtime.Ref)
	if ctx.Err() != nil {
		return
	}
	lock := d.lock(st.Desired.ID)
	lock.Lock()
	defer lock.Unlock()
	current, known, readErr := d.read(st.Desired.ID)
	if readErr != nil || !known || current.Token != st.Token || current.Phase != preparing || d.ctx.Err() != nil {
		return
	}
	if err != nil {
		d.failPreparation(ctx, current, protocol.CodeImage, err)
		return
	}
	if !cleanID(image) {
		d.failPreparation(ctx, current, protocol.CodeImage, fmt.Errorf("docker returned an invalid image ID"))
		return
	}
	// A recovered create/start uses the recorded spec, token, and device IDs. It never clears the
	// socket of a server that may already be running after an ambiguous API response.
	if info, inspectErr := d.engine.inspect(ctx, d.name(current)); inspectErr == nil {
		if !owned(info, d.instance, current) {
			d.failPreparation(ctx, current, protocol.CodeDriverProblem, fmt.Errorf("refusing a container owned by a different attempt"))
			return
		}
		if !current.Verified || d.checkProtection(info, current) != nil {
			current.Phase, current.Code = stopping, protocol.CodeDriverProblem
			_ = d.write(current)
			return
		}
		if !info.State.Running {
			if err := d.engine.containerAction(ctx, http.MethodPost, d.name(current), "/start"); err != nil {
				d.failPreparation(ctx, current, protocol.CodeDriverProblem, err)
				return
			}
		}
		current.Phase = running
		_ = d.write(current)
		return
	} else if !isHTTP(inspectErr, http.StatusNotFound) {
		d.failPreparation(ctx, current, protocol.CodeDriverProblem, inspectErr)
		return
	}
	if _, err := privateDirectory(d.run(current), d.uid); err != nil {
		d.failPreparation(ctx, current, protocol.CodeDriverProblem, err)
		return
	}
	entries, err := os.ReadDir(d.run(current))
	if err != nil {
		d.failPreparation(ctx, current, protocol.CodeDriverProblem, err)
		return
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(d.run(current), entry.Name())); err != nil {
			d.failPreparation(ctx, current, protocol.CodeDriverProblem, err)
			return
		}
	}
	if err := d.engine.network(ctx, d.network(st.Desired.ID), labels(d.instance, st.Desired), true); err != nil {
		d.failPreparation(ctx, current, protocol.CodeDriverProblem, err)
		return
	}
	config := buildContainer(d.instance, image, d.dir(st.Desired.ID), d.run(current), d.bundle, d.network(st.Desired.ID), d.user, st.Token, st.Desired, st.Devices, downloadCredentials())
	current.Spec = config.Labels[specLabel]
	if err := d.write(current); err != nil {
		d.opts.Log.Error("couldn't record Docker spec", "workload", st.Desired.ID, "err", err)
		return
	}
	if err := d.engine.create(ctx, d.name(current), config); err != nil {
		d.failPreparation(ctx, current, protocol.CodeDriverProblem, err)
		return
	}
	info, err := d.engine.inspect(ctx, d.name(current))
	if err != nil || !owned(info, d.instance, current) {
		if err == nil {
			err = fmt.Errorf("container ownership does not match its saved spec")
		}
		d.failPreparation(ctx, current, protocol.CodeDriverProblem, err)
		return
	}
	if err := d.checkProtection(info, current); err != nil {
		d.failPreparation(ctx, current, protocol.CodeDriverProblem, err)
		return
	}
	current.Verified = true
	if err := d.write(current); err != nil {
		d.opts.Log.Error("couldn't record verified Docker protections", "workload", st.Desired.ID, "err", err)
		return
	}
	if err := d.engine.containerAction(ctx, http.MethodPost, d.name(current), "/start"); err != nil {
		d.failPreparation(ctx, current, protocol.CodeDriverProblem, err)
		return
	}
	current.Phase = running
	_ = d.write(current)
}

// A failed/ambiguous mutation cannot release GPU capacity until its container is confirmed absent
// or stopped. If start succeeded despite a lost reply, adopt that matching server instead.
func (d *Driver) failPreparation(ctx context.Context, st state, code protocol.Code, cause error) {
	d.opts.Log.Warn("couldn't prepare Docker workload", "workload", st.Desired.ID, "err", redact.Secrets(cause.Error()))
	info, err := d.engine.inspect(ctx, d.name(st))
	switch {
	case err == nil && owned(info, d.instance, st) && info.State.Running && st.Verified && d.checkProtection(info, st) == nil:
		st.Phase, st.Code = running, ""
	case err == nil && owned(info, d.instance, st) && (info.State.Running || info.State.Paused || info.State.Restarting):
		st.Phase, st.Code = stopping, code
	case isHTTP(err, http.StatusNotFound) || err == nil && owned(info, d.instance, st):
		st.Phase, st.Code = failed, code
	default:
		st.Code = protocol.CodeDriverProblem // retain the active reservation until the Engine answers
	}
	_ = d.write(st)
}

func (d *Driver) Stop(ctx context.Context, id uuid.UUID) error   { return d.requestStop(ctx, id, false) }
func (d *Driver) Remove(ctx context.Context, id uuid.UUID) error { return d.requestStop(ctx, id, true) }
func (d *Driver) requestStop(ctx context.Context, id uuid.UUID, remove bool) error {
	lock := d.lock(id)
	lock.Lock()
	defer lock.Unlock()
	st, known, err := d.read(id)
	if err != nil {
		return err
	}
	if !known {
		if !remove {
			return d.stopOrphans(ctx, id, false)
		}
		st, err = d.removalState(ctx, id)
		if err != nil {
			return err
		}
	}
	if st.Phase == removing {
		d.schedule(st)
		return nil
	}
	if !remove && st.Phase == stopped {
		return nil
	}
	st.Phase = stopping
	if remove {
		st.Phase = removing
	}
	st.Code = ""
	if err := d.write(st); err != nil {
		return err
	}
	d.cancelJob(id)
	d.schedule(st)
	return nil
}

func (d *Driver) finishStop(ctx context.Context, st state) {
	lock := d.lock(st.Desired.ID)
	lock.Lock()
	defer lock.Unlock()
	current, known, err := d.read(st.Desired.ID)
	if err != nil || !known || current.Token != st.Token || (current.Phase != stopping && current.Phase != removing) {
		return
	}
	if current.Phase == removing {
		if err := d.stopContainer(ctx, current); err != nil {
			d.logStop(current, err)
			return
		}
		if err := os.RemoveAll(d.run(current)); err != nil {
			d.logStop(current, err)
			return
		}
		if err := os.Remove(filepath.Join(d.runs, current.Run+".owner.json")); err != nil && !os.IsNotExist(err) {
			d.logStop(current, err)
			return
		}
		// Include older stopped attempts retained after a lost state file. Ownership markers
		// outside writable mounts keep their cleanup inside this workload's directories.
		if err := d.stopOrphans(ctx, st.Desired.ID, true); err != nil {
			d.logStop(current, err)
			return
		}
		return
	}
	if err := d.stopContainer(ctx, current); err != nil {
		d.logStop(current, err)
		return
	}
	current.Phase = stopped
	if current.Code != "" {
		current.Phase = failed
	}
	_ = d.write(current)
}
func (d *Driver) logStop(st state, err error) {
	d.opts.Log.Warn("couldn't stop or remove Docker workload", "workload", st.Desired.ID, "err", redact.Secrets(err.Error()))
}

func (d *Driver) stopContainer(ctx context.Context, st state) error {
	info, err := d.engine.inspect(ctx, d.name(st))
	if isHTTP(err, http.StatusNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !owned(info, d.instance, st) {
		return fmt.Errorf("refusing to stop a container not owned by this attempt")
	}
	if info.State.Running || info.State.Paused || info.State.Restarting {
		if err := d.engine.containerAction(ctx, http.MethodPost, d.name(st), "/stop?t=5"); err != nil && !isHTTP(err, http.StatusNotModified) {
			return err
		}
	}
	info, err = d.engine.inspect(ctx, d.name(st))
	if isHTTP(err, http.StatusNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.State.Running || info.State.Restarting || info.State.Paused {
		return fmt.Errorf("docker processes have not stopped")
	}
	return nil
}
func (d *Driver) removeContainer(ctx context.Context, st state) error {
	if err := d.stopContainer(ctx, st); err != nil {
		return err
	}
	err := d.engine.containerAction(ctx, http.MethodDelete, d.name(st), "?v=true")
	if isHTTP(err, http.StatusNotFound) {
		return nil
	}
	return err
}

func (d *Driver) Statuses(ctx context.Context) ([]protocol.Status, error) {
	all, err := d.states()
	if err != nil {
		return nil, err
	}
	var statuses []protocol.Status
	for _, saved := range all {
		lock := d.lock(saved.Desired.ID)
		lock.Lock()
		st, known, err := d.read(saved.Desired.ID)
		if err != nil {
			lock.Unlock()
			return nil, err
		}
		if !known {
			lock.Unlock()
			continue
		}
		if (st.Phase == running || st.Phase == preparing) && st.Policy != d.policy(st.Desired.Kind) {
			st.Phase, st.Code = stopping, protocol.CodeRuntimeChanged
			if err := d.write(st); err != nil {
				lock.Unlock()
				return nil, err
			}
			d.cancelJob(st.Desired.ID)
		}
		s := protocol.Status{ID: st.Desired.ID, Attempt: st.Desired.Attempt, State: protocol.StateProgressing}
		switch st.Phase {
		case preparing:
			s.Code = st.Code
			if s.Code == "" {
				s.Code = protocol.CodePullingImage
			}
			d.schedule(st)
		case stopping, removing:
			d.schedule(st)
		case stopped:
			s.State = protocol.StateStopped
		case failed:
			s.State, s.Code = protocol.StateFailed, st.Code
		case running:
			info, err := d.engine.inspect(ctx, d.name(st))
			if err != nil && !isHTTP(err, http.StatusNotFound) {
				lock.Unlock()
				return nil, err
			}
			if err == nil && !owned(info, d.instance, st) {
				lock.Unlock()
				return nil, fmt.Errorf("docker workload's container no longer matches its saved spec")
			}
			switch {
			case err != nil || !info.State.Running:
				if err == nil && (info.State.Paused || info.State.Restarting) {
					s.Code = protocol.CodeStarting
					break
				}
				st.Phase, st.Code = failed, protocol.CodeExited
				if info.State.OOMKilled {
					st.Code = protocol.CodeOutOfMemory
				} else if st.Desired.Kind == domain.KindEndpoint {
					st.Code = d.workerCode(st, true)
				}
				if err := d.write(st); err != nil {
					lock.Unlock()
					return nil, err
				}
				s.State, s.Code = protocol.StateFailed, st.Code
			case d.healthy(ctx, st):
				s.State, s.Access = protocol.StateReady, &protocol.Access{Socket: d.socket(st), Token: st.Token}
			default:
				s.Code = protocol.CodeStarting
				if st.Desired.Kind == domain.KindEndpoint {
					s.Code = d.workerCode(st, false)
				}
			}
		default:
			lock.Unlock()
			return nil, fmt.Errorf("unknown Docker workload phase")
		}
		lock.Unlock()
		statuses = append(statuses, s)
	}
	orphans, err := d.orphans(ctx, all)
	if err != nil {
		return nil, err
	}
	statuses = append(statuses, orphans...)
	return statuses, nil
}

func (d *Driver) healthy(ctx context.Context, st state) bool {
	path := "/api"
	if st.Desired.Kind == domain.KindEndpoint {
		path = "/health"
	}
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", d.socket(st))
		}}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost"+path, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (d *Driver) workerCode(st state, exited bool) protocol.Code {
	fallback := protocol.CodeFetchingModel
	if exited {
		fallback = protocol.CodeExited
	}
	var status struct {
		Stage string        `json:"stage"`
		Code  protocol.Code `json:"code"`
	}
	if err := readJSON(filepath.Join(d.run(st), "status.json"), &status); err != nil {
		return fallback
	}
	if exited {
		switch status.Code {
		case protocol.CodeModelRejected, protocol.CodeModelNotFound, protocol.CodeImage, protocol.CodeDriverProblem:
			return status.Code
		}
		return fallback
	}
	switch status.Stage {
	case "validating":
		return protocol.CodeValidatingModel
	case "loading":
		return protocol.CodeLoadingModel
	}
	return fallback
}

func downloadCredentials() []string {
	var out []string
	for _, key := range []string{"HF_TOKEN", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_REGION", "AWS_DEFAULT_REGION", "AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_S3", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY"} {
		if value, ok := os.LookupEnv(key); ok {
			out = append(out, key+"="+value)
		}
	}
	return out
}

// strconv stays at the boundary for labels parsed from the Engine rather than being trusted state.
func parseAttempt(text string) (int64, error) { return strconv.ParseInt(text, 10, 64) }
