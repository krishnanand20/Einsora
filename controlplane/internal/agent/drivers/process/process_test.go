//go:build darwin || linux

package process

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain lets the test binary act as a server that starts a child in a session of its own, as
// Jupyter starts kernels (EINSORA_TEST_ROLE=server), and as that child (kernel).
func TestMain(m *testing.M) {
	switch os.Getenv("EINSORA_TEST_ROLE") {
	case "server":
		kernel := exec.Command(os.Args[0])
		kernel.Env = append(os.Environ(), "EINSORA_TEST_ROLE=kernel")
		kernel.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := kernel.Start(); err != nil {
			os.Exit(2)
		}
		_ = os.WriteFile(os.Getenv("EINSORA_TEST_PIDFILE"), []byte(strconv.Itoa(kernel.Process.Pid)), 0o600)
		// The server runs until the test tells it to exit (the exit file appears); its kernel goes on.
		for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if _, err := os.Stat(os.Getenv("EINSORA_TEST_EXITFILE")); err == nil {
				os.Exit(0)
			}
		}
		os.Exit(0)
	case "kernel":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// startServer starts a server whose kernel runs in a session of its own, and returns both, and
// the file whose creation makes the server exit.
func startServer(t *testing.T) (Proc, int, string) {
	t.Helper()
	dir := t.TempDir()
	pidFile, exitFile := filepath.Join(dir, "kernel.pid"), filepath.Join(dir, "exit")
	env := []string{"EINSORA_TEST_ROLE=server", "EINSORA_TEST_PIDFILE=" + pidFile, "EINSORA_TEST_EXITFILE=" + exitFile}
	p, err := Start(Spec{Path: os.Args[0], Env: env, Dir: dir,
		Stdout: filepath.Join(dir, "out.log"), Stderr: filepath.Join(dir, "err.log")})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if raw, err := os.ReadFile(pidFile); err == nil && len(raw) > 0 {
			kernel, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
			t.Cleanup(func() { _ = syscall.Kill(kernel, syscall.SIGKILL) })
			return p, kernel, exitFile
		}
	}
	t.Fatal("the server didn't start its kernel")
	return Proc{}, 0, ""
}

func running(pid int) bool {
	// A killed child of a process that's still alive is a zombie until reaped: it counts as gone.
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	_, err := startTime(pid)
	return err == nil && !zombie(pid)
}

func TestStartAliveStop(t *testing.T) {
	dir := t.TempDir()
	p, err := Start(Spec{Path: "/bin/sh", Args: []string{"-c", "echo started; exec sleep 60"}, Dir: dir,
		Stdout: filepath.Join(dir, "out.log"), Stderr: filepath.Join(dir, "err.log")})
	if err != nil {
		t.Fatal(err)
	}
	if !Alive(p) {
		t.Fatal("not alive after start")
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if out, _ := os.ReadFile(filepath.Join(dir, "out.log")); len(out) > 0 {
			break
		}
	}
	if Alive(Proc{PID: p.PID, Start: p.Start + 1}) {
		t.Error("another start time was taken for the same process: a reused ID would be mistaken for it")
	}
	Stop(p, 5*time.Second)
	if Alive(p) {
		t.Error("still alive after Stop")
	}
	out, _ := os.ReadFile(filepath.Join(dir, "out.log"))
	if string(out) != "started\n" {
		t.Errorf("stdout log = %q", out)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "out.log")); fi.Mode().Perm() != 0o600 {
		t.Errorf("log mode %v; want 0600", fi.Mode().Perm())
	}
}

// A process that ignores SIGTERM is killed after the grace period, with its whole group.
func TestStopKillsStubbornGroups(t *testing.T) {
	dir := t.TempDir()
	p, err := Start(Spec{Path: "/bin/sh", Args: []string{"-c", "trap '' TERM; sleep 60 & wait"}, Dir: dir,
		Stdout: filepath.Join(dir, "out.log"), Stderr: filepath.Join(dir, "err.log")})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	Stop(p, time.Second) // a short grace, for the test
	if Alive(p) {
		t.Error("a process ignoring SIGTERM survived Stop")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("Stop took %s", took)
	}
}

func TestExitedProcessesAreNotAlive(t *testing.T) {
	dir := t.TempDir()
	p, err := Start(Spec{Path: "/bin/sh", Args: []string{"-c", "exit 3"}, Dir: dir,
		Stdout: filepath.Join(dir, "out.log"), Stderr: filepath.Join(dir, "err.log")})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && Alive(p); time.Sleep(20 * time.Millisecond) {
	}
	if Alive(p) {
		t.Error("an exited process is still alive")
	}
}

// Stop reaches a kernel in a session of its own, outside the server's process group.
func TestStopReachesKernelsInTheirOwnSession(t *testing.T) {
	p, kernel, _ := startServer(t)
	if tree := Tree(p); len(tree) != 2 {
		t.Fatalf("tree = %+v; want the server and its kernel", tree)
	}
	if !Stop(p, time.Second) {
		t.Error("Stop says something is left")
	}
	if running(kernel) {
		t.Error("the kernel survived Stop")
	}
}

// A kernel whose server died is found from what was seen before, and stopped.
func TestStopAllReachesOrphans(t *testing.T) {
	p, kernel, exit := startServer(t)
	seen := Tree(p) // as the driver sees a running workload
	if err := os.WriteFile(exit, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && Alive(p); time.Sleep(20 * time.Millisecond) {
	}
	if Alive(p) {
		t.Fatal("the server didn't exit")
	}
	if !running(kernel) {
		t.Fatal("the kernel should outlive its server here")
	}
	found := false
	for _, q := range seen {
		found = found || q.PID == kernel
	}
	if !found {
		t.Fatalf("the kernel (%d) wasn't seen: %+v", kernel, seen)
	}
	if !StopAll(seen, time.Second) || running(kernel) {
		t.Error("an orphaned kernel survived StopAll")
	}
}
