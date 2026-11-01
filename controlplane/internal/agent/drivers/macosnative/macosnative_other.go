//go:build !darwin

package macosnative

import (
	"errors"
	"log/slog"

	"github.com/tensora/tensora/controlplane/internal/agent"
)

// Options configure the driver.
type Options struct {
	Root    string
	State   string
	Private []string
	UV      string
	Log     *slog.Logger
}

// New reports that macos-native runs only on macOS.
func New(Options) (agent.Driver, error) { return nil, errors.New("macos-native runs only on macOS") }
