//go:build darwin

package inventory

import (
	"context"
	"fmt"

	"golang.org/x/sys/unix"

	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
)

func memoryMiB() (int64, error) {
	b, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, fmt.Errorf("sysctl hw.memsize: %w", err)
	}
	return int64(b / (1 << 20)), nil //nolint:gosec // memory in MiB fits in an int64
}

func accelerators(_ context.Context, memMiB int64) []domain.Accelerator {
	brand, err := unix.Sysctl("machdep.cpu.brand_string")
	if err != nil {
		return nil
	}
	return appleGPU(brand, memMiB)
}
