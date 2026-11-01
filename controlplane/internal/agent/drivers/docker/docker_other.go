//go:build !linux

package docker

import "errors"

// New refuses remote/container-VM daemons: this driver owns files and GPU IDs on the Linux host.
func New(Options) (*Driver, error) {
	return nil, errors.New("the Docker driver runs on Linux hosts only")
}
