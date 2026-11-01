package domain

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRegionIDs(t *testing.T) {
	for r, ok := range map[Region]bool{"local": true, "in-mum-1": true, "eu-fra-1": true, "": false, "EU": false, "us east": false, "-x": false} {
		if r.Valid() != ok {
			t.Errorf("Region(%q).Valid() = %v, want %v", r, !ok, ok)
		}
	}
}

func TestMachineValidate(t *testing.T) {
	good := Machine{
		ID: uuid.New(), TenantID: uuid.New(), Name: "studio-mac", Region: RegionLocal, OS: "darwin", Arch: "arm64",
		Accelerators: []Accelerator{{Kind: AcceleratorApple, Model: "Apple M3 Max", Count: 1, MemoryMiB: 131072}},
		Sharing:      SharingPrivate,
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid machine: %v", err)
	}
	tests := map[string]func(*Machine){
		"no region":         func(m *Machine) { m.Region = "" }, // a machine belongs to exactly one region
		"bad region":        func(m *Machine) { m.Region = "Mumbai 1" },
		"windows":           func(m *Machine) { m.OS = "windows" },
		"32-bit":            func(m *Machine) { m.Arch = "386" },
		"unknown sharing":   func(m *Machine) { m.Sharing = "public" },
		"unknown device":    func(m *Machine) { m.Accelerators = []Accelerator{{Kind: "tpu", Count: 1}} },
		"zero devices":      func(m *Machine) { m.Accelerators = []Accelerator{{Kind: AcceleratorNVIDIA, Count: 0}} },
		"name with a space": func(m *Machine) { m.Name = "my mac" },
		"escape in model":   func(m *Machine) { m.Accelerators[0].Model = "M3\x1b[2J" },
		"huge model text":   func(m *Machine) { m.Accelerators[0].Model = strings.Repeat("x", 101) },
	}
	for name, mutate := range tests {
		m := good
		m.Accelerators = slices.Clone(good.Accelerators)
		mutate(&m)
		if code, _ := CodeOf(m.Validate()); code != CodeInvalid {
			t.Errorf("%s: Validate() code = %q, want %q", name, code, CodeInvalid)
		}
	}
}

// Capacity is counted and usage accounted for what actually runs, so nothing is provisioning or
// running before it's placed (ADR-0010).
func TestWorkloadsArePlacedBeforeTheyHoldGPUs(t *testing.T) {
	w := Workload{ID: uuid.New(), GPUClass: "l40s-48gb"}
	for _, s := range AllStates() {
		needsPlacement := s == StateProvisioning || s.Metered()
		if err := w.CheckPlaced(s); (err != nil) != needsPlacement {
			t.Errorf("%s: CheckPlaced = %v, want an error: %v", s, err, needsPlacement)
		}
	}
	w.PlacedClass = "l40s-48gb"
	if err := w.CheckPlaced(StateRunning); err != nil {
		t.Errorf("placed workload: %v", err)
	}
}

func TestMachineCanHost(t *testing.T) {
	owner, other := uuid.New(), uuid.New()
	m := Machine{Name: "box", TenantID: owner, Region: RegionLocal, Sharing: SharingPrivate}
	tests := []struct {
		name    string
		sharing SharingLevel
		tenant  uuid.UUID
		region  Region
		ok      bool
	}{
		{"owner, same region", SharingPrivate, owner, RegionLocal, true},
		{"owner, other region", SharingPrivate, owner, "eu-fra-1", false}, // rule 9
		{"stranger, private", SharingPrivate, other, RegionLocal, false},
		{"stranger, invite-only (no invite lists yet)", SharingInvite, other, RegionLocal, false},
		{"stranger, community pool", SharingCommunity, other, RegionLocal, true},
		{"stranger, community pool, other region", SharingCommunity, other, "eu-fra-1", false},
	}
	for _, tt := range tests {
		m.Sharing = tt.sharing
		err := m.CanHost(Workload{TenantID: tt.tenant, Region: tt.region})
		if (err == nil) != tt.ok {
			t.Errorf("%s: CanHost = %v, want ok=%v", tt.name, err, tt.ok)
		}
	}
}

func TestNormalizeEmail(t *testing.T) {
	for in, want := range map[string]string{
		"  Me@Example.COM ": "me@example.com",
		"me@example":        "",
		"@example.com":      "",
		"me@@example.com":   "",
		"me @example.com":   "",
		"me@exa\nmple.com":  "",
	} {
		got, ok := NormalizeEmail(in)
		if got != want || ok != (want != "") {
			t.Errorf("NormalizeEmail(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

// Weak isolation (native macOS, plain containers) is only for the machine owner's own workloads;
// anyone else's need a VM or a sandboxed container. Unknown drivers are refused.
func TestPermits(t *testing.T) {
	owner, guest := uuid.New(), uuid.New()
	m := Machine{Name: "mac", TenantID: owner, Region: RegionLocal, Sharing: SharingCommunity}
	for _, tt := range []struct {
		tenant uuid.UUID
		driver string
		want   Code
	}{
		{owner, DriverMacOSNative, ""},
		{owner, DriverDocker, ""},
		{guest, DriverMacOSNative, CodeForbidden},
		{guest, DriverDocker, CodeForbidden},
		{guest, DriverSim, CodeForbidden},
		{guest, DriverMacOSVZ, ""},
		{guest, DriverLibvirt, ""},
		{guest, DriverMacOSContainer, ""},
		{owner, "made-up", CodeInvalid},
	} {
		code, _ := CodeOf(m.Permits(Workload{TenantID: tt.tenant}, tt.driver))
		if code != tt.want {
			t.Errorf("tenant owner=%v, driver %s: code %q, want %q", tt.tenant == owner, tt.driver, code, tt.want)
		}
	}
}

func TestMachineOnline(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name string
		seen time.Time
		want bool
	}{
		{"never seen", time.Time{}, false},
		{"seen a minute ago (a heartbeat)", now.Add(-time.Minute), true},
		{"seen just inside the window", now.Add(-MachineSeenWithin + time.Second), true},
		{"seen just outside it", now.Add(-MachineSeenWithin), false},
	} {
		if got := (Machine{LastSeenAt: tt.seen}).Online(now); got != tt.want {
			t.Errorf("%s: online = %v; want %v", tt.name, got, tt.want)
		}
	}
}
