package docker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tensora/tensora/controlplane/internal/agent"
	"github.com/tensora/tensora/controlplane/internal/agent/protocol"
	"github.com/tensora/tensora/controlplane/internal/domain"
)

type fakeContainer struct {
	name     string
	config   containerConfig
	info     containerInfo
	listener net.Listener
	server   *http.Server
}
type fakeEngine struct {
	mu                sync.Mutex
	server            *httptest.Server
	containers        map[string]*fakeContainer
	networks          map[string]map[string]string
	imageID           string
	haveImage         bool
	pullFailure       bool
	pullBlock         <-chan struct{}
	pullEntered       chan struct{}
	pullOnce          sync.Once
	lostStartReply    bool
	failStop          bool
	info              map[string]any
	warnings          []string
	inspectFailures   int
	failNetworkDelete bool
	loseLimits        bool
}

func newFakeEngine(t *testing.T) *fakeEngine {
	t.Helper()
	sum := sha256.Sum256([]byte("test image"))
	f := &fakeEngine{containers: map[string]*fakeContainer{}, networks: map[string]map[string]string{}, imageID: "sha256:" + hex.EncodeToString(sum[:]), haveImage: true, pullEntered: make(chan struct{}), info: map[string]any{
		"OSType": "linux", "Runtimes": map[string]any{"runc": map[string]string{}, "nvidia": map[string]string{}},
		"MemoryLimit": true, "SwapLimit": true, "PidsLimit": true, "CpuCfsQuota": true}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, c := range f.containers {
			if c.server != nil {
				_ = c.server.Close()
			}
		}
		f.server.Close()
	})
	return f
}
func (f *fakeEngine) respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func (f *fakeEngine) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/v1.47"), "/v1.41")
	if path == "/images/create" {
		f.mu.Lock()
		blocked, bad := f.pullBlock, f.pullFailure
		f.mu.Unlock()
		f.pullOnce.Do(func() { close(f.pullEntered) })
		if blocked != nil {
			select {
			case <-blocked:
			case <-r.Context().Done():
				return
			}
		}
		if bad {
			f.respond(w, 200, map[string]string{"error": "image unavailable"})
			return
		}
		f.mu.Lock()
		f.haveImage = true
		f.mu.Unlock()
		f.respond(w, 200, map[string]string{"status": "pulled"})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case path == "/version":
		f.respond(w, 200, map[string]string{"ApiVersion": "1.48", "MinAPIVersion": "1.24", "Os": "linux"})
	case path == "/info":
		f.respond(w, 200, f.info)
	case strings.HasPrefix(path, "/images/"):
		if !f.haveImage {
			f.respond(w, 404, map[string]string{"message": "no image"})
			return
		}
		f.respond(w, 200, map[string]string{"Id": f.imageID})
	case path == "/networks/create":
		var in struct {
			Name   string
			Labels map[string]string
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			f.respond(w, 400, nil)
			return
		}
		f.networks[in.Name] = in.Labels
		f.respond(w, 201, map[string]string{"Id": in.Name})
	case strings.HasPrefix(path, "/networks/"):
		name := strings.TrimPrefix(path, "/networks/")
		labels, ok := f.networks[name]
		if !ok {
			f.respond(w, 404, map[string]string{"message": "no network"})
			return
		}
		if r.Method == http.MethodDelete {
			if f.failNetworkDelete {
				f.respond(w, 500, nil)
				return
			}
			delete(f.networks, name)
			w.WriteHeader(204)
			return
		}
		f.respond(w, 200, map[string]any{"Labels": labels})
	case path == "/containers/create":
		name := r.URL.Query().Get("name")
		if f.containers[name] != nil {
			f.respond(w, 409, nil)
			return
		}
		var config containerConfig
		if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
			f.respond(w, 400, nil)
			return
		}
		c := &fakeContainer{name: name, config: config}
		c.info.ID = name
		c.info.Config.Labels = config.Labels
		c.info.Config.Image = config.Image
		c.info.Config.User = config.User
		c.info.HostConfig = config.HostConfig
		if f.loseLimits {
			c.info.HostConfig.Memory = 0
		}
		f.containers[name] = c
		f.respond(w, 201, map[string]any{"Id": name, "Warnings": f.warnings})
	case path == "/containers/json":
		var out []listedContainer
		for name, c := range f.containers {
			state := "exited"
			if c.info.State.Running {
				state = "running"
			}
			out = append(out, listedContainer{ID: name, Labels: c.config.Labels, State: state})
		}
		f.respond(w, 200, out)
	case strings.HasPrefix(path, "/containers/"):
		parts := strings.Split(strings.TrimPrefix(path, "/containers/"), "/")
		name := parts[0]
		c := f.containers[name]
		if c == nil {
			f.respond(w, 404, map[string]string{"message": "no container"})
			return
		}
		if len(parts) == 1 && r.Method == http.MethodDelete {
			if c.info.State.Running {
				f.respond(w, 409, nil)
				return
			}
			delete(f.containers, name)
			w.WriteHeader(204)
			return
		}
		if len(parts) != 2 {
			f.respond(w, 400, nil)
			return
		}
		switch parts[1] {
		case "json":
			if f.inspectFailures > 0 {
				f.inspectFailures--
				f.respond(w, 500, nil)
				return
			}
			f.respond(w, 200, c.info)
		case "start":
			if !c.info.State.Running {
				var run string
				for _, m := range c.config.HostConfig.Mounts {
					if m.Target == "/run/tensora" {
						run = m.Source
					}
				}
				listener, err := net.Listen("unix", filepath.Join(run, "server.sock"))
				if err != nil {
					f.respond(w, 500, map[string]string{"message": err.Error()})
					return
				}
				c.listener = listener
				c.server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, `{"ready":true}`) }), ReadHeaderTimeout: time.Second}
				go func() { _ = c.server.Serve(listener) }()
				c.info.State.Running = true
			}
			if f.lostStartReply {
				f.lostStartReply = false
				f.respond(w, 500, map[string]string{"message": "reply lost after successful start"})
				return
			}
			w.WriteHeader(204)
		case "stop":
			if f.failStop {
				f.respond(w, 500, map[string]string{"message": "stop unavailable"})
				return
			}
			if c.server != nil {
				_ = c.server.Close()
			}
			c.info.State.Running = false
			w.WriteHeader(204)
		default:
			f.respond(w, 404, nil)
		}
	default:
		f.respond(w, 404, map[string]string{"message": "unknown route"})
	}
}

func newTestDriver(t *testing.T, f *fakeEngine, devices ...Device) *Driver {
	t.Helper()
	runs, err := os.MkdirTemp("/tmp", "td")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runs) })
	e := &engine{http: &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", f.server.Listener.Addr().String())
	}}}}
	if _, err := e.detect(context.Background()); err != nil {
		t.Fatal(err)
	}
	uid, gid := currentUser()
	d, err := newDriver(Options{Root: filepath.Join(t.TempDir(), "docker-state"), Runs: runs}, e, devices, uid, gid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d
}

func desired() protocol.Desired {
	return protocol.Desired{ID: uuid.New(), Attempt: 1, Mode: protocol.ModeRun, Driver: domain.DriverDocker,
		Kind: domain.KindNotebook, Accelerator: domain.AcceleratorCPU, VCPUs: 2, MemoryMiB: 1024, Runtime: protocol.Runtime{Ref: "example.test/notebook:1.0"}}
}

func awaitStatus(t *testing.T, d *Driver, id uuid.UUID, want func(protocol.Status) bool) protocol.Status {
	t.Helper()
	var last protocol.Status
	for limit := time.Now().Add(5 * time.Second); time.Now().Before(limit); time.Sleep(10 * time.Millisecond) {
		all, err := d.Statuses(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, st := range all {
			if st.ID == id {
				last = st
				if want(st) {
					return st
				}
			}
		}
	}
	t.Fatalf("Docker workload never reached expected status; last state=%s code=%s", last.State, last.Code)
	return last
}
func isReady(st protocol.Status) bool   { return st.State == protocol.StateReady }
func isStopped(st protocol.Status) bool { return st.State == protocol.StateStopped }

func TestNotebookLifecycleKeepsDataAndAdoptsRunningContainer(t *testing.T) {
	f := newFakeEngine(t)
	d := newTestDriver(t, f)
	ctx := context.Background()
	w := desired()
	if err := d.Run(ctx, w); err != nil {
		t.Fatal(err)
	}
	ready := awaitStatus(t, d, w.ID, isReady)
	if ready.Access == nil || len(ready.Access.Token) != 64 {
		t.Fatal("ready workload has no private socket credential")
	}
	if err := d.Run(ctx, w); err != nil {
		t.Fatal(err)
	}
	if again := awaitStatus(t, d, w.ID, isReady); again.Access.Token != ready.Access.Token {
		t.Error("same attempt restarted")
	}
	file := filepath.Join(d.dir(w.ID), "data", "notes.txt")
	if err := os.WriteFile(file, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	d.Close()
	again, err := newDriver(d.opts, d.engine, nil, d.uid, d.gid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(again.Close)
	if found := awaitStatus(t, again, w.ID, isReady); found.Access.Token != ready.Access.Token {
		t.Error("restart changed access credential")
	}
	if err := again.Stop(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	awaitStatus(t, again, w.ID, isStopped)
	w.Attempt++
	if err := again.Run(ctx, w); err != nil {
		t.Fatal(err)
	}
	next := awaitStatus(t, again, w.ID, isReady)
	if next.Access.Token == ready.Access.Token {
		t.Error("new attempt kept old access session alive")
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "keep" {
		t.Error("notebook data did not survive restart")
	}
	if err := again.Remove(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		all, err := again.Statuses(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(again.dir(w.ID)); !os.IsNotExist(err) {
		t.Errorf("removal left its data: %v", err)
	}
}

func TestImagePullIsAsynchronousAndStopDoesNotStartItLater(t *testing.T) {
	f := newFakeEngine(t)
	blocked := make(chan struct{})
	f.haveImage = false
	f.pullBlock = blocked
	d := newTestDriver(t, f)
	w := desired()
	if err := d.Run(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.pullEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("image pull never started")
	}
	awaitStatus(t, d, w.ID, func(st protocol.Status) bool {
		return st.State == protocol.StateProgressing && st.Code == protocol.CodePullingImage
	})
	if err := d.Stop(context.Background(), w.ID); err != nil {
		t.Fatal(err)
	}
	awaitStatus(t, d, w.ID, isStopped)
	close(blocked)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.containers) != 0 {
		t.Error("a stopped preparation created a container")
	}
}

func TestLostStartReplyAdoptsTheLiveContainer(t *testing.T) {
	f := newFakeEngine(t)
	f.lostStartReply = true
	d := newTestDriver(t, f)
	w := desired()
	if err := d.Run(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	awaitStatus(t, d, w.ID, isReady)
}

func TestMissingNotebookDiskIsNeverReplaced(t *testing.T) {
	f := newFakeEngine(t)
	d := newTestDriver(t, f)
	w := desired()
	ctx := context.Background()
	if err := d.Run(ctx, w); err != nil {
		t.Fatal(err)
	}
	awaitStatus(t, d, w.ID, isReady)
	if err := d.Stop(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	awaitStatus(t, d, w.ID, isStopped)
	if err := os.RemoveAll(filepath.Join(d.dir(w.ID), "data")); err != nil {
		t.Fatal(err)
	}
	w.Attempt++
	if err := d.Run(ctx, w); err == nil {
		t.Fatal("missing disk was replaced")
	}
	if _, err := os.Stat(filepath.Join(d.dir(w.ID), "data")); !os.IsNotExist(err) {
		t.Fatal("created an empty replacement disk")
	}
}

func TestFailedStopKeepsCapacityUntilDockerConfirmsExit(t *testing.T) {
	f := newFakeEngine(t)
	d := newTestDriver(t, f)
	w := desired()
	ctx := context.Background()
	if err := d.Run(ctx, w); err != nil {
		t.Fatal(err)
	}
	awaitStatus(t, d, w.ID, isReady)
	f.mu.Lock()
	f.failStop = true
	f.mu.Unlock()
	if err := d.Stop(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	awaitStatus(t, d, w.ID, func(st protocol.Status) bool { return st.State == protocol.StateProgressing })
	f.mu.Lock()
	f.failStop = false
	f.mu.Unlock()
	awaitStatus(t, d, w.ID, isStopped)
}

func TestOrphanContainersAreVisibleAndParkedWithoutDeletingTheirData(t *testing.T) {
	f := newFakeEngine(t)
	d := newTestDriver(t, f)
	w := desired()
	ctx := context.Background()
	if err := d.Run(ctx, w); err != nil {
		t.Fatal(err)
	}
	awaitStatus(t, d, w.ID, isReady)
	keep := filepath.Join(d.dir(w.ID), "data", "notes.txt")
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(d.dir(w.ID), "state.json")); err != nil {
		t.Fatal(err)
	}
	awaitStatus(t, d, w.ID, func(st protocol.Status) bool { return st.State == protocol.StateProgressing })
	if err := d.Stop(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(keep); err != nil || string(data) != "keep" {
		t.Fatal("parking an unknown workload deleted its data")
	}
	all, err := d.Statuses(ctx)
	if err != nil || len(all) != 1 || all[0].State != protocol.StateStopped {
		t.Errorf("parked orphan must stay visible for later explicit removal: %v", err)
	}
}

func TestAgentCanRemoveAnOrphanAfterParkingIt(t *testing.T) {
	f := newFakeEngine(t)
	d := newTestDriver(t, f)
	w := desired()
	ctx := context.Background()
	if err := d.Run(ctx, w); err != nil {
		t.Fatal(err)
	}
	awaitStatus(t, d, w.ID, isReady)
	st, _, _ := d.read(w.ID)
	if err := os.Remove(filepath.Join(d.dir(w.ID), "state.json")); err != nil {
		t.Fatal(err)
	}
	a := agent.New(nil, []agent.Driver{d}, nil, "test", slog.New(slog.DiscardHandler))
	if !a.Apply(ctx, nil) {
		t.Fatal("could not park an unlisted workload")
	}
	awaitStatus(t, d, w.ID, isStopped)
	w.Mode = protocol.ModeRemove
	if !a.Apply(ctx, []protocol.Desired{w}) {
		t.Fatal("could not remove parked orphan")
	}
	awaitRemoved(t, d, w.ID)
	all, err := d.Statuses(ctx)
	if err != nil || len(all) != 0 {
		t.Errorf("removed orphan still reported: %v", err)
	}
	for _, path := range []string{d.dir(w.ID), d.run(st), filepath.Join(d.runs, st.Run+".owner.json")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("orphan removal retained %s", path)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.containers) != 0 || len(f.networks) != 0 {
		t.Error("orphan removal retained a container or network")
	}
}

func TestMissingResourceControllersAndUIDRemappingAreUnavailable(t *testing.T) {
	for _, key := range []string{"MemoryLimit", "SwapLimit", "PidsLimit", "CpuCfsQuota", "rootless", "userns"} {
		t.Run(key, func(t *testing.T) {
			f := newFakeEngine(t)
			if key == "rootless" || key == "userns" {
				f.info["SecurityOptions"] = []string{"name=" + key}
			} else {
				f.info[key] = false
			}
			e := &engine{http: &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", f.server.Listener.Addr().String())
			}}}}
			defer e.close()
			if _, err := e.detect(context.Background()); err == nil {
				t.Error("advertised a daemon that cannot enforce resource or mount ownership requirements")
			}
		})
	}
}

func TestCreateWarningsPreventContainerStart(t *testing.T) {
	f := newFakeEngine(t)
	f.warnings = []string{"memory limit was discarded"}
	d := newTestDriver(t, f)
	w := desired()
	if err := d.Run(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	awaitStatus(t, d, w.ID, func(st protocol.Status) bool {
		return st.State == protocol.StateFailed && st.Code == protocol.CodeDriverProblem
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, container := range f.containers {
		if container.info.State.Running {
			t.Error("started a container whose resource settings were not applied")
		}
	}
}

func TestForeignContainerIsNeverStoppedOrRemoved(t *testing.T) {
	f := newFakeEngine(t)
	d := newTestDriver(t, f)
	w := desired()
	ctx := context.Background()
	if err := d.Run(ctx, w); err != nil {
		t.Fatal(err)
	}
	awaitStatus(t, d, w.ID, isReady)
	st, _, _ := d.read(w.ID)
	f.mu.Lock()
	f.containers[d.name(st)].info.Config.Labels[specLabel] = "different-spec"
	f.mu.Unlock()
	if err := d.stopContainer(ctx, st); err == nil {
		t.Fatal("stopped a container with different ownership")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.containers[d.name(st)].info.State.Running {
		t.Fatal("foreign container was changed")
	}
}

func TestPreparationRecoversAfterAgentRestart(t *testing.T) {
	f := newFakeEngine(t)
	blocked := make(chan struct{})
	f.haveImage = false
	f.pullBlock = blocked
	d := newTestDriver(t, f)
	w := desired()
	if err := d.Run(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.pullEntered:
	case <-time.After(time.Second):
		t.Fatal("pull did not start")
	}
	d.Close()
	close(blocked)
	again, err := newDriver(d.opts, d.engine, nil, d.uid, d.gid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(again.Close)
	awaitStatus(t, again, w.ID, isReady)
}

func TestDistinctWorkloadsReserveDistinctPhysicalGPUs(t *testing.T) {
	f := newFakeEngine(t)
	devices := []Device{{ID: "GPU-a", Model: "NVIDIA L40S", MemoryMiB: 48 * 1024}, {ID: "GPU-b", Model: "NVIDIA L40S", MemoryMiB: 48 * 1024}}
	d := newTestDriver(t, f, devices...)
	w1, w2 := desired(), desired()
	for _, w := range []*protocol.Desired{&w1, &w2} {
		w.Accelerator = domain.AcceleratorNVIDIA
		w.Devices = 1
		w.DeviceModels = []string{"NVIDIA L40S"}
	}
	var wg sync.WaitGroup
	for _, w := range []protocol.Desired{w1, w2} {
		wg.Go(func() {
			if err := d.Run(context.Background(), w); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	awaitStatus(t, d, w1.ID, isReady)
	awaitStatus(t, d, w2.ID, isReady)
	a, _, _ := d.read(w1.ID)
	b, _, _ := d.read(w2.ID)
	if len(a.Devices) != 1 || len(b.Devices) != 1 || a.Devices[0] == b.Devices[0] {
		t.Fatal("workloads share a physical GPU")
	}
}

func awaitRemoved(t *testing.T, d *Driver, id uuid.UUID) {
	t.Helper()
	for until := time.Now().Add(5 * time.Second); time.Now().Before(until); time.Sleep(10 * time.Millisecond) {
		all, err := d.Statuses(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, s := range all {
			if s.ID == id {
				found = true
			}
		}
		if !found {
			return
		}
	}
	t.Fatal("workload removal never completed")
}

func TestUnsafeCreateCannotStartAfterTransientInspectFailure(t *testing.T) {
	for _, warning := range []bool{true, false} {
		t.Run(fmt.Sprint("warning=", warning), func(t *testing.T) {
			f := newFakeEngine(t)
			if warning {
				f.warnings = []string{"memory limit discarded"}
				f.inspectFailures = 1
			} else {
				f.loseLimits = true
			}
			d := newTestDriver(t, f)
			w := desired()
			if err := d.Run(context.Background(), w); err != nil {
				t.Fatal(err)
			}
			awaitStatus(t, d, w.ID, func(s protocol.Status) bool {
				return s.State == protocol.StateFailed && s.Code == protocol.CodeDriverProblem
			})
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, c := range f.containers {
				if c.info.State.Running || c.server != nil {
					t.Fatal("unsafe container was started during recovery")
				}
			}
		})
	}
}

func TestOrphanRemovalRetriesAfterLastContainerIsGone(t *testing.T) {
	f := newFakeEngine(t)
	d := newTestDriver(t, f, Device{ID: "GPU-a", Model: "NVIDIA L40S", MemoryMiB: 46068})
	w := desired()
	ctx := context.Background()
	if err := d.Run(ctx, w); err != nil {
		t.Fatal(err)
	}
	awaitStatus(t, d, w.ID, isReady)
	if err := os.Remove(filepath.Join(d.dir(w.ID), "state.json")); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.failNetworkDelete = true
	f.mu.Unlock()
	if err := d.Remove(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	for until := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := d.Statuses(ctx); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		gone := len(f.containers) == 0
		f.mu.Unlock()
		if gone {
			break
		}
		if time.Now().After(until) {
			t.Fatal("orphan container never removed")
		}
	}
	st, known, err := d.read(w.ID)
	if err != nil || !known || st.Phase != removing {
		t.Fatalf("cleanup lost its durable tombstone: %+v %v", st, err)
	}
	next := desired()
	next.Accelerator = domain.AcceleratorNVIDIA
	next.Devices = 1
	next.DeviceModels = []string{"NVIDIA L40S"}
	if err := d.Run(ctx, next); err == nil {
		t.Fatal("unknown allocation released GPU capacity before cleanup")
	}
	// Restart the driver: the container list is empty, so only the durable record can retry.
	d.Close()
	f.mu.Lock()
	f.failNetworkDelete = false
	f.mu.Unlock()
	again, err := newDriver(d.opts, d.engine, d.devices, d.uid, d.gid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(again.Close)
	awaitRemoved(t, again, w.ID)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.networks) != 0 {
		t.Fatal("retry left the orphan network")
	}
}
