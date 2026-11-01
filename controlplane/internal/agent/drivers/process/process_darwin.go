//go:build darwin

package process

import "golang.org/x/sys/unix"

// startTime is when the kernel started pid, in microseconds since the epoch.
func startTime(pid int) (int64, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	if kp.Proc.P_starttime.Sec == 0 {
		return 0, errNoProcess
	}
	return kp.Proc.P_starttime.Sec*1_000_000 + int64(kp.Proc.P_starttime.Usec), nil
}

// table lists every process, with its parent and start time.
func table() ([]entry, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	out := make([]entry, 0, len(kps))
	for _, kp := range kps {
		out = append(out, entry{pid: int(kp.Proc.P_pid), ppid: int(kp.Eproc.Ppid),
			start: kp.Proc.P_starttime.Sec*1_000_000 + int64(kp.Proc.P_starttime.Usec)})
	}
	return out, nil
}
