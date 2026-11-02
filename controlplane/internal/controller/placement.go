package controller

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/catalog"
	"github.com/krishnanand20/einsora/controlplane/internal/cluster"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
)

// Placement decides where admitted workloads run (ADR-0016). It's pure: the controller loads what
// it needs once per pass, and every rule about machines, drivers, and isolation lives here.

// placement is where a workload runs: a class and, on agents, a machine and driver.
type placement struct {
	class     string
	machineID uuid.UUID
	machine   string // the machine's name, for the timeline
	driver    string
}

// pool is what's free during one admission pass.
type pool struct {
	cat *catalog.Catalog
	// classes is free units per class in a cluster that schedules itself (Kubernetes); nil when the
	// controller places on machines.
	classes  map[string]int
	machines []*slot
}

// slot is one connected machine that's in the controller's region, with what's free on it.
type slot struct {
	cat       *catalog.Catalog
	m         domain.Machine
	drivers   []cluster.Driver
	devices   map[string]int
	vcpus     int
	memMiB    int64
	running   map[string]int // workloads holding capacity here, per driver (for DriverSpec.MaxPerMachine)
	appleHeld int
	nativeCPU bool // a native CPU notebook or endpoint can still use Metal, so it reserves the Apple GPU
}

// newPool builds the pass's view: capacity from the backend, minus what holding workloads use.
// Machines the backend reports but the store doesn't list in this region are ignored: a machine's
// region comes from its registration, never from its agent.
func newPool(cat *catalog.Catalog, capacity cluster.Capacity, machines []domain.Machine, holding []domain.Workload) *pool {
	p := &pool{cat: cat}
	if capacity.Classes != nil {
		p.classes = map[string]int{}
		for class, n := range capacity.Classes {
			p.classes[class] = n
		}
		for _, w := range holding {
			p.classes[classOf(w)] -= w.GPUCount
		}
		return p
	}
	known := map[uuid.UUID]domain.Machine{}
	for _, m := range machines {
		known[m.ID] = m
	}
	for _, mc := range capacity.Machines {
		m, ok := known[mc.ID]
		if !ok {
			continue
		}
		s := &slot{cat: cat, m: m, drivers: mc.Drivers, devices: map[string]int{}, vcpus: mc.VCPUs, memMiB: mc.MemoryMiB, running: map[string]int{}}
		for class, n := range mc.Devices {
			s.devices[class] = n
		}
		p.machines = append(p.machines, s)
	}
	for _, w := range holding {
		for _, s := range p.machines {
			if s.m.ID != w.MachineID {
				continue
			}
			if g, ok := cat.GPUClass(classOf(w)); ok {
				s.take(g, w.GPUCount, w.Driver)
			} else {
				s.saturate(w.Driver) // its class left the catalog: what it holds is unknown, so assume all of it
			}
		}
	}
	return p
}

// classOf is the class a workload holds: the placed one, or the requested one for a workload
// admitted before placements were recorded, so capacity is never under-counted.
func classOf(w domain.Workload) string {
	if w.PlacedClass != "" {
		return w.PlacedClass
	}
	return w.GPUClass
}

// needs is what n units of class g take on a machine.
func needs(g catalog.GPUClass, n int) (devices, vcpus int, memMiB int64) {
	if g.Accelerator != domain.AcceleratorCPU {
		devices = n
	}
	return devices, g.VCPUsPerGPU * n, int64(g.MemoryGiBPerGPU) * 1024 * int64(n)
}

func (s *slot) take(g catalog.GPUClass, n int, driver string) {
	devices, vcpus, mem := needs(g, n)
	if g.Accelerator == domain.AcceleratorApple {
		s.appleHeld += devices
	}
	if driver == domain.DriverMacOSNative && g.Accelerator == domain.AcceleratorCPU {
		s.nativeCPU = true
	}
	s.devices[g.ID] -= devices
	s.vcpus -= vcpus
	s.memMiB -= mem
	if driver != "" {
		s.running[driver]++
	}
}

// saturate marks everything on s as taken, by a workload whose size isn't known.
func (s *slot) saturate(driver string) {
	s.nativeCPU = true
	for class := range s.devices {
		s.devices[class] = 0
	}
	s.vcpus, s.memMiB = 0, 0
	if driver != "" {
		s.running[driver]++
	}
}

// fits reports how many units of g fit in what's free on s, run with driver.
func (s *slot) fits(g catalog.GPUClass, driver string) int {
	if g.Accelerator == domain.AcceleratorApple && s.nativeCPU ||
		driver == domain.DriverMacOSNative && g.Accelerator == domain.AcceleratorCPU && (s.nativeCPU || s.appleHeld > 0) {
		return 0
	}
	if spec, ok := domain.LookupDriver(driver); ok && spec.MaxPerMachine > 0 && s.running[driver] >= spec.MaxPerMachine {
		return 0
	}
	devices, vcpus, mem := needs(g, 1)
	n := 1 << 30
	if devices > 0 {
		n = min(n, s.devices[g.ID])
	}
	if vcpus > 0 {
		n = min(n, s.vcpus/vcpus)
	}
	if mem > 0 {
		n = min(n, int(s.memMiB/mem))
	}
	return max(n, 0)
}

// offers reports whether s could ever run g: it has devices of g's class, or g needs none.
func (s *slot) offers(g catalog.GPUClass) bool {
	_, ok := s.devices[g.ID]
	return g.Accelerator == domain.AcceleratorCPU || ok
}

// keepsDriver reports whether w must run with the driver it ran with before: a VM or notebook that
// has run keeps its data in that driver's storage, which another driver can't use.
func keepsDriver(w domain.Workload) bool {
	return w.MachineID != uuid.Nil && w.Kind != domain.KindEndpoint && w.Driver != ""
}

// driverFor picks the first of s's drivers (in its agent's order) that runs w's kind on g's
// accelerator with isolation the trust matrix allows for w's tenant on this machine. A workload
// that keeps its driver gets only that one.
func (s *slot) driverFor(w domain.Workload, g catalog.GPUClass) (string, bool) {
	img, ok := s.cat.Image(w.ImageID)
	if !ok {
		return "", false
	}
	for _, d := range s.drivers {
		if keepsDriver(w) && d.Name != w.Driver {
			continue
		}
		spec, ok := domain.LookupDriver(d.Name)
		if !ok || !slices.Contains(spec.Kinds, w.Kind) || !slices.Contains(spec.Accelerators, g.Accelerator) ||
			!slices.Contains(d.Accelerators, g.Accelerator) {
			continue
		}
		if _, ok := img.LaunchRuntime(g.Accelerator, d.Name); !ok {
			continue
		}
		if s.m.CheckPlacement(w, d.Name) == nil {
			return d.Name, true
		}
	}
	return "", false
}

// choose finds where w can run now on class g. If it can't, it says why, from a fixed vocabulary,
// and how many units are free on the machines it could use (for trusted tenants' timelines).
func (p *pool) choose(w domain.Workload, g catalog.GPUClass) (placement, int, string, bool) {
	if p.classes != nil {
		free := max(p.classes[g.ID], 0)
		if free < w.GPUCount {
			return placement{}, free, "", false
		}
		return placement{class: g.ID}, free, "", true
	}
	candidates := p.candidates(w)
	diskBound := w.MachineID != uuid.Nil && w.Kind != domain.KindEndpoint
	if diskBound && len(candidates) == 0 {
		return placement{}, 0, "waiting for its machine to come online: its disk is there", false
	}
	free, eligible, driverFound := 0, false, false
	for _, s := range candidates {
		driver, ok := s.driverFor(w, g)
		driverFound = driverFound || ok
		if !ok || !s.offers(g) {
			continue
		}
		eligible = true
		n := s.fits(g, driver)
		free += n
		if n >= w.GPUCount {
			return placement{class: g.ID, machineID: s.m.ID, machine: s.m.Name, driver: driver}, n, "", true
		}
	}
	switch {
	case !eligible && keepsDriver(w) && !driverFound:
		return placement{}, 0, fmt.Sprintf("waiting for its machine to offer the %s driver again: its disk is there", w.Driver), false
	case !eligible && diskBound:
		return placement{}, 0, fmt.Sprintf("waiting for its machine to offer %s again: its disk is there", g.ID), false
	case !eligible && len(candidates) > 0 && !driverFound:
		return placement{}, 0, fmt.Sprintf("waiting for a machine with a driver that runs %s on %s: none here has one yet", plural(w.Kind), g.ID), false
	case !eligible:
		return placement{}, 0, fmt.Sprintf("waiting for a machine that can run %s to come online", g.ID), false
	}
	return placement{}, free, "", false
}

// plural names a kind of workload in a reason.
func plural(k domain.WorkloadKind) string {
	if k == domain.KindVM {
		return "VMs"
	}
	return string(k) + "s"
}

// candidates are the machines w may be placed on. A VM or notebook that has run before goes back to
// the machine that holds its disk. Until sharing gives users a choice (ADR-0014, Phase 4), that's
// only the tenant's own machines: a workload never lands on someone else's machine, where the host
// could see its data, without its user saying so (rule 10).
func (p *pool) candidates(w domain.Workload) []*slot {
	var out []*slot
	for _, s := range p.machines {
		if w.MachineID != uuid.Nil && w.Kind != domain.KindEndpoint && s.m.ID != w.MachineID {
			continue
		}
		if s.m.TenantID == w.TenantID && s.m.CanHost(w) == nil {
			out = append(out, s)
		}
	}
	slices.SortStableFunc(out, func(a, b *slot) int { return cmp.Compare(a.m.Name, b.m.Name) })
	return out
}

// take records that w now holds pl's capacity.
func (p *pool) take(pl placement, w domain.Workload) {
	if p.classes != nil {
		p.classes[pl.class] -= w.GPUCount
		return
	}
	g, ok := p.cat.GPUClass(pl.class)
	if !ok {
		return
	}
	for _, s := range p.machines {
		if s.m.ID == pl.machineID {
			s.take(g, w.GPUCount, pl.driver)
		}
	}
}
