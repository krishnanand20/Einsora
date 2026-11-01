//go:build !darwin && !linux

package inventory

import (
	"context"

	"github.com/krishnanand20/einsora/controlplane/internal/domain"
)

func memoryMiB() (int64, error) { return 0, nil }

func accelerators(context.Context, int64) []domain.Accelerator { return nil }
