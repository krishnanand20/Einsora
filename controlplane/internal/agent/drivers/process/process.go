// Package process supervises the processes native drivers run for workloads (ADR-0017): each in its
// own process group, with output to log files, identified by its process ID and start time so a
// reused process ID is never taken for it, and found again after the agent restarts. A workload is
// its process and everything descended from it, found by parent process ID: Jupyter starts each
// kernel in a session of its own, outside the server's process group. Unix only; OS-specific code
// is behind build tags (rule 11).
package process

import (
	"errors"
	"time"
)

// Spec is a process to start.
type Spec struct {
	Path   string
	Args   []string // not including Path
	Env    []string // the whole environment
	Dir    string
	Stdout string // log files, appended to and created owner-only
	Stderr string
}

// Proc identifies a started process: its ID, and when it started, so a later process given the same
// ID isn't mistaken for it.
type Proc struct {
	PID   int   `json:"pid"`
	Start int64 `json:"start"` // the OS's start time for it, in microseconds since the epoch
}

// entry is one process in the OS's process table.
type entry struct {
	pid, ppid int
	start     int64
}

// ErrUnsupported means processes can't be supervised on this OS.
var ErrUnsupported = errors.New("native workloads aren't supported on this OS")

// stopGrace is how long a process group gets to exit after SIGTERM before SIGKILL.
const stopGrace = 10 * time.Second
