//go:build linux

package process

import (
	"fmt"
	"os"
	"strings"
)

// zombie reports whether pid has exited and waits to be reaped.
func zombie(pid int) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	s := string(raw)
	end := strings.LastIndexByte(s, ')')
	return end >= 0 && strings.HasPrefix(strings.TrimSpace(s[end+1:]), "Z")
}
