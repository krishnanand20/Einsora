package controller

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/krishnanand20/tensorafi/controlplane/internal/cluster"
	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
	"github.com/krishnanand20/tensorafi/controlplane/internal/testutil"
)

func TestClusterPlacementCountsWhatsHeld(t *testing.T) {
	cat := testutil.Catalog(t)
	l40s, _ := cat.GPUClass("l40s-48gb")
	held := domain.Workload{GPUClass: "a100-80gb", PlacedClass: "l40s-48gb", GPUCount: 3, State: domain.StateRunning}
	p := newPool(cat, cluster.Capacity{Classes: map[string]int{"l40s-48gb": 4}}, nil, []domain.Workload{held})

	w := domain.Workload{Kind: domain.KindNotebook, ImageID: "pytorch-notebook", GPUClass: "l40s-48gb", GPUCount: 2}
	if _, free, _, ok := p.choose(w, l40s); ok || free != 1 {
		t.Fatalf("2 of 4 with 3 held: ok=%v free=%d; want no room, 1 free", ok, free)
	}
	w.GPUCount = 1
	pl, _, _, ok := p.choose(w, l40s)
	if !ok || pl.class != "l40s-48gb" || pl.machineID != uuid.Nil || pl.driver != "" {
		t.Fatalf("1 of 4 with 3 held: %+v, %v", pl, ok)
	}
	p.take(pl, w)
	if _, _, _, ok := p.choose(w, l40s); ok {
		t.Error("taken capacity was offered again")
	}
}

func TestMissingNVIDIAToolkitDoesNotSilentlyPlaceGPUWorkOnCPU(t *testing.T) {
	cat := testutil.Catalog(t)
	f := machines()
	f.pc.TenantID = f.owner
	f.capacity.Machines = f.capacity.Machines[1:]
	f.capacity.Machines[0].Drivers = []cluster.Driver{{Name: domain.DriverDocker, Accelerators: []domain.AcceleratorKind{domain.AcceleratorCPU}}}
	p := newPool(cat, f.capacity, []domain.Machine{f.pc}, nil)
	w := domain.Workload{ID: uuid.New(), TenantID: f.owner, Region: domain.RegionLocal, Kind: domain.KindNotebook, ImageID: "pytorch-notebook", GPUClass: "l40s-48gb", GPUCount: 1}
	gpu, _ := cat.GPUClass(w.GPUClass)
	if _, _, why, ok := p.choose(w, gpu); ok || !strings.Contains(why, "driver") {
		t.Errorf("GPU request fell back silently: %v %q", ok, why)
	}
	w.GPUClass = "cpu-4c-16g"
	cpu, _ := cat.GPUClass(w.GPUClass)
	if placement, _, _, ok := p.choose(w, cpu); !ok || placement.class != w.GPUClass || placement.driver != domain.DriverDocker {
		t.Error("explicit CPU request cannot use CPU-only Docker")
	}
}

type fixture struct {
	owner, guest uuid.UUID
	mac, pc      domain.Machine
	capacity     cluster.Capacity
}

// machines: the owner's Mac (36 GB, macos-native, macos-container, and macos-vz) and a guest-visible Linux PC owned
// by someone else in a community pool (two L40S, docker and libvirt).
func machines() fixture {
	f := fixture{owner: uuid.New(), guest: uuid.New()}
	f.mac = domain.Machine{ID: uuid.New(), TenantID: f.owner, Name: "studio", Region: domain.RegionLocal, Sharing: domain.SharingPrivate}
	f.pc = domain.Machine{ID: uuid.New(), TenantID: uuid.New(), Name: "box", Region: domain.RegionLocal, Sharing: domain.SharingCommunity}
	f.capacity = cluster.Capacity{Machines: []cluster.MachineCapacity{
		{ID: f.mac.ID, Devices: map[string]int{"apple-silicon-32gb": 1}, VCPUs: 9, MemoryMiB: 27 * 1024, Drivers: []cluster.Driver{
			{Name: domain.DriverMacOSNative, Accelerators: []domain.AcceleratorKind{domain.AcceleratorApple, domain.AcceleratorCPU}},
			{Name: domain.DriverMacOSContainer, Accelerators: []domain.AcceleratorKind{domain.AcceleratorCPU}},
			{Name: domain.DriverMacOSVZ, Accelerators: []domain.AcceleratorKind{domain.AcceleratorApple, domain.AcceleratorCPU}},
		}},
		{ID: f.pc.ID, Devices: map[string]int{"l40s-48gb": 2}, VCPUs: 30, MemoryMiB: 192 * 1024, Drivers: []cluster.Driver{
			{Name: domain.DriverDocker, Accelerators: []domain.AcceleratorKind{domain.AcceleratorNVIDIA, domain.AcceleratorCPU}},
			{Name: domain.DriverLibvirt, Accelerators: []domain.AcceleratorKind{domain.AcceleratorNVIDIA, domain.AcceleratorCPU}},
		}},
	}}
	return f
}

func TestMachinePlacementFollowsTheTrustMatrix(t *testing.T) {
	cat := testutil.Catalog(t)
	f := machines()
	tests := []struct {
		name          string
		w             domain.Workload
		class         string
		wantMachine   uuid.UUID
		wantDriver    string
		wantWhyPrefix string
	}{
		{"the owner's notebook on their Mac runs natively", domain.Workload{TenantID: f.owner, Kind: domain.KindNotebook, ImageID: "pytorch-notebook", GPUCount: 1},
			"apple-silicon-32gb", f.mac.ID, domain.DriverMacOSNative, ""},
		{"the owner's endpoint on their Mac runs natively", domain.Workload{TenantID: f.owner, Kind: domain.KindEndpoint, ImageID: "openai-endpoint", GPUCount: 1},
			"apple-silicon-32gb", f.mac.ID, domain.DriverMacOSNative, ""},
		{"a guest can't use someone's private Mac", domain.Workload{TenantID: f.guest, Kind: domain.KindVM, ImageID: "ubuntu-24.04-cuda", GPUCount: 1},
			"apple-silicon-32gb", uuid.Nil, "", "waiting for a machine that can run apple-silicon-32gb"},
		{"a guest's notebook on a community PC can't use plain docker", domain.Workload{TenantID: f.guest, Kind: domain.KindNotebook, ImageID: "pytorch-notebook", GPUCount: 1},
			"l40s-48gb", uuid.Nil, "", "waiting for a machine that can run l40s-48gb"},
		{"a guest isn't placed on someone else's community PC without choosing to (Phase 4)",
			domain.Workload{TenantID: f.guest, Kind: domain.KindVM, ImageID: "ubuntu-24.04-cuda", GPUCount: 2},
			"l40s-48gb", uuid.Nil, "", "waiting for a machine that can run l40s-48gb"},
		{"the owner's CPU notebook fits by CPUs and memory", domain.Workload{TenantID: f.owner, Kind: domain.KindNotebook, ImageID: "pytorch-notebook", GPUCount: 1},
			"cpu-4c-16g", f.mac.ID, domain.DriverMacOSNative, ""},
		{"no driver here runs the owner's CPU VM, and the reason says so, not that the Mac is offline",
			domain.Workload{TenantID: f.owner, Kind: domain.KindVM, ImageID: "ubuntu-24.04-cuda", GPUCount: 1},
			"cpu-4c-16g", uuid.Nil, "", "waiting for a machine with a driver that runs VMs on cpu-4c-16g: none here has one yet"},
	}
	for _, tt := range tests {
		p := newPool(cat, f.capacity, []domain.Machine{f.mac, f.pc}, nil)
		tt.w.ID, tt.w.Region, tt.w.GPUClass = uuid.New(), domain.RegionLocal, tt.class
		g, _ := cat.GPUClass(tt.class)
		pl, _, why, ok := p.choose(tt.w, g)
		if pl.machineID != tt.wantMachine || pl.driver != tt.wantDriver || ok != (tt.wantMachine != uuid.Nil) ||
			(tt.wantWhyPrefix != "" && (len(why) < len(tt.wantWhyPrefix) || why[:len(tt.wantWhyPrefix)] != tt.wantWhyPrefix)) {
			t.Errorf("%s: placed %+v ok=%v why=%q; want machine %v driver %q why %q…", tt.name, pl, ok, why, tt.wantMachine, tt.wantDriver, tt.wantWhyPrefix)
		}
	}
}

func TestMachinePlacementCountsResources(t *testing.T) {
	cat := testutil.Catalog(t)
	f := machines()
	apple, _ := cat.GPUClass("apple-silicon-32gb")
	cpu, _ := cat.GPUClass("cpu-4c-16g")
	running := domain.Workload{TenantID: f.owner, MachineID: f.mac.ID, GPUClass: "apple-silicon-32gb", PlacedClass: "apple-silicon-32gb",
		GPUCount: 1, State: domain.StateRunning}
	p := newPool(cat, f.capacity, []domain.Machine{f.mac, f.pc}, []domain.Workload{running})

	w := domain.Workload{ID: uuid.New(), TenantID: f.owner, Region: domain.RegionLocal, Kind: domain.KindNotebook, ImageID: "pytorch-notebook", GPUClass: "apple-silicon-32gb", GPUCount: 1}
	if _, free, why, ok := p.choose(w, apple); ok || free != 0 || why != "" {
		t.Errorf("the Mac's GPU is taken: ok=%v free=%d why=%q; want a capacity wait with 0 free", ok, free, why)
	}
	// The running Apple workload holds 24 of the Mac's 27 GiB, so a 16 GiB CPU notebook waits too.
	w.GPUClass = "cpu-4c-16g"
	if _, _, _, ok := p.choose(w, cpu); ok {
		t.Error("a CPU notebook was placed in memory the GPU workload holds")
	}
}

// A notebook or VM that has run before goes back to the machine holding its disk, and waits for
// it, saying so; an endpoint has no disk, so it goes wherever there's room.
func TestStoppedWorkloadsReturnToTheirDisk(t *testing.T) {
	cat := testutil.Catalog(t)
	f := machines()
	l40s, _ := cat.GPUClass("l40s-48gb")
	gone := uuid.New() // a machine of the guest's that's offline
	offline := domain.Machine{ID: gone, TenantID: f.guest, Name: "laptop", Region: domain.RegionLocal, Sharing: domain.SharingPrivate}
	p := newPool(cat, f.capacity, []domain.Machine{f.mac, f.pc, offline}, nil)

	vm := domain.Workload{ID: uuid.New(), TenantID: f.guest, Region: domain.RegionLocal, Kind: domain.KindVM, ImageID: "ubuntu-24.04-cuda",
		GPUClass: "l40s-48gb", GPUCount: 1, MachineID: gone}
	if _, _, why, ok := p.choose(vm, l40s); ok || why != "waiting for its machine to come online: its disk is there" {
		t.Errorf("a VM whose disk is on an offline machine: ok=%v why=%q", ok, why)
	}
	ep := vm
	ep.Kind = domain.KindEndpoint
	// Endpoints on the community PC need strong isolation, which it has only for VMs: no driver.
	if _, _, why, ok := p.choose(ep, l40s); ok || why == "waiting for its machine to come online: its disk is there" {
		t.Errorf("an endpoint isn't tied to a machine: ok=%v why=%q", ok, why)
	}
}

// Drivers follow the trust matrix: on someone else's machine (once sharing lets a user choose
// one), only a driver with strong isolation.
func TestDriverChoiceFollowsTheTrustMatrix(t *testing.T) {
	cat := testutil.Catalog(t)
	f := machines()
	l40s, _ := cat.GPUClass("l40s-48gb")
	p := newPool(cat, f.capacity, []domain.Machine{f.mac, f.pc}, nil)
	var pc *slot
	for _, s := range p.machines {
		if s.m.ID == f.pc.ID {
			pc = s
		}
	}
	guestVM := domain.Workload{TenantID: f.guest, Region: domain.RegionLocal, Kind: domain.KindVM, ImageID: "ubuntu-24.04-cuda"}
	if d, ok := pc.driverFor(guestVM, l40s); !ok || d != domain.DriverLibvirt {
		t.Errorf("a guest's VM: %q, %v; want libvirt", d, ok)
	}
	guestNotebook := guestVM
	guestNotebook.Kind, guestNotebook.ImageID = domain.KindNotebook, "pytorch-notebook"
	if d, ok := pc.driverFor(guestNotebook, l40s); ok {
		t.Errorf("a guest's notebook got %q; plain docker is weak isolation", d)
	}
	ownersNotebook := guestNotebook
	ownersNotebook.TenantID = f.pc.TenantID
	if d, ok := pc.driverFor(ownersNotebook, l40s); !ok || d != domain.DriverDocker {
		t.Errorf("the owner's notebook: %q, %v; want docker", d, ok)
	}
}

// A VM or notebook that has run keeps its driver: its data is in that driver's storage.
func TestDriversStickToTheirData(t *testing.T) {
	cat := testutil.Catalog(t)
	f := machines()
	apple, _ := cat.GPUClass("apple-silicon-32gb")
	native := f.capacity.Machines[0].Drivers[0]
	sim := cluster.Driver{Name: domain.DriverSim, Accelerators: []domain.AcceleratorKind{domain.AcceleratorApple}}
	f.capacity.Machines[0].Drivers = []cluster.Driver{native, sim}
	nb := domain.Workload{ID: uuid.New(), TenantID: f.owner, Region: domain.RegionLocal, Kind: domain.KindNotebook, ImageID: "pytorch-notebook", GPUClass: apple.ID,
		GPUCount: 1, MachineID: f.mac.ID, Driver: domain.DriverSim}
	p := newPool(cat, f.capacity, []domain.Machine{f.mac}, nil)
	if pl, _, _, ok := p.choose(nb, apple); !ok || pl.driver != domain.DriverSim {
		t.Errorf("placed with %q; want the driver holding its data, not the agent's first choice", pl.driver)
	}
	f.capacity.Machines[0].Drivers = []cluster.Driver{native}
	p = newPool(cat, f.capacity, []domain.Machine{f.mac}, nil)
	if _, _, why, ok := p.choose(nb, apple); ok || why != "waiting for its machine to offer the sim driver again: its disk is there" {
		t.Errorf("without its driver: ok=%v why=%q", ok, why)
	}
	fresh := nb
	fresh.ID, fresh.MachineID, fresh.Driver = uuid.New(), uuid.Nil, ""
	if pl, _, _, ok := p.choose(fresh, apple); !ok || pl.driver != domain.DriverMacOSNative {
		t.Errorf("a new notebook: %+v; want the agent's first suitable driver", pl)
	}
}

// The macOS licence allows two macOS VMs per Mac.
func TestDriverLimitsPerMachine(t *testing.T) {
	cat := testutil.Catalog(t)
	apple, _ := cat.GPUClass("apple-silicon-32gb")
	s := &slot{devices: map[string]int{apple.ID: 4}, vcpus: 64, memMiB: 512 * 1024, running: map[string]int{domain.DriverMacOSVZ: 2}}
	if n := s.fits(apple, domain.DriverMacOSVZ); n != 0 {
		t.Errorf("a third macOS VM fits %d; want 0", n)
	}
	if n := s.fits(apple, domain.DriverSim); n != 4 {
		t.Errorf("another driver fits %d; want 4", n)
	}
}

// A machine the backend reports but the store doesn't list in this region is never used: a
// machine's region comes from its registration, not its agent.
func TestUnknownMachinesAreIgnored(t *testing.T) {
	cat := testutil.Catalog(t)
	f := machines()
	apple, _ := cat.GPUClass("apple-silicon-32gb")
	p := newPool(cat, f.capacity, []domain.Machine{f.pc}, nil) // the Mac isn't in this region
	w := domain.Workload{ID: uuid.New(), TenantID: f.owner, Region: domain.RegionLocal, Kind: domain.KindNotebook, ImageID: "pytorch-notebook", GPUClass: "apple-silicon-32gb", GPUCount: 1}
	if _, _, _, ok := p.choose(w, apple); ok {
		t.Error("placed on a machine outside the store's region")
	}
}

// A workload holding capacity whose class left the catalog is counted as holding everything on its
// machine: what it holds can't be known, and guessing low would put someone else on its GPU.
func TestUnknownClassesHoldTheirMachine(t *testing.T) {
	cat := testutil.Catalog(t)
	f := machines()
	apple, _ := cat.GPUClass("apple-silicon-32gb")
	retired := domain.Workload{TenantID: f.owner, MachineID: f.mac.ID, GPUClass: "retired-class", PlacedClass: "retired-class",
		GPUCount: 1, State: domain.StateRunning, Driver: domain.DriverMacOSNative}
	p := newPool(cat, f.capacity, []domain.Machine{f.mac}, []domain.Workload{retired})
	w := domain.Workload{ID: uuid.New(), TenantID: f.owner, Region: domain.RegionLocal, Kind: domain.KindNotebook, ImageID: "pytorch-notebook", GPUClass: apple.ID, GPUCount: 1}
	if _, _, _, ok := p.choose(w, apple); ok {
		t.Error("placed beside a workload whose size is unknown")
	}
}

// A VM or notebook whose machine still has its driver but no longer offers its class says so.
func TestWaitingForAClassOnItsMachine(t *testing.T) {
	cat := testutil.Catalog(t)
	f := machines()
	apple, _ := cat.GPUClass("apple-silicon-32gb")
	f.capacity.Machines[0].Devices = map[string]int{} // the Mac's GPU isn't reported this time
	p := newPool(cat, f.capacity, []domain.Machine{f.mac}, nil)
	nb := domain.Workload{ID: uuid.New(), TenantID: f.owner, Region: domain.RegionLocal, Kind: domain.KindNotebook, ImageID: "pytorch-notebook", GPUClass: apple.ID,
		GPUCount: 1, MachineID: f.mac.ID, Driver: domain.DriverMacOSNative}
	if _, _, why, ok := p.choose(nb, apple); ok || why != "waiting for its machine to offer apple-silicon-32gb again: its disk is there" {
		t.Errorf("ok=%v why=%q", ok, why)
	}
}

func TestNativeCPUReservesMetalInBothAdmissionOrders(t *testing.T) {
	cat := testutil.Catalog(t)
	for _, cpuFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "CPU first", false: "Metal first"}[cpuFirst], func(t *testing.T) {
			f := machines()
			f.capacity.Machines = f.capacity.Machines[:1]
			f.capacity.Machines[0].VCPUs = 64
			f.capacity.Machines[0].MemoryMiB = 128 * 1024
			cpu, _ := cat.GPUClass("cpu-2c-8g")
			apple, _ := cat.GPUClass("apple-silicon-32gb")
			w := domain.Workload{ID: uuid.New(), TenantID: f.owner, Region: domain.RegionLocal, Kind: domain.KindNotebook, ImageID: "pytorch-notebook", GPUCount: 1}
			first, second := cpu, apple
			if !cpuFirst {
				first, second = apple, cpu
			}
			p := newPool(cat, f.capacity, []domain.Machine{f.mac}, nil)
			pl, _, _, ok := p.choose(w, first)
			if !ok || pl.driver != domain.DriverMacOSNative {
				t.Fatal("first workload did not select native runtime")
			}
			p.take(pl, w)
			if _, _, _, ok := p.choose(w, second); ok {
				t.Fatal("CPU-native workload and Apple GPU allocation overlapped")
			}
			// The same reservation is reconstructed after a controller restart.
			w.MachineID, w.Driver, w.PlacedClass = f.mac.ID, pl.driver, first.ID
			p = newPool(cat, f.capacity, []domain.Machine{f.mac}, []domain.Workload{w})
			if _, _, _, ok := p.choose(w, second); ok {
				t.Fatal("restart lost native CPU's Metal reservation")
			}
		})
	}
}

func TestImageWithoutRuntimeForOfferedDriverWaits(t *testing.T) {
	cat := testutil.Catalog(t)
	f := machines()
	f.capacity.Machines = f.capacity.Machines[:1]
	f.capacity.Machines[0].Drivers = f.capacity.Machines[0].Drivers[1:2]
	p := newPool(cat, f.capacity, []domain.Machine{f.mac}, nil)
	w := domain.Workload{TenantID: f.owner, Region: domain.RegionLocal, Kind: domain.KindNotebook, ImageID: "pytorch-notebook", GPUCount: 1}
	cpu, _ := cat.GPUClass("cpu-2c-8g")
	if _, _, why, ok := p.choose(w, cpu); ok || !strings.Contains(why, "driver") {
		t.Fatalf("incompatible runtime was placed: %v %q", ok, why)
	}
}
