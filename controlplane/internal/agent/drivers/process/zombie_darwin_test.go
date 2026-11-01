//go:build darwin

package process

import "golang.org/x/sys/unix"

// zombie reports whether pid has exited and waits to be reaped.
func zombie(pid int) bool {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	return err == nil && kp.Proc.P_stat == 5 // SZOMB
}
