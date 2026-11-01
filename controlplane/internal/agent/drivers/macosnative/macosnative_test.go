//go:build darwin

package macosnative

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tensora/tensora/controlplane/internal/agent/protocol"
	"github.com/tensora/tensora/controlplane/internal/domain"
)

const runtimeRef = "jupyterlab-pytorch-mps"

// TestMain lets this test binary stand in for jupyter-lab, so the driver's process supervision,
// sandbox, and health checks are tested without downloading JupyterLab.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "fake-jupyter" {
		fakeJupyter(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "fake-kernel" {
		time.Sleep(2 * time.Minute)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "fake-endpoint" {
		fakeEndpoint(os.Args[2:])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "fake-env-lock" {
		_, _ = fmt.Fprintln(os.Stdout, "ready")
		time.Sleep(2 * time.Minute)
		return
	}
	os.Exit(m.Run())
}

// fakeJupyter answers /api on the Unix socket it's given, and reports what the sandbox let it do.
func fakeJupyter(args []string) {
	sock := ""
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--ServerApp.sock="); ok {
			sock = v
		}
	}
	if os.Getenv("JUPYTER_TOKEN") == "" {
		os.Exit(2)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, `{"version":"fake"}`) })
	mux.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		var err error
		q := r.URL.Query()
		switch {
		case q.Has("write"):
			err = os.WriteFile(q.Get("write"), []byte("x"), 0o600)
		case q.Has("signal"):
			pid, _ := strconv.Atoi(q.Get("signal"))
			err = syscall.Kill(pid, 0)
		case q.Has("listen-tcp"):
			var l net.Listener
			if l, err = net.Listen("tcp", "127.0.0.1:0"); err == nil {
				_ = l.Close()
			}
		case q.Has("listen-implicit"):
			family := syscall.AF_INET
			if q.Get("listen-implicit") == "ipv6" {
				family = syscall.AF_INET6
			}
			var fd int
			if fd, err = syscall.Socket(family, syscall.SOCK_STREAM, 0); err == nil {
				err = syscall.Listen(fd, 1) // no bind(): macOS chooses a port implicitly
				_ = syscall.Close(fd)
			}
		case q.Has("listen-udp"):
			var c net.PacketConn
			if c, err = net.ListenPacket("udp", "127.0.0.1:0"); err == nil {
				_ = c.Close()
			}
		case q.Has("listen-unix"):
			var l net.Listener
			if l, err = net.Listen("unix", q.Get("listen-unix")); err == nil {
				_ = l.Close()
			}
		case q.Has("connect"):
			var c net.Conn
			if c, err = net.Dial("unix", q.Get("connect")); err == nil {
				_ = c.Close()
			}
		default:
			_, err = os.ReadFile(q.Get("read"))
		}
		_, _ = fmt.Fprint(w, err == nil)
	})
	// /spawn starts a kernel in a session of its own, as Jupyter does, and answers its process ID.
	mux.HandleFunc("/spawn", func(w http.ResponseWriter, _ *http.Request) {
		k := exec.Command(os.Args[0], "fake-kernel")
		k.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := k.Start(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		go func() { _ = k.Wait() }()
		_, _ = fmt.Fprint(w, k.Process.Pid)
	})
	l, err := net.Listen("unix", sock)
	if err != nil {
		os.Exit(3)
	}
	_ = http.Serve(l, mux)
}

// get asks the workload's server on socket for path, and returns the body.
func get(t *testing.T, socket, path string) string {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	resp, err := client.Get("http://localhost" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// newDriver is a driver whose environment for runtimeRef is already built: its jupyter-lab runs
// this test binary.
func newDriver(t *testing.T, private ...string) (*Driver, string) {
	t.Helper()
	state := t.TempDir()
	// Run directories hold sockets, whose paths macOS limits to 104 bytes: t.TempDir's are too long.
	runs, err := os.MkdirTemp("/tmp", "tr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runs) })
	d, err := New(Options{Root: filepath.Join(state, "native"), State: state, Private: private, Runs: runs, UV: "/usr/bin/false",
		Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	lock, _ := envs.ReadFile("envs/" + runtimeRef + ".txt")
	_, env := d.envDir(runtimeRef, lock)
	self, _ := os.Executable()
	script := fmt.Sprintf("#!/bin/sh\nexec %q fake-jupyter \"$@\"\n", self)
	if err := os.MkdirAll(filepath.Join(env, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env, "bin", "jupyter-lab"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env, ".tensora-ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return d, state
}

func notebook(attempt int64) protocol.Desired {
	return protocol.Desired{ID: uuid.New(), Mode: protocol.ModeRun, Attempt: attempt, Kind: domain.KindNotebook,
		Driver: domain.DriverMacOSNative, Accelerator: domain.AcceleratorApple, Devices: 1,
		Runtime: protocol.Runtime{Ref: runtimeRef}}
}

// status waits until the workload's status satisfies ok.
func status(t *testing.T, d *Driver, id uuid.UUID, ok func(protocol.Status) bool) protocol.Status {
	t.Helper()
	var last protocol.Status
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		sts, err := d.Statuses(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range sts {
			if s.ID == id {
				last = s
				if ok(s) {
					return s
				}
			}
		}
	}
	t.Fatalf("workload %s never got there; last status %+v", id, last)
	return last
}

func ready(s protocol.Status) bool { return s.State == protocol.StateReady }

func TestNotebookLifecycle(t *testing.T) {
	d, _ := newDriver(t)
	ctx := context.Background()
	w := notebook(1)
	if err := d.Run(ctx, w); err != nil {
		t.Fatal(err)
	}
	s := status(t, d, w.ID, ready)
	if s.Attempt != 1 || s.Access == nil || !strings.HasSuffix(s.Access.Socket, "/jupyter.sock") || len(s.Access.Token) != 64 {
		t.Fatalf("ready status = %+v", s)
	}
	if err := d.Run(ctx, w); err != nil { // the same attempt again: nothing changes
		t.Fatal(err)
	}
	if again := status(t, d, w.ID, ready); again.Access.Token != s.Access.Token {
		t.Error("running the same attempt again restarted it")
	}

	// Data survives a stop and a new attempt.
	keep := filepath.Join(d.dir(w.ID), "data", "notes.txt")
	if err := os.WriteFile(keep, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.Stop(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	status(t, d, w.ID, func(s protocol.Status) bool { return s.State == protocol.StateStopped })
	w.Attempt = 2
	if err := d.Run(ctx, w); err != nil {
		t.Fatal(err)
	}
	if s := status(t, d, w.ID, ready); s.Attempt != 2 {
		t.Errorf("attempt %d; want 2", s.Attempt)
	}
	if b, _ := os.ReadFile(keep); string(b) != "mine" {
		t.Error("the data didn't survive a stop")
	}

	if err := d.Remove(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d.dir(w.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Error("removing it left its directory")
	}
	if sts, _ := d.Statuses(ctx); len(sts) != 0 {
		t.Errorf("statuses after removal: %+v", sts)
	}
}

// After the agent restarts, a new driver finds the workloads still running, and a crash shows as a
// failure, never as a fresh start.
func TestRestartAndCrash(t *testing.T) {
	d, state := newDriver(t)
	ctx := context.Background()
	w := notebook(1)
	if err := d.Run(ctx, w); err != nil {
		t.Fatal(err)
	}
	s := status(t, d, w.ID, ready)
	again, err := New(Options{Root: filepath.Join(state, "native"), State: state, Runs: d.opts.Runs, UV: "/usr/bin/false",
		Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if found := status(t, again, w.ID, ready); found.Access.Token != s.Access.Token {
		t.Error("after a restart, a different workload was found")
	}
	st, _, _ := again.readState(w.ID)
	_ = syscall.Kill(-st.Proc.PID, syscall.SIGKILL)
	if crashed := status(t, again, w.ID, func(s protocol.Status) bool { return s.State == protocol.StateFailed }); crashed.Code != protocol.CodeExited {
		t.Errorf("after a crash: %+v; want failed, exited", crashed)
	}
	_ = again.Remove(ctx, w.ID)
}

// An upgrade must retire servers started under the old TCP-enabled sandbox before capacity is
// released. Their notebook data stays available for an explicit new attempt.
func TestLegacyServerIsStoppedAfterUpgrade(t *testing.T) {
	for _, tt := range []struct {
		name string
		tcp  bool
	}{{"legacy TCP state", true}, {"older Unix socket sandbox", false}} {
		t.Run(tt.name, func(t *testing.T) { checkSandboxUpgrade(t, tt.tcp) })
	}
}

func checkSandboxUpgrade(t *testing.T, legacyTCP bool) {
	t.Helper()
	d, _ := newDriver(t)
	ctx := context.Background()
	w := notebook(1)
	if err := d.Run(ctx, w); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Remove(ctx, w.ID) })
	kernel := spawnKernel(t, d, w.ID)
	st, _, err := d.readState(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	server := st.Proc
	keep := filepath.Join(d.dir(w.ID), "data", "notes.txt")
	if err := os.WriteFile(keep, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	st.Sandbox = "" // older versions never recorded which sandbox the server used
	if legacyTCP {
		st.Run = "" // the old TCP state format had a port instead of a run directory
	}
	if err := d.writeState(w.ID, st); err != nil {
		t.Fatal(err)
	}
	failed := status(t, d, w.ID, func(s protocol.Status) bool { return s.State == protocol.StateFailed })
	if failed.Code != protocol.CodeSandboxChanged || failed.Access != nil {
		t.Errorf("legacy status = %+v; want a sandbox change without access", failed)
	}
	if !gone(server.PID) || !gone(kernel) {
		t.Error("an old server or kernel survived the upgrade")
	}
	w.Attempt++
	if err := d.Run(ctx, w); err != nil {
		t.Fatal(err)
	}
	status(t, d, w.ID, ready)
	if data, err := os.ReadFile(keep); err != nil || string(data) != "mine" {
		t.Errorf("notebook data after upgrade = %q, %v", data, err)
	}
}

func TestRunDirectoriesAreReservedAndPrivate(t *testing.T) {
	d, _ := newDriver(t)
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			name, err := d.newRunName()
			if err != nil {
				t.Error(err)
				return
			}
			run := d.runDir(state{Run: name})
			info, err := os.Stat(run)
			if err != nil {
				t.Errorf("run directory wasn't reserved before the environment build: %v", err)
				return
			}
			if info.Mode().Perm() != 0o700 {
				t.Errorf("run directory permissions = %o; want 700", info.Mode().Perm())
			}
			if err := os.WriteFile(filepath.Join(run, "stale.sock"), nil, 0o600); err != nil {
				t.Error(err)
				return
			}
			if err := d.prepareRun(state{Run: name}); err != nil {
				t.Error(err)
				return
			}
			after, err := os.Stat(run)
			if err != nil || !os.SameFile(info, after) {
				t.Errorf("preparing the run replaced its reservation: %v", err)
			}
			if _, err := os.Stat(filepath.Join(run, "stale.sock")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("preparing the run left a stale socket: %v", err)
			}
		})
	}
	wg.Wait()
	if entries, err := os.ReadDir(d.runs); err != nil || len(entries) != 32 {
		t.Errorf("reserved directories = %d, %v; want 32", len(entries), err)
	}
}

func TestUnsafeRunDirectoriesAreRefused(t *testing.T) {
	d, _ := newDriver(t)
	shared := filepath.Join(d.runs, "shared")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o755); err != nil { //nolint:gosec // deliberately unsafe test directory
		t.Fatal(err)
	}
	link := filepath.Join(d.runs, "symlink")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"relative", shared, link} {
		if _, err := privateDir(dir); err == nil {
			t.Errorf("accepted unsafe run directory %q", dir)
		}
	}
	if err := d.prepareRun(state{Run: "symlink"}); err == nil {
		t.Error("followed a run directory symlink while preparing a workload")
	}
	for _, run := range []string{"..", "../elsewhere", "123456789", "1234567g"} {
		id := uuid.New()
		if err := os.MkdirAll(d.dir(id), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := d.writeState(id, state{Run: run}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := d.readState(id); err == nil {
			t.Errorf("accepted run directory %q from saved state", run)
		}
	}
}

// A workload that ran here before and whose data is gone gets no fresh, empty disk.
func TestNoFreshDiskForAWorkloadThatRanBefore(t *testing.T) {
	d, _ := newDriver(t)
	ctx := context.Background()
	w := notebook(1)
	if err := d.Run(ctx, w); err != nil {
		t.Fatal(err)
	}
	status(t, d, w.ID, ready)
	_ = d.Stop(ctx, w.ID)
	status(t, d, w.ID, func(s protocol.Status) bool { return s.State == protocol.StateStopped })
	if err := os.RemoveAll(filepath.Join(d.dir(w.ID), "data")); err != nil {
		t.Fatal(err)
	}
	w.Attempt = 2
	var coded interface{ Unwrap() error }
	if err := d.Run(ctx, w); err == nil || !errors.As(err, &coded) || !strings.Contains(err.Error(), "missing") {
		t.Errorf("Run = %v; want it refused: its disk is missing", err)
	}
}

// The workload runs under the sandbox: it can't read the agent's state (the database) or the
// owner's credentials, but can read its own directory.
func TestSandbox(t *testing.T) {
	// A private file outside the state directory, named through a symlink: the sandbox must deny the
	// file it resolves to.
	elsewhere := t.TempDir()
	if err := os.Mkdir(filepath.Join(elsewhere, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, "real"), filepath.Join(elsewhere, "link")); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(elsewhere, "real", "tensora.log")
	if err := os.WriteFile(private, []byte("log"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, state := newDriver(t, filepath.Join(elsewhere, "link", "tensora.log"))
	ctx := context.Background()
	db := filepath.Join(state, "tensora.db")
	if err := os.WriteFile(db, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := notebook(1)
	if err := d.Run(ctx, w); err != nil {
		t.Fatal(err)
	}
	s := status(t, d, w.ID, ready)
	defer func() { _ = d.Remove(ctx, w.ID) }()
	probe := func(op, path string) string { return get(t, s.Access.Socket, "/probe?"+op+"="+url.QueryEscape(path)) }
	// Another workload's run directory, with its server listening.
	elsewhereRun := filepath.Join(d.runs, "0ther000")
	if err := os.Mkdir(elsewhereRun, 0o700); err != nil {
		t.Fatal(err)
	}
	theirs, err := net.Listen("unix", filepath.Join(elsewhereRun, "jupyter.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = theirs.Close() }()
	st, _, _ := d.readState(w.ID)
	lock, _ := envs.ReadFile("envs/" + runtimeRef + ".txt")
	_, env := d.envDir(runtimeRef, lock)
	other := filepath.Join(d.opts.Root, "workloads", uuid.NewString())
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "state.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The state directory is inside a temporary directory here (t.TempDir), which workloads may
	// write to: the sandbox must deny it anyway.
	for _, tt := range []struct{ op, path, want, what string }{
		{"read", db, "false", "read the agent's database"},
		{"write", db, "false", "wrote the agent's database"},
		{"write", filepath.Join(state, "planted"), "false", "wrote to the agent's state directory"},
		{"read", filepath.Join(other, "state.json"), "false", "read another workload's state"},
		{"write", filepath.Join(other, "state.json"), "false", "wrote another workload's state"},
		{"write", filepath.Join(env, "bin", "planted"), "false", "wrote to the environment other workloads share"},
		{"read", filepath.Join(env, ".tensora-ready"), "true", "couldn't read its environment"},
		{"read", private, "false", "read a private file outside the state directory, named through a symlink"},
		// Its state file and logs are written by the agent, outside the sandbox: nothing may be
		// planted there, such as a symlink to the database.
		{"write", filepath.Join(d.dir(w.ID), "state.json"), "false", "changed its own state file"},
		{"write", filepath.Join(d.dir(w.ID), "state.json.tmp"), "false", "planted a file where the agent writes its state"},
		{"write", filepath.Join(d.dir(w.ID), "logs", "stderr.log"), "false", "wrote its log file directly"},
		{"signal", strconv.Itoa(os.Getpid()), "false", "signalled a process outside its sandbox"},
		{"write", filepath.Join(d.dir(w.ID), "home", "notes.txt"), "true", "couldn't write its home directory"},
		// It listens on no TCP port, which a page in the browser could be sent to, and on Unix
		// sockets only in its own run directory; it can't reach another workload's server.
		{"listen-tcp", "", "false", "listened on a TCP port"},
		{"listen-implicit", "ipv4", "false", "listened on an implicitly bound IPv4 port"},
		{"listen-implicit", "ipv6", "false", "listened on an implicitly bound IPv6 port"},
		{"listen-udp", "", "false", "listened on a UDP port"},
		{"listen-unix", filepath.Join(d.runDir(st), "mine.sock"), "true", "couldn't listen in its run directory"},
		{"listen-unix", filepath.Join(elsewhereRun, "planted.sock"), "false", "listened in another workload's run directory"},
		{"connect", filepath.Join(elsewhereRun, "jupyter.sock"), "false", "connected to another workload's server"},
		{"write", filepath.Join(d.runDir(st), "tmp", "scratch"), "true", "couldn't write its temporary directory"},
		{"read", filepath.Join(d.dir(w.ID), "state.json"), "true", "couldn't read its own directory"},
		{"write", filepath.Join(d.dir(w.ID), "data", "notes.txt"), "true", "couldn't write its own data"},
	} {
		if got := probe(tt.op, tt.path); got != tt.want {
			t.Errorf("a workload %s (%s %s: %s)", tt.what, tt.op, tt.path, got)
		}
	}
}

// A runtime the driver has no environment for, and an environment that can't be built, are image
// failures.
func TestEnvironmentFailures(t *testing.T) {
	d, _ := newDriver(t)
	ctx := context.Background()
	w := notebook(1)
	w.Runtime.Ref = "no-such-runtime"
	if err := d.Run(ctx, w); err == nil {
		t.Error("a runtime with no environment was run")
	}
	// The real environment isn't built in this test: uv is /usr/bin/false, so building fails.
	other := notebook(1)
	lock, _ := envs.ReadFile("envs/" + runtimeRef + ".txt")
	_, env := d.envDir(runtimeRef, lock)
	_ = os.Remove(filepath.Join(env, ".tensora-ready"))
	if err := d.Run(ctx, other); err != nil {
		t.Fatal(err)
	}
	if s := status(t, d, other.ID, func(s protocol.Status) bool { return s.State == protocol.StateFailed }); s.Code != protocol.CodeImage {
		t.Errorf("status = %+v; want an image failure", s)
	}
}

// Stop reaches kernels, which run in sessions of their own, and a server that dies takes its
// kernels with it.
func TestStopReachesKernels(t *testing.T) {
	d, _ := newDriver(t)
	ctx := context.Background()
	w := notebook(1)
	if err := d.Run(ctx, w); err != nil {
		t.Fatal(err)
	}
	kernel := spawnKernel(t, d, w.ID)
	if err := d.Stop(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	status(t, d, w.ID, func(s protocol.Status) bool { return s.State == protocol.StateStopped })
	if !gone(kernel) {
		t.Error("a kernel survived its workload's stop")
	}

	w.Attempt = 2
	if err := d.Run(ctx, w); err != nil {
		t.Fatal(err)
	}
	kernel = spawnKernel(t, d, w.ID)
	st, _, _ := d.readState(w.ID)
	_ = syscall.Kill(st.Proc.PID, syscall.SIGKILL)
	status(t, d, w.ID, func(s protocol.Status) bool { return s.State == protocol.StateFailed })
	if !gone(kernel) {
		t.Error("a kernel survived its server's crash")
	}
	_ = d.Remove(ctx, w.ID)
}

// spawnKernel has the workload start a kernel, and waits until the driver has seen it.
func spawnKernel(t *testing.T, d *Driver, id uuid.UUID) int {
	t.Helper()
	s := status(t, d, id, ready)
	pid, err := strconv.Atoi(get(t, s.Access.Socket, "/spawn"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	status(t, d, id, ready) // a status pass sees it
	return pid
}

// gone waits a little for pid to be gone.
func gone(pid int) bool {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if syscall.Kill(pid, 0) != nil {
			return true
		}
	}
	return false
}

func TestSandboxProfileQuotesPaths(t *testing.T) {
	profile, err := sandbox("/Users/o", []string{`/data/a "quoted" \ path/tensora.db`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(profile, `(subpath "/data/a \"quoted\" \\ path/tensora.db")`) ||
		!strings.Contains(profile, `(subpath "/Users/o/.ssh")`) {
		t.Errorf("profile:\n%s", profile)
	}
	for _, bad := range []string{"relative/tensora.db", "/data/new\nline"} {
		if _, err := sandbox("/Users/o", []string{bad}); err == nil {
			t.Errorf("%q was written into the profile", bad)
		}
	}
}

func TestNativeCPUNotebookLifecycleAndRestart(t *testing.T) {
	d, _ := newDriver(t)
	ctx := context.Background()
	w := notebook(1)
	w.Accelerator, w.Devices = domain.AcceleratorCPU, 0
	if err := d.Run(ctx, w); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Remove(ctx, w.ID) })
	status(t, d, w.ID, ready)
	file := filepath.Join(d.dir(w.ID), "data", "saved.txt")
	if err := os.WriteFile(file, []byte("saved CPU work"), 0o600); err != nil {
		t.Fatal(err)
	}
	d.Close()
	again, err := New(d.opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(again.Close)
	status(t, again, w.ID, ready)
	if err := again.Stop(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	status(t, again, w.ID, func(s protocol.Status) bool { return s.State == protocol.StateStopped })
	w.Attempt++
	if err := again.Run(ctx, w); err != nil {
		t.Fatal(err)
	}
	status(t, again, w.ID, ready)
	if saved, err := os.ReadFile(file); err != nil || string(saved) != "saved CPU work" {
		t.Fatal("CPU notebook lost data across stop/restart")
	}
}

func TestNativeCPUDoesNotEnableGuestsOrMLXOnCPU(t *testing.T) {
	d, _ := newDriver(t)
	for _, w := range []protocol.Desired{func() protocol.Desired {
		w := notebook(1)
		w.Accelerator, w.Devices, w.StrongIsolation = domain.AcceleratorCPU, 0, true
		return w
	}(), func() protocol.Desired {
		w := endpointWorkload()
		w.Accelerator, w.Devices = domain.AcceleratorCPU, 0
		return w
	}()} {
		if err := d.Run(context.Background(), w); err == nil {
			t.Fatal("native CPU advertised guest isolation or ran MLX on CPU")
		}
	}
}
