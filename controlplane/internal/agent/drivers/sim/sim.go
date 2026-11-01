// Package sim is a driver that runs nothing: workloads are ready as soon as they're applied. It's
// for development (`tensorafi dev` with TENSORAFI_CLUSTER_BACKEND=agent) and tests, and the control
// plane accepts it only when it's allowed to (TENSORAFI_ENV=dev). Its isolation counts as weak, so it
// never stands in for a strong driver.
package sim

import (
	"context"
	"slices"
	"sync"

	"github.com/google/uuid"

	"github.com/krishnanand20/tensorafi/controlplane/internal/agent/protocol"
	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
)

type Driver struct {
	accelerators []domain.AcceleratorKind

	mu        sync.Mutex
	workloads map[uuid.UUID]protocol.Status
}

// New simulates the given accelerators, usually what the machine has, plus CPU.
func New(accelerators ...domain.AcceleratorKind) *Driver {
	return &Driver{accelerators: slices.Clone(accelerators), workloads: map[uuid.UUID]protocol.Status{}}
}

func (d *Driver) Name() string { return domain.DriverSim }

func (d *Driver) Accelerators() []domain.AcceleratorKind { return slices.Clone(d.accelerators) }

func (d *Driver) Run(_ context.Context, w protocol.Desired) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.workloads[w.ID] = protocol.Status{ID: w.ID, Attempt: w.Attempt, State: protocol.StateReady}
	return nil
}

func (d *Driver) Stop(_ context.Context, id uuid.UUID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if st, ok := d.workloads[id]; ok {
		st.State, st.Code = protocol.StateStopped, ""
		d.workloads[id] = st
	}
	return nil
}

func (d *Driver) Remove(_ context.Context, id uuid.UUID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.workloads, id)
	return nil
}

func (d *Driver) Statuses(context.Context) ([]protocol.Status, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]protocol.Status, 0, len(d.workloads))
	for _, st := range d.workloads {
		out = append(out, st)
	}
	return out, nil
}

// Crash makes a running workload fail, as if it had exited (for tests).
func (d *Driver) Crash(id uuid.UUID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if st, ok := d.workloads[id]; ok {
		st.State, st.Code = protocol.StateFailed, protocol.CodeExited
		d.workloads[id] = st
	}
}
