package domain

import (
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// Region is one location of a deployment: a home, an office, or a datacenter (ADR-0009). Every
// workload and machine belongs to exactly one, and a workload's data never leaves it (rule 9).
type Region string

// RegionLocal is the only region of a Solo install.
const RegionLocal Region = "local"

// Valid reports whether r is a region ID: lowercase and location-based, like local or in-mum-1.
func (r Region) Valid() bool { return ValidSlug(string(r)) }

// AcceleratorKind is the kind of compute a machine offers or a class needs (ADR-0013).
type AcceleratorKind string

const (
	AcceleratorNVIDIA AcceleratorKind = "nvidia"
	AcceleratorApple  AcceleratorKind = "apple" // the Apple Silicon GPU (Metal), with unified memory
	AcceleratorAMD    AcceleratorKind = "amd"
	AcceleratorCPU    AcceleratorKind = "cpu"
)

func (k AcceleratorKind) Valid() bool {
	switch k {
	case AcceleratorNVIDIA, AcceleratorApple, AcceleratorAMD, AcceleratorCPU:
		return true
	}
	return false
}

// Accelerator is one kind of device in a machine's inventory.
type Accelerator struct {
	Kind      AcceleratorKind `json:"kind"`
	Model     string          `json:"model"` // as the driver reports it, for example "NVIDIA L40S" or "Apple M3 Max"
	Count     int             `json:"count"`
	MemoryMiB int64           `json:"memory_mib"` // per device; unified memory for Apple
}

// SharingLevel decides who may run work on a machine (ADR-0014).
type SharingLevel string

const (
	SharingPrivate   SharingLevel = "private"   // the owner's tenant only (the default)
	SharingInvite    SharingLevel = "invite"    // named users, groups, or tenants
	SharingCommunity SharingLevel = "community" // anyone with an account on the instance
)

func (l SharingLevel) Valid() bool {
	switch l {
	case SharingPrivate, SharingInvite, SharingCommunity:
		return true
	}
	return false
}

// Machine is a computer that runs workloads through the node agent (ADR-0013). Its owner is one
// tenant, and it belongs to exactly one region. Its agent reports the inventory (OS through
// Accelerators), which never decides the owner, name, region, or sharing.
type Machine struct {
	ID           uuid.UUID
	TenantID     uuid.UUID // the owner
	Name         string
	Region       Region
	OS           string // "linux" or "darwin"
	Arch         string // "amd64" or "arm64"
	CPUs         int    // logical CPUs; zero until the agent reports
	MemoryMiB    int64  // total memory, unified memory on Apple Silicon; zero until the agent reports
	Accelerators []Accelerator
	Sharing      SharingLevel
	LastSeenAt   time.Time // zero until the agent's first heartbeat
	CreatedAt    time.Time
}

// Validate checks a machine before it's stored. The error is safe to show its owner.
func (m Machine) Validate() error {
	switch {
	case !ValidSlug(m.Name):
		return Errorf(CodeInvalid, "machine name must be 1-40 lowercase letters, digits, or '-'")
	case !m.Region.Valid():
		return Errorf(CodeInvalid, "machine %s needs exactly one valid region, got %q", m.Name, m.Region)
	case m.OS != "linux" && m.OS != "darwin":
		return Errorf(CodeInvalid, "machine %s: os must be linux or darwin, got %q", m.Name, m.OS)
	case m.Arch != "amd64" && m.Arch != "arm64":
		return Errorf(CodeInvalid, "machine %s: arch must be amd64 or arm64, got %q", m.Name, m.Arch)
	case !m.Sharing.Valid():
		return Errorf(CodeInvalid, "machine %s: sharing must be private, invite, or community, got %q", m.Name, m.Sharing)
	case m.CPUs < 0 || m.CPUs > maxCPUs || m.MemoryMiB < 0 || m.MemoryMiB > maxMemoryMiB || len(m.Accelerators) > maxAccelerators:
		return Errorf(CodeInvalid, "machine %s: its inventory is out of range", m.Name)
	}
	for i, a := range m.Accelerators {
		// The agent reports these, and a host may be hostile: bound the text, and never echo it.
		if !a.Kind.Valid() || a.Count < 1 || a.Count > maxDevices || a.MemoryMiB < 0 || a.MemoryMiB > maxMemoryMiB || len(a.Model) > maxModelLen ||
			strings.IndexFunc(a.Model, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
			return Errorf(CodeInvalid, "machine %s: accelerator %d is invalid", m.Name, i)
		}
	}
	return nil
}

// Bounds on what an agent may report: generous for real machines, small enough that a hostile host
// can't make the control plane store or compute with absurd values.
const (
	maxModelLen     = 100
	maxCPUs         = 4096
	maxMemoryMiB    = 64 << 20 // 64 TiB
	maxAccelerators = 16       // kinds of device, each with a count
	maxDevices      = 64
)

// CanHost reports whether the machine may run w: it must be in w's region (rule 9), and a machine
// that isn't in a community pool runs only its owner's workloads (ADR-0014). Invite lists arrive
// with sharing (Phase 4); until then an invite-only machine is treated as private.
func (m Machine) CanHost(w Workload) error {
	switch {
	case m.Region != w.Region:
		return Errorf(CodeInvalid, "machine %s is in region %s, not the workload's region %s", m.Name, m.Region, w.Region)
	case m.TenantID != w.TenantID && m.Sharing != SharingCommunity:
		return Errorf(CodeForbidden, "machine %s isn't shared with this workload's tenant", m.Name)
	}
	return nil
}

// Isolation is how strongly an agent driver separates a workload from its host and from other
// workloads (the trust matrix in docs/architecture/tenancy-and-isolation.md).
type Isolation string

const (
	IsolationStrong Isolation = "strong" // a VM, or a container in its own VM or sandboxed kernel
	IsolationWeak   Isolation = "weak"   // a shared kernel, or the macOS sandbox
)

// Agent drivers (ADR-0013).
const (
	DriverSim            = "sim" // runs nothing and reports ready; development and tests only
	DriverMacOSNative    = "macos-native"
	DriverMacOSVZ        = "macos-vz"
	DriverMacOSContainer = "macos-container"
	DriverDocker         = "docker" // runc: a shared kernel
	DriverLibvirt        = "libvirt"
)

// DriverSpec is what an agent driver runs, where, and how strongly it isolates (ADR-0013 and the
// trust matrix). It's decided here, never by an agent, which only says which drivers it has and
// which of these accelerators each can use on its machine.
type DriverSpec struct {
	Isolation    Isolation
	OS           string // the only OS it runs on; empty for any
	Kinds        []WorkloadKind
	Accelerators []AcceleratorKind
	// MaxPerMachine limits how many of its workloads one machine runs at once; 0 means no limit.
	MaxPerMachine int
}

// LookupDriver returns driver's spec, or false for a driver the control plane doesn't know.
func LookupDriver(driver string) (DriverSpec, bool) {
	notebooksAndEndpoints := []WorkloadKind{KindNotebook, KindEndpoint}
	switch driver {
	case DriverSim:
		return DriverSpec{Isolation: IsolationWeak, Kinds: []WorkloadKind{KindVM, KindNotebook, KindEndpoint},
			Accelerators: []AcceleratorKind{AcceleratorNVIDIA, AcceleratorApple, AcceleratorAMD, AcceleratorCPU}}, true
	case DriverMacOSNative:
		return DriverSpec{Isolation: IsolationWeak, OS: "darwin", Kinds: notebooksAndEndpoints,
			Accelerators: []AcceleratorKind{AcceleratorApple, AcceleratorCPU}}, true
	case DriverMacOSVZ:
		// macOS guests get the Mac's GPU through paravirtualized Metal, so a VM is always the GPU's
		// whole unit: no CPU-only VMs that would share it. The macOS licence allows two per Mac.
		return DriverSpec{Isolation: IsolationStrong, OS: "darwin", Kinds: []WorkloadKind{KindVM},
			Accelerators: []AcceleratorKind{AcceleratorApple}, MaxPerMachine: 2}, true
	case DriverMacOSContainer:
		return DriverSpec{Isolation: IsolationStrong, OS: "darwin", Kinds: notebooksAndEndpoints,
			Accelerators: []AcceleratorKind{AcceleratorCPU}}, true
	case DriverDocker:
		return DriverSpec{Isolation: IsolationWeak, OS: "linux", Kinds: notebooksAndEndpoints,
			Accelerators: []AcceleratorKind{AcceleratorNVIDIA, AcceleratorCPU}}, true
	case DriverLibvirt:
		return DriverSpec{Isolation: IsolationStrong, OS: "linux", Kinds: []WorkloadKind{KindVM},
			Accelerators: []AcceleratorKind{AcceleratorNVIDIA, AcceleratorCPU}}, true
	}
	return DriverSpec{}, false
}

// Permits reports whether w may run on m with driver (the trust matrix): weak isolation is only for
// the machine owner's own workloads, since other people's code could reach the host. Region and
// sharing are CanHost's to check.
func (m Machine) Permits(w Workload, driver string) error {
	spec, ok := LookupDriver(driver)
	switch {
	case !ok:
		return Errorf(CodeInvalid, "machine %s: unknown driver %q", m.Name, driver)
	case spec.Isolation == IsolationWeak && m.TenantID != w.TenantID:
		return Errorf(CodeForbidden, "machine %s would run this with %s, whose isolation is only for its owner's own workloads", m.Name, driver)
	}
	return nil
}

// CheckPlacement is what a store checks before recording that w runs on m with driver: m may host
// it (CanHost) with that driver's isolation (Permits).
func (m Machine) CheckPlacement(w Workload, driver string) error {
	if err := m.CanHost(w); err != nil {
		return err
	}
	return m.Permits(w, driver)
}

// ErrDriverWithoutMachine refuses a placement that names a driver but no machine: drivers run on
// agents' machines only.
var ErrDriverWithoutMachine = Errorf(CodeInvalid, "a placement that names a driver needs a machine")

// CheckPlaced reports whether a workload may enter state to. Once it's provisioning it holds
// capacity, and once it's running it's metered, so from then on it must say what it runs on:
// capacity is counted and usage accounted for what actually runs (ADR-0010).
func (w Workload) CheckPlaced(to State) error {
	if (to == StateProvisioning || to.Metered()) && w.PlacedClass == "" {
		return Errorf(CodeInvalidTransition, "workload %s can't be %s before it's placed on a class", w.ID, to)
	}
	return nil
}
