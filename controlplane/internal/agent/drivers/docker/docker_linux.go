//go:build linux

package docker

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"
)

// New discovers a local Linux Docker Engine and advertises NVIDIA only when both its container
// runtime and physical device UUIDs are available. Missing GPU support never changes an allocation.
func New(opts Options) (*Driver, error) {
	socket := opts.Socket
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	if !filepath.IsAbs(socket) {
		return nil, fmt.Errorf("TENSORA_DOCKER_SOCKET must be an absolute local Unix socket path")
	}
	e := localEngine(socket)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := e.detect(ctx)
	if err != nil {
		e.close()
		return nil, err
	}
	var devices []Device
	if info.Runtimes["nvidia"] != nil {
		devices = detectNVIDIA(ctx)
	}
	uid, gid := currentUser()
	return newDriver(opts, e, devices, uid, gid)
}

func detectNVIDIA(ctx context.Context) []Device {
	path, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return nil
	}
	cmd := exec.CommandContext(ctx, path, "--query-gpu=uuid,name,memory.total", "--format=csv,noheader,nounits") //nolint:gosec // fixed program discovered on PATH
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return parseNVIDIA(string(out))
}
