// Package fake is an in-memory cluster.Backend for `tensorafi dev` and tests. It reports cluster-wide
// capacity per class, like Kubernetes, and workloads become ready as soon as they're run.
package fake

import (
	"context"
	"fmt"
	"maps"
	"sync"

	"github.com/google/uuid"

	"github.com/krishnanand20/tensorafi/controlplane/internal/cluster"
)

type Backend struct {
	mu          sync.Mutex
	capacity    map[string]int
	running     map[uuid.UUID]bool // workload → desired running
	overrides   map[uuid.UUID]cluster.Observed
	runErr      error
	observeErr  error
	capacityErr error
}

func New(capacity map[string]int) *Backend {
	return &Backend{
		capacity:  maps.Clone(capacity),
		running:   map[uuid.UUID]bool{},
		overrides: map[uuid.UUID]cluster.Observed{},
	}
}

// SetObserved forces what Observe reports for a workload (e.g. to simulate a stuck or failed one).
func (b *Backend) SetObserved(id uuid.UUID, o cluster.Observed) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.overrides[id] = o
}

// FailRun makes every Run and Park fail with err (nil restores normal behavior), to simulate a
// runtime that rejects or can't be reached.
func (b *Backend) FailRun(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.runErr = err
}

// FailObserve makes every Observe fail with err (nil restores normal behavior).
func (b *Backend) FailObserve(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.observeErr = err
}

// FailCapacity makes Capacity fail with err (nil restores normal behavior).
func (b *Backend) FailCapacity(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.capacityErr = err
}

func (b *Backend) Capacity(context.Context) (cluster.Capacity, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.capacityErr != nil {
		return cluster.Capacity{}, b.capacityErr
	}
	return cluster.Capacity{Classes: maps.Clone(b.capacity)}, nil
}

func (b *Backend) Run(_ context.Context, s cluster.Spec) error { return b.set(s, true) }

func (b *Backend) Park(_ context.Context, s cluster.Spec) error { return b.set(s, false) }

func (b *Backend) set(s cluster.Spec, running bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.runErr != nil {
		return b.runErr
	}
	if s.Class.ID == "" || (s.Workload.PlacedClass != "" && s.Class.ID != s.Workload.PlacedClass) {
		return fmt.Errorf("fake: workload %s: the spec's class isn't the one it's placed on", s.Workload.ID)
	}
	b.running[s.Workload.ID] = running
	return nil
}

func (b *Backend) Observe(_ context.Context, ref cluster.Ref) (cluster.Observed, string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.observeErr != nil {
		return "", "", b.observeErr
	}
	if o, ok := b.overrides[ref.ID]; ok {
		return o, "forced by test", nil
	}
	running, ok := b.running[ref.ID]
	switch {
	case !ok:
		return cluster.ObservedAbsent, "", nil
	case running:
		return cluster.ObservedReady, "", nil
	default:
		return cluster.ObservedStopped, "", nil
	}
}

func (b *Backend) Delete(_ context.Context, ref cluster.Ref) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.running, ref.ID)
	delete(b.overrides, ref.ID)
	return nil
}
