// Package sim is a driver that runs nothing: workloads are ready as soon as they're applied. It's
// for development (`einsora dev` with EINSORA_CLUSTER_BACKEND=agent) and tests, and the control
// plane accepts it only when it's allowed to (EINSORA_ENV=dev). Its isolation counts as weak, so it
// never stands in for a strong driver. It keeps output as real drivers do (ADR-0024): a line saying
// what it pretended to do, so the whole output pipeline can be tried in development.
package sim

import (
	"context"
	"fmt"
	"io/fs"
	"slices"
	"sync"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/agent/protocol"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
)

type Driver struct {
	accelerators []domain.AcceleratorKind

	mu        sync.Mutex
	workloads map[uuid.UUID]protocol.Status
	output    map[uuid.UUID]*output
	sources   int // numbers each workload's output, so a removed and recreated one starts over
}

type output struct {
	source string
	data   map[domain.OutputStream][]byte
}

// New simulates the given accelerators, usually what the machine has, plus CPU.
func New(accelerators ...domain.AcceleratorKind) *Driver {
	return &Driver{accelerators: slices.Clone(accelerators), workloads: map[uuid.UUID]protocol.Status{}, output: map[uuid.UUID]*output{}}
}

// write adds text to a workload's stream. Call with d.mu held.
func (d *Driver) write(id uuid.UUID, stream domain.OutputStream, text string) {
	o, ok := d.output[id]
	if !ok {
		d.sources++
		o = &output{source: fmt.Sprintf("sim-%d", d.sources), data: map[domain.OutputStream][]byte{}}
		d.output[id] = o
	}
	o.data[stream] = append(o.data[stream], text...)
}

// Write adds text to a workload's output, as if it had printed it (for tests).
func (d *Driver) Write(id uuid.UUID, stream domain.OutputStream, text string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.write(id, stream, text)
}

// ReadOutput implements agent.OutputReader.
func (d *Driver) ReadOutput(_ context.Context, id uuid.UUID, stream domain.OutputStream, offset int64, limit int) ([]byte, string, int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	o, ok := d.output[id]
	if !ok {
		return nil, "", 0, fs.ErrNotExist
	}
	data := o.data[stream]
	size := int64(len(data))
	if offset >= size {
		return nil, o.source, size, nil
	}
	return slices.Clone(data[offset:min(size, offset+int64(limit))]), o.source, size, nil
}

func (d *Driver) Name() string { return domain.DriverSim }

func (d *Driver) Accelerators() []domain.AcceleratorKind { return slices.Clone(d.accelerators) }

func (d *Driver) Run(_ context.Context, w protocol.Desired) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if st, ok := d.workloads[w.ID]; !ok || st.Attempt != w.Attempt || st.State != protocol.StateReady {
		d.write(w.ID, domain.StreamStdout, fmt.Sprintf("sim: pretending to run this %s on %s (attempt %d); nothing really runs\n",
			w.Kind, w.Accelerator, w.Attempt))
	}
	d.workloads[w.ID] = protocol.Status{ID: w.ID, Attempt: w.Attempt, State: protocol.StateReady}
	return nil
}

func (d *Driver) Stop(_ context.Context, id uuid.UUID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if st, ok := d.workloads[id]; ok {
		if st.State == protocol.StateReady {
			d.write(id, domain.StreamStdout, "sim: stopped\n")
		}
		st.State, st.Code = protocol.StateStopped, ""
		d.workloads[id] = st
	}
	return nil
}

func (d *Driver) Remove(_ context.Context, id uuid.UUID) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.workloads, id)
	delete(d.output, id)
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
