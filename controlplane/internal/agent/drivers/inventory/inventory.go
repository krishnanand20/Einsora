// Package inventory detects a machine's hardware for its agent's reports (ADR-0013): CPUs, memory,
// and accelerators (Apple Silicon GPUs through sysctl on macOS; NVIDIA GPUs through nvidia-smi on
// Linux). OS-specific code is behind build tags (rule 11); parsing is plain functions, tested on
// every platform.
package inventory

import (
	"bufio"
	"context"
	"runtime"
	"strconv"
	"strings"
	"unicode"

	"github.com/krishnanand20/einsora/controlplane/internal/agent/protocol"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
)

// Detect reports this machine's hardware, and what Einsora may use of it: all but a quarter of its
// CPUs (at least one kept) and three quarters of its memory. The rest stays the owner's (ADR-0013).
func Detect(ctx context.Context) (protocol.Inventory, error) {
	mem, err := memoryMiB()
	if err != nil {
		return protocol.Inventory{}, err
	}
	cpus := runtime.NumCPU()
	inv := protocol.Inventory{
		OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: cpus, MemoryMiB: mem,
		Accelerators: accelerators(ctx, mem),
		UsableCPUs:   max(cpus-max(1, cpus/4), 1), UsableMemoryMiB: mem * 3 / 4,
	}
	if inv.UsableCPUs > cpus {
		inv.UsableCPUs = cpus
	}
	return inv, nil
}

// parseMeminfo reads MemTotal from /proc/meminfo, in MiB.
func parseMeminfo(s string) (int64, bool) {
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kib, err := strconv.ParseInt(fields[1], 10, 64)
			return kib / 1024, err == nil && kib > 0
		}
	}
	return 0, false
}

// parseNvidiaSMI reads `nvidia-smi --query-gpu=name,memory.total --format=csv,noheader,nounits`:
// one line per GPU, grouped by model.
func parseNvidiaSMI(s string) []domain.Accelerator {
	var out []domain.Accelerator
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		name, mem, ok := strings.Cut(sc.Text(), ",")
		name = cleanModel(name)
		mib, err := strconv.ParseInt(strings.TrimSpace(mem), 10, 64)
		if !ok || name == "" || err != nil || mib <= 0 {
			continue
		}
		found := false
		for i := range out {
			if out[i].Model == name && out[i].MemoryMiB == mib {
				out[i].Count++
				found = true
			}
		}
		if !found {
			out = append(out, domain.Accelerator{Kind: domain.AcceleratorNVIDIA, Model: name, Count: 1, MemoryMiB: mib})
		}
	}
	return out
}

// appleGPU is a Mac's GPU: the chip's name (machdep.cpu.brand_string, such as "Apple M3 Max") and
// its unified memory. Anything that isn't Apple Silicon has none.
func appleGPU(brand string, memMiB int64) []domain.Accelerator {
	brand = cleanModel(brand)
	if !strings.HasPrefix(brand, "Apple M") || memMiB <= 0 {
		return nil
	}
	return []domain.Accelerator{{Kind: domain.AcceleratorApple, Model: brand, Count: 1, MemoryMiB: memMiB}}
}

// cleanModel trims a device name to printable text of a bounded length.
func cleanModel(s string) string {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, s))
	if len(s) > 100 {
		s = s[:100]
	}
	return s
}
