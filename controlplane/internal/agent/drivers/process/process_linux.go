//go:build linux

package process

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ticksPerSecond is the kernel's USER_HZ, which /proc reports times in: 100 on every Linux
// architecture TensoraFi builds for.
const ticksPerSecond = 100

// startTime is when the kernel started pid, in microseconds since the epoch: the boot time plus its
// start in ticks since boot (/proc/<pid>/stat, field 22). Counting from the boot time means a
// process from after a reboot never matches one recorded before it.
func startTime(pid int) (int64, error) {
	boot, err := bootTime()
	if err != nil {
		return 0, err
	}
	e, err := readStat(pid, boot)
	return e.start, err
}

// table lists every process, with its parent and start time.
func table() ([]entry, error) {
	boot, err := bootTime()
	if err != nil {
		return nil, err
	}
	dirs, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var out []entry
	for _, d := range dirs {
		pid, err := strconv.Atoi(d.Name())
		if err != nil {
			continue
		}
		if e, err := readStat(pid, boot); err == nil {
			out = append(out, e)
		}
	}
	return out, nil
}

func readStat(pid int, boot int64) (entry, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return entry{}, errNoProcess
	}
	// The command name (field 2) is in parentheses and may contain spaces: fields after it are
	// counted from its closing parenthesis.
	s := string(raw)
	end := strings.LastIndexByte(s, ')')
	if end < 0 {
		return entry{}, errNoProcess
	}
	fields := strings.Fields(s[end+1:])
	if len(fields) < 20 {
		return entry{}, errNoProcess
	}
	ppid, err1 := strconv.Atoi(fields[1])               // field 4 overall
	ticks, err2 := strconv.ParseInt(fields[19], 10, 64) // field 22 overall
	if err1 != nil || err2 != nil {
		return entry{}, errNoProcess
	}
	return entry{pid: pid, ppid: ppid, start: boot*1_000_000 + ticks*(1_000_000/ticksPerSecond)}, nil
}

// bootTime is when the machine booted, in seconds since the epoch (/proc/stat's btime).
func bootTime() (int64, error) {
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, err
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "btime "); ok {
			return strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		}
	}
	return 0, errors.New("no boot time in /proc/stat")
}
