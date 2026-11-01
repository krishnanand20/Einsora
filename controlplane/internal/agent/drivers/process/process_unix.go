//go:build darwin || linux

package process

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Start starts spec in a new process group and returns it. The process outlives the agent, which
// finds it again by its Proc. Start reaps it when it exits, while this process runs.
func Start(spec Spec) (Proc, error) {
	stdout, err := openLog(spec.Stdout)
	if err != nil {
		return Proc{}, err
	}
	defer func() { _ = stdout.Close() }()
	stderr, err := openLog(spec.Stderr)
	if err != nil {
		return Proc{}, err
	}
	defer func() { _ = stderr.Close() }()
	cmd := exec.Command(spec.Path, spec.Args...) //nolint:gosec // the driver builds the command from its own fixed parts
	cmd.Env, cmd.Dir, cmd.Stdout, cmd.Stderr = spec.Env, spec.Dir, stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return Proc{}, fmt.Errorf("start %s: %w", spec.Path, err)
	}
	go func() { _ = cmd.Wait() }() // reap it, so an exited child doesn't linger as a zombie
	start, err := startTime(cmd.Process.Pid)
	if err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return Proc{}, err
	}
	return Proc{PID: cmd.Process.Pid, Start: start}, nil
}

// openLog opens a log file for appending, never through a symlink: the file is handed to the
// workload's process, so it must be the file the driver meant.
func openLog(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600) //nolint:gosec // the driver's own log path
	if err != nil {
		return nil, fmt.Errorf("open log: %w", err)
	}
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("open log: %s isn't a regular file", path)
	}
	return f, nil
}

// Alive reports whether p is still running: the same process, not another given its ID.
func Alive(p Proc) bool {
	if p.PID <= 0 || syscall.Kill(p.PID, 0) != nil {
		return false
	}
	start, err := startTime(p.PID)
	return err == nil && start == p.Start
}

// Tree is every process in procs that's still running, and every process descended from them. A
// process whose parent exited before it was seen has been handed to launchd (or init), and isn't
// found: callers keep what they've seen and pass it back in.
func Tree(procs ...Proc) []Proc {
	tab, err := table()
	if err != nil {
		var out []Proc
		for _, p := range procs {
			if Alive(p) {
				out = append(out, p)
			}
		}
		return out
	}
	byPID, children := map[int]entry{}, map[int][]entry{}
	for _, e := range tab {
		byPID[e.pid] = e
		children[e.ppid] = append(children[e.ppid], e)
	}
	var out []Proc
	seen := map[int]bool{}
	for _, p := range procs {
		if e, ok := byPID[p.PID]; ok && e.start == p.Start && !seen[p.PID] {
			seen[p.PID] = true
			out = append(out, p)
		}
	}
	for i := 0; i < len(out); i++ {
		for _, c := range children[out[i].PID] {
			if !seen[c.pid] && c.pid > 1 {
				seen[c.pid] = true
				out = append(out, Proc{PID: c.pid, Start: c.start})
			}
		}
	}
	return out
}

// Stop stops p and every process descended from it: SIGTERM, a grace period of up to wait, then
// SIGKILL for whatever is left, including anything started meanwhile. It reports whether they're
// all gone.
func Stop(p Proc, wait time.Duration) bool {
	return StopAll([]Proc{p}, wait)
}

// StopAll is Stop for processes seen earlier, such as a workload's children after its server
// exited: they're stopped with everything descended from them.
func StopAll(procs []Proc, wait time.Duration) bool {
	known := Tree(procs...)
	if len(known) == 0 {
		return true
	}
	signal(known, syscall.SIGTERM)
	grace := min(wait, stopGrace)
	for deadline := time.Now().Add(grace); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if known = Tree(known...); len(known) == 0 {
			return true
		}
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if known = Tree(known...); len(known) == 0 {
			return true
		}
		signal(known, syscall.SIGKILL)
	}
	return len(Tree(known...)) == 0
}

// signal sends sig to each process that's still the one it was, and to the process group of each
// group leader among them, which reaches children not seen yet.
func signal(procs []Proc, sig syscall.Signal) {
	for _, p := range procs {
		if !Alive(p) {
			continue
		}
		if pgid, err := syscall.Getpgid(p.PID); err == nil && pgid == p.PID {
			_ = syscall.Kill(-p.PID, sig)
		}
		_ = syscall.Kill(p.PID, sig)
	}
}

var errNoProcess = errors.New("no such process")
