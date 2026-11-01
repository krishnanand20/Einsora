//go:build linux

package inventory

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
)

func memoryMiB() (int64, error) {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, fmt.Errorf("read /proc/meminfo: %w", err)
	}
	mib, ok := parseMeminfo(string(raw))
	if !ok {
		return 0, fmt.Errorf("/proc/meminfo has no MemTotal")
	}
	return mib, nil
}

// accelerators lists NVIDIA GPUs through nvidia-smi, if the driver is installed. AMD comes later.
func accelerators(ctx context.Context, _ int64) []domain.Accelerator {
	path, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--query-gpu=name,memory.total", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil
	}
	return parseNvidiaSMI(string(out))
}
