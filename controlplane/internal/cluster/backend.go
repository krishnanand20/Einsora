// Package cluster is the only boundary between Einsora and the runtimes that run workloads
// (AGENTS.md rule 2): Kubernetes (kube), node agents (agent), and an in-memory fake. The interface
// is runtime-neutral (ADR-0016): the controller places a workload and hands over a Spec; each
// backend turns it into what its runtime needs.
package cluster

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/catalog"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
)

// Manifest is one Kubernetes object as unstructured JSON-compatible data.
type Manifest = map[string]any

// Observed is the runtime condition of a workload.
type Observed string

const (
	ObservedAbsent      Observed = "absent"
	ObservedProgressing Observed = "progressing"
	ObservedReady       Observed = "ready"
	ObservedStopped     Observed = "stopped"
	ObservedFailed      Observed = "failed"
	// ObservedUnknown means the runtime can't tell right now, such as when the workload's machine
	// is offline. Nothing is decided on it, except that provisioning still times out.
	ObservedUnknown Observed = "unknown"
)

// Spec is everything a backend needs to run one workload, as the controller placed it.
type Spec struct {
	Workload   domain.Workload // placed: PlacedClass, and on agents MachineID and Driver
	TenantSlug string
	Tier       domain.IsolationTier
	Class      catalog.GPUClass // the placed class
	Image      catalog.Image
	Runtime    catalog.Runtime // the image's runtime for the class's accelerator
	Quotas     []domain.Quota  // the tenant's quotas, for Kubernetes' ResourceQuota backstop
}

// Ref names a workload in its runtime, and says what the store expects of it: its state and
// attempt, so a backend can tell an answer from a report about an earlier attempt.
type Ref struct {
	ID         uuid.UUID
	TenantSlug string
	Kind       domain.WorkloadKind
	MachineID  uuid.UUID // the agent machine, or uuid.Nil
	State      domain.State
	Attempt    int64
}

// Ref is the spec's workload's Ref.
func (s Spec) Ref() Ref {
	w := s.Workload
	return Ref{ID: w.ID, TenantSlug: s.TenantSlug, Kind: w.Kind, MachineID: w.MachineID, State: w.State, Attempt: w.Attempt}
}

// Capacity is where a backend can place workloads: units per class in a cluster that schedules
// them onto its own nodes (Kubernetes), or machines the controller chooses among (agents).
type Capacity struct {
	Classes  map[string]int // nil when the controller places on Machines
	Machines []MachineCapacity
	// Settling means the backend is still learning what's connected, such as right after a
	// restart: nothing should be decided on it yet.
	Settling bool
}

// MachineCapacity is one connected machine: its devices per catalog class, the CPUs and memory
// Einsora may use there (its agent keeps the rest for the owner), and the drivers it has.
type MachineCapacity struct {
	ID        uuid.UUID
	Devices   map[string]int // devices per GPU class; CPU classes need none
	VCPUs     int
	MemoryMiB int64
	Drivers   []Driver // in the agent's order of preference
}

// Driver is an agent driver available on a machine, and which accelerators it can use there.
// What it runs and how strongly it isolates come from domain.LookupDriver, never from the agent.
type Driver struct {
	Name         string
	Accelerators []domain.AcceleratorKind
}

// Leader is a Backend that needs to know when its controller starts leading its region, such as the
// agent backend, whose machines' agents sync only with a leader.
type Leader interface {
	Lead()
}

type Backend interface {
	// Capacity reports where workloads can go. Allocation comes from the store, because every
	// workload goes through Einsora; the controller subtracts it.
	Capacity(ctx context.Context) (Capacity, error)
	// Run makes the workload run: create it, or update it to running. Idempotent.
	Run(ctx context.Context, s Spec) error
	// Park stops the workload and frees its GPUs while keeping its disks (VMs and notebooks).
	Park(ctx context.Context, s Spec) error
	// Observe reports the workload's state plus a human-readable reason. The reason is shown to the
	// workload's owner, so it must come from a fixed vocabulary: never raw runtime messages, object
	// or node names, or anything that echoes inputs (ADR-0015).
	Observe(ctx context.Context, ref Ref) (Observed, string, error)
	// Delete removes the workload and everything it stored, including volumes. Idempotent.
	Delete(ctx context.Context, ref Ref) error
}

// CheckJSONTypes reports values Kubernetes' unstructured machinery cannot handle: it accepts
// only map[string]any, []any, string, bool, int64, float64, and nil. Builders must pass this;
// the fake backend enforces it so mistakes surface in tests, not in production.
func CheckJSONTypes(v any) error { return checkJSON(v, "$") }

func checkJSON(v any, path string) error {
	switch x := v.(type) {
	case nil, string, bool, int64, float64:
		return nil
	case map[string]any:
		for k, e := range x {
			if err := checkJSON(e, path+"."+k); err != nil {
				return err
			}
		}
		return nil
	case []any:
		for i, e := range x {
			if err := checkJSON(e, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("%s: unsupported type %T (use int64, []any, map[string]any)", path, v)
}
