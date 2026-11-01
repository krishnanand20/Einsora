//go:build darwin

package macosnative

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	os.Exit(m.Run())
}

// fakeJupyter answers /api on the port it's given, and reports what the sandbox let it do.
func fakeJupyter(args []string) {
	port := ""
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--ServerApp.port="); ok {
			port = v
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
	l, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		os.Exit(3)
	}
	_ = http.Serve(l, mux)
}

// newDriver is a driver whose environment for runtimeRef is already built: its jupyter-lab runs
// this test binary.
func newDriver(t *testing.T, private ...string) (*Driver, string) {
	t.Helper()
	state := t.TempDir()
	d, err := New(Options{Root: filepath.Join(state, "native"), State: state, Private: private, UV: "/usr/bin/false",
		Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
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
	if s.Attempt != 1 || s.Access == nil || s.Access.Port == 0 || len(s.Access.Token) != 64 {
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
	again, err := New(Options{Root: filepath.Join(state, "native"), State: state, UV: "/usr/bin/false", Log: slog.New(slog.DiscardHandler)})
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
	probe := func(op, path string) string {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/probe?%s=%s", s.Access.Port, op, url.QueryEscape(path))) //nolint:gosec // the test's own server
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var b strings.Builder
		_, _ = fmt.Fprint(&b, resp.StatusCode)
		buf := make([]byte, 16)
		n, _ := resp.Body.Read(buf)
		return string(buf[:n])
	}
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
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/spawn", s.Access.Port)) //nolint:gosec // the test's own server
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var pid int
	if _, err := fmt.Fscan(resp.Body, &pid); err != nil {
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
