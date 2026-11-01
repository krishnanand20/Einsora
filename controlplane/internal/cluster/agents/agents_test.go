package agents_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tensora/tensora/controlplane/internal/agent/protocol"
	"github.com/tensora/tensora/controlplane/internal/cluster"
	"github.com/tensora/tensora/controlplane/internal/cluster/agents"
	"github.com/tensora/tensora/controlplane/internal/domain"
	"github.com/tensora/tensora/controlplane/internal/store"
	"github.com/tensora/tensora/controlplane/internal/testutil"
)

type fixture struct {
	st      store.Store
	hub     *agents.Hub
	machine domain.Machine
	owner   domain.Tenant
	guest   domain.Tenant
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	st := testutil.SeededStore(t)
	ctx := context.Background()
	m, err := agents.RegisterLocal(ctx, st, uuid.Nil, "internal", "studio", "darwin", "arm64", domain.RegionLocal, testutil.T0)
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{st: st, machine: m}
	_ = st.InTx(ctx, func(q store.Queries) error {
		f.owner, _ = q.TenantBySlug(ctx, "internal")
		f.guest, _ = q.TenantBySlug(ctx, "acme")
		return nil
	})
	f.hub = agents.New(st, testutil.Catalog(t), agents.Options{PollTimeout: 50 * time.Millisecond, AllowSim: true},
		slog.New(slog.DiscardHandler), nil)
	return f
}

// mac is a 36 GB Apple Silicon Mac's report with the sim driver.
func mac() *protocol.Inventory {
	return &protocol.Inventory{OS: "darwin", Arch: "arm64", CPUs: 11, MemoryMiB: 36 * 1024,
		Accelerators: []domain.Accelerator{{Kind: domain.AcceleratorApple, Model: "Apple M3 Pro", Count: 1, MemoryMiB: 36 * 1024}},
		UsableCPUs:   9, UsableMemoryMiB: 27 * 1024,
		Drivers: []protocol.Driver{{Name: domain.DriverSim, Accelerators: []domain.AcceleratorKind{domain.AcceleratorApple, domain.AcceleratorCPU}}}}
}

// place stores a workload placed on the fixture's machine, in state.
func (f fixture) place(t *testing.T, tenant domain.Tenant, kind domain.WorkloadKind, class string, state domain.State) domain.Workload {
	t.Helper()
	return f.placeOn(t, f.machine, tenant, kind, class, state)
}

func (f fixture) placeOn(t *testing.T, m domain.Machine, tenant domain.Tenant, kind domain.WorkloadKind, class string, state domain.State) domain.Workload {
	t.Helper()
	ctx := context.Background()
	image := map[domain.WorkloadKind]string{domain.KindNotebook: "pytorch-notebook", domain.KindEndpoint: "openai-endpoint", domain.KindVM: "ubuntu-24.04-cuda"}[kind]
	w := domain.Workload{ID: uuid.New(), TenantID: tenant.ID, Name: "w", Kind: kind, State: domain.StatePending, ImageID: image,
		GPUClass: class, GPUCount: 1, Params: map[string]string{domain.ParamModelURI: "hf://Qwen/Qwen2.5-0.5B-Instruct"},
		CreatedBy: "test", CreatedAt: testutil.T0, UpdatedAt: testutil.T0, Region: domain.RegionLocal}
	err := f.st.InTx(ctx, func(q store.Queries) error {
		if err := q.CreateWorkload(ctx, w); err != nil {
			return err
		}
		driver := domain.DriverSim
		if tenant.ID != m.TenantID {
			driver = domain.DriverMacOSVZ
		}
		if err := q.SetPlacement(ctx, w.ID, class, m.ID, driver); err != nil {
			return err
		}
		w.PlacedClass, w.MachineID, w.Driver = class, m.ID, driver
		w.Attempt++ // as SetPlacement counts it
		if state != domain.StatePending {
			w.State = state
			return q.UpdateWorkloadState(ctx, w.ID, domain.StatePending, state, testutil.T0)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestSyncSendsDesiredStateFromTheStore(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	running := f.place(t, f.owner, domain.KindNotebook, "apple-silicon-32gb", domain.StateProvisioning)
	stopped := f.place(t, f.owner, domain.KindNotebook, "cpu-2c-8g", domain.StateStopping)
	stoppedEndpoint := f.place(t, f.owner, domain.KindEndpoint, "apple-silicon-32gb", domain.StateStopping)
	deleting := f.place(t, f.owner, domain.KindNotebook, "cpu-2c-8g", domain.StateDeleting)

	resp, err := f.hub.Conn(f.machine.ID).Sync(ctx, protocol.SyncRequest{Version: protocol.Version, Inventory: mac()})
	if err != nil {
		t.Fatal(err)
	}
	modes := map[uuid.UUID]protocol.Desired{}
	for _, d := range resp.Workloads {
		modes[d.ID] = d
	}
	for id, want := range map[uuid.UUID]protocol.Mode{
		running.ID: protocol.ModeRun, stopped.ID: protocol.ModePark, stoppedEndpoint.ID: protocol.ModeRemove, deleting.ID: protocol.ModeRemove,
	} {
		if got := modes[id].Mode; got != want {
			t.Errorf("workload %s: mode %q, want %q", id, got, want)
		}
	}
	d := modes[running.ID]
	if d.Driver != domain.DriverSim || d.Accelerator != domain.AcceleratorApple || d.Devices != 1 || d.VCPUs != 6 ||
		d.MemoryMiB != 24*1024 || d.Runtime.Ref != "jupyterlab-pytorch-mps" || d.StrongIsolation {
		t.Errorf("desired = %+v", d)
	}
	if resp.SendInventory {
		t.Error("the inventory was sent; the hub mustn't ask again")
	}

	// The inventory is recorded on the machine, never its owner, name, or region.
	_ = f.st.InTx(ctx, func(q store.Queries) error {
		m, err := q.MachineByID(ctx, f.machine.ID)
		if err != nil || m.CPUs != 11 || m.MemoryMiB != 36*1024 || len(m.Accelerators) != 1 || m.LastSeenAt.IsZero() ||
			m.TenantID != f.machine.TenantID || m.Region != domain.RegionLocal {
			t.Errorf("machine after the first sync = %+v, %v", m, err)
		}
		return nil
	})
}

// A workload that isn't the machine owner's is marked as needing strong isolation, so the agent
// checks the driver too.
func TestGuestsNeedStrongIsolation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	pool := domain.Machine{ID: uuid.New(), TenantID: f.owner.ID, Name: "pool-mac", Region: domain.RegionLocal, OS: "darwin",
		Arch: "arm64", Sharing: domain.SharingCommunity, CreatedAt: testutil.T0}
	if err := f.st.InTx(ctx, func(q store.Queries) error { return q.CreateMachine(ctx, pool) }); err != nil {
		t.Fatal(err)
	}
	guests := f.placeOn(t, pool, f.guest, domain.KindVM, "apple-silicon-32gb", domain.StateProvisioning)
	owners := f.placeOn(t, pool, f.owner, domain.KindNotebook, "cpu-2c-8g", domain.StateProvisioning)
	resp, err := f.hub.Conn(pool.ID).Sync(ctx, protocol.SyncRequest{Version: protocol.Version, Inventory: mac()})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range resp.Workloads {
		if want := d.ID == guests.ID; d.StrongIsolation != want || (d.ID != guests.ID && d.ID != owners.ID) {
			t.Errorf("workload %s: strong isolation %v, want %v", d.ID, d.StrongIsolation, want)
		}
	}
}

func TestInventoryIsCheckedAndDriversFiltered(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	bad := mac()
	bad.UsableMemoryMiB = bad.MemoryMiB + 1
	if _, err := f.hub.Conn(f.machine.ID).Sync(ctx, protocol.SyncRequest{Version: protocol.Version, Inventory: bad}); err != nil {
		t.Fatal(err)
	}
	if c, _ := f.hub.Capacity(ctx); len(c.Machines) != 0 {
		t.Error("more usable memory than the machine has was accepted")
	}
	if _, err := f.hub.Conn(f.machine.ID).Sync(ctx, protocol.SyncRequest{Version: 99}); err == nil {
		t.Error("another protocol version was accepted")
	}

	noSim := agents.New(f.st, testutil.Catalog(t), agents.Options{PollTimeout: 10 * time.Millisecond}, slog.New(slog.DiscardHandler), nil)
	inv := mac()
	inv.Drivers = append(inv.Drivers, protocol.Driver{Name: "made-up", Accelerators: []domain.AcceleratorKind{domain.AcceleratorApple}},
		protocol.Driver{Name: domain.DriverMacOSNative, Accelerators: []domain.AcceleratorKind{domain.AcceleratorApple, domain.AcceleratorNVIDIA}})
	if _, err := noSim.Conn(f.machine.ID).Sync(ctx, protocol.SyncRequest{Version: protocol.Version, Inventory: inv}); err != nil {
		t.Fatal(err)
	}
	c, _ := noSim.Capacity(ctx)
	if len(c.Machines) != 1 || len(c.Machines[0].Drivers) != 1 || c.Machines[0].Drivers[0].Name != domain.DriverMacOSNative ||
		len(c.Machines[0].Drivers[0].Accelerators) != 1 {
		t.Fatalf("capacity = %+v; want only macos-native, with only the accelerators it can use", c.Machines)
	}
	if c.Classes != nil || c.Machines[0].Devices["apple-silicon-32gb"] != 1 || c.Machines[0].VCPUs != 9 {
		t.Errorf("capacity = %+v; want the Mac's GPU as apple-silicon-32gb and its usable CPUs", c)
	}
}

func TestMachinesInOtherRegionsAreRefused(t *testing.T) {
	f := newFixture(t)
	other := agents.New(f.st, testutil.Catalog(t), agents.Options{Region: "eu-fra-1"}, slog.New(slog.DiscardHandler), nil)
	_, err := other.Conn(f.machine.ID).Sync(context.Background(), protocol.SyncRequest{Version: protocol.Version, Inventory: mac()})
	if code, _ := domain.CodeOf(err); code != domain.CodeForbidden {
		t.Errorf("a machine syncing with another region's control plane: %v", err)
	}
}

// An up-to-date agent waits for a change, and a nudge answers it at once.
func TestSyncWaitsForAChange(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	hub := agents.New(f.st, testutil.Catalog(t), agents.Options{PollTimeout: 5 * time.Second, AllowSim: true}, slog.New(slog.DiscardHandler), nil)
	conn := hub.Conn(f.machine.ID)
	first, err := conn.Sync(ctx, protocol.SyncRequest{Version: protocol.Version, Inventory: mac()})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan protocol.SyncResponse)
	go func() {
		resp, _ := conn.Sync(ctx, protocol.SyncRequest{Version: protocol.Version, Generation: first.Generation})
		done <- resp
	}()
	select {
	case <-done:
		t.Fatal("an up-to-date agent was answered without a change")
	case <-time.After(100 * time.Millisecond):
	}
	w := f.place(t, f.owner, domain.KindNotebook, "cpu-2c-8g", domain.StateProvisioning)
	_ = hub.Run(ctx, cluster.Spec{Workload: w})
	select {
	case resp := <-done:
		if resp.Generation == first.Generation || len(resp.Workloads) != 1 {
			t.Errorf("after a nudge: %+v", resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a nudge didn't wake the waiting agent")
	}
}

func TestObserveSaysWhatTheAgentReported(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var ws []domain.Workload
	for range 4 {
		ws = append(ws, f.place(t, f.owner, domain.KindNotebook, "cpu-2c-8g", domain.StateProvisioning))
	}
	ready, failed, odd, missing := ws[0], ws[1], ws[2], ws[3]
	ref := func(w domain.Workload) cluster.Ref {
		return cluster.Ref{ID: w.ID, Kind: w.Kind, MachineID: f.machine.ID, State: w.State, Attempt: w.Attempt}
	}

	if obs, reason, _ := f.hub.Observe(ctx, ref(ready)); obs != cluster.ObservedUnknown || reason != "" {
		t.Errorf("before the machine syncs, just after a restart: %s %q; want unknown, with nothing to say yet", obs, reason)
	}
	conn := f.hub.Conn(f.machine.ID)
	first, err := conn.Sync(ctx, protocol.SyncRequest{Version: protocol.Version, Inventory: mac()})
	if err != nil {
		t.Fatal(err)
	}
	if obs, _, _ := f.hub.Observe(ctx, ref(missing)); obs != cluster.ObservedProgressing {
		t.Errorf("before the agent applied its run: %s; want progressing, not absent", obs)
	}
	_, err = conn.Sync(ctx, protocol.SyncRequest{Version: protocol.Version, Generation: first.Generation, Applied: first.Generation,
		Statuses: []protocol.Status{
			{ID: ready.ID, Attempt: 1, State: protocol.StateReady},
			{ID: failed.ID, Attempt: 1, State: protocol.StateFailed, Code: protocol.CodeOutOfMemory},
			{ID: odd.ID, Attempt: 1, State: "exploded", Code: "rm -rf / at t-acme"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		w      domain.Workload
		obs    cluster.Observed
		reason string
	}{
		{ready, cluster.ObservedReady, ""},
		{failed, cluster.ObservedFailed, "it ran out of memory"},
		{odd, cluster.ObservedFailed, "its machine reported a problem"},
		{missing, cluster.ObservedAbsent, ""},
	} {
		want := tt
		obs, reason, err := f.hub.Observe(ctx, ref(tt.w))
		if err != nil || obs != want.obs || reason != want.reason {
			t.Errorf("Observe = %s %q %v; want %s %q", obs, reason, err, want.obs, want.reason)
		}
	}
	// A report about what the store no longer expects isn't the answer: after the workload is
	// stopped, its "ready" doesn't answer the stop.
	stopping := ready
	stopping.State = domain.StateStopping
	if obs, _, _ := f.hub.Observe(ctx, ref(stopping)); obs != cluster.ObservedProgressing {
		t.Errorf("a report about running, read for a stop: %s; want progressing", obs)
	}
	if obs, _, _ := f.hub.Observe(ctx, cluster.Ref{ID: ready.ID}); obs != cluster.ObservedAbsent {
		t.Errorf("a workload never placed on a machine: %s; want absent", obs)
	}
	if got := protocol.Reason("rm -rf / at t-acme"); got != "its machine reported a problem" {
		t.Errorf("an unknown code's reason = %q; an agent's words never reach a timeline", got)
	}
}

// A machine's agent that stops syncing goes offline: it isn't offered for placement, and its
// workloads read as unknown, not absent.
func TestOfflineMachines(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	clock := testutil.NewClock(testutil.T0)
	hub := agents.New(f.st, testutil.Catalog(t), agents.Options{PollTimeout: time.Millisecond, OfflineAfter: time.Minute, AllowSim: true},
		slog.New(slog.DiscardHandler), clock.Now)
	if _, err := hub.Conn(f.machine.ID).Sync(ctx, protocol.SyncRequest{Version: protocol.Version, Inventory: mac()}); err != nil {
		t.Fatal(err)
	}
	if c, _ := hub.Capacity(ctx); len(c.Machines) != 1 {
		t.Fatalf("online: %+v", c)
	}
	clock.Advance(2 * time.Minute)
	if c, _ := hub.Capacity(ctx); len(c.Machines) != 0 {
		t.Errorf("offline machine offered: %+v", c)
	}
	if obs, reason, _ := hub.Observe(ctx, cluster.Ref{ID: uuid.New(), MachineID: f.machine.ID}); obs != cluster.ObservedUnknown || reason != agents.Offline {
		t.Errorf("offline: %s %q", obs, reason)
	}
	// Gone too long: its workloads aren't running, so they mustn't stay running and accounted.
	clock.Advance(10 * time.Minute)
	if obs, reason, _ := hub.Observe(ctx, cluster.Ref{ID: uuid.New(), MachineID: f.machine.ID}); obs != cluster.ObservedFailed ||
		reason != "its machine has been offline for over 10m0s" {
		t.Errorf("lost: %s %q", obs, reason)
	}
}

// Right after a restart, while machines reconnect, the backend says it's settling, so nothing is
// queued for a machine that's about to come back.
func TestCapacitySettlesAfterARestart(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	clock := testutil.NewClock(testutil.T0)
	hub := agents.New(f.st, testutil.Catalog(t), agents.Options{PollTimeout: time.Millisecond, AllowSim: true}, slog.New(slog.DiscardHandler), clock.Now)
	if c, _ := hub.Capacity(ctx); !c.Settling {
		t.Error("just started, with the machine not yet back: want settling")
	}
	if _, err := hub.Conn(f.machine.ID).Sync(ctx, protocol.SyncRequest{Version: protocol.Version, Inventory: mac()}); err != nil {
		t.Fatal(err)
	}
	if c, _ := hub.Capacity(ctx); c.Settling || len(c.Machines) != 1 {
		t.Errorf("every machine is back: %+v", c)
	}
	quiet := agents.New(f.st, testutil.Catalog(t), agents.Options{}, slog.New(slog.DiscardHandler), clock.Now)
	clock.Advance(time.Minute)
	if c, _ := quiet.Capacity(ctx); c.Settling {
		t.Error("still settling after the reconnect window")
	}
}

// An agent's report must fit its machine: GPU counts decide who shares nothing, so they're never
// taken on trust.
func TestInventoryMustFitTheMachine(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for name, change := range map[string]func(*protocol.Inventory){
		"two Apple GPUs":           func(inv *protocol.Inventory) { inv.Accelerators[0].Count = 2 },
		"an Apple GPU on Linux":    func(inv *protocol.Inventory) { inv.OS = "linux" },
		"more GPU memory than RAM": func(inv *protocol.Inventory) { inv.Accelerators[0].MemoryMiB = inv.MemoryMiB + 1 },
		"an NVIDIA GPU on a Mac": func(inv *protocol.Inventory) {
			inv.Accelerators = append(inv.Accelerators, domain.Accelerator{Kind: domain.AcceleratorNVIDIA, Model: "NVIDIA L40S", Count: 1})
		},
	} {
		hub := agents.New(f.st, testutil.Catalog(t), agents.Options{PollTimeout: time.Millisecond, AllowSim: true}, slog.New(slog.DiscardHandler), nil)
		inv := mac()
		change(inv)
		// Refused, but the machine still gets its desired state, so stops and removals reach it;
		// it isn't placeable, and it's asked for a report again.
		resp, err := hub.Conn(f.machine.ID).Sync(ctx, protocol.SyncRequest{Version: protocol.Version, Inventory: inv})
		if err != nil || !resp.SendInventory {
			t.Errorf("%s: %+v, %v; want desired state and a request for a new report", name, resp, err)
		}
		if c, _ := hub.Capacity(ctx); len(c.Machines) != 0 {
			t.Errorf("%s: the machine is offered for placement", name)
		}
	}
	// A driver for another OS is dropped.
	inv := mac()
	inv.Drivers = append(inv.Drivers, protocol.Driver{Name: domain.DriverDocker, Accelerators: []domain.AcceleratorKind{domain.AcceleratorCPU}})
	if _, err := f.hub.Conn(f.machine.ID).Sync(ctx, protocol.SyncRequest{Version: protocol.Version, Inventory: inv}); err != nil {
		t.Fatal(err)
	}
	c, _ := f.hub.Capacity(ctx)
	for _, d := range c.Machines[0].Drivers {
		if d.Name == domain.DriverDocker {
			t.Error("docker was accepted on a Mac")
		}
	}
}

func TestRegisterLocal(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	again, err := agents.RegisterLocal(ctx, f.st, uuid.Nil, "internal", "studio", "darwin", "arm64", domain.RegionLocal, testutil.T0)
	if err != nil || again.ID != f.machine.ID {
		t.Errorf("registering again = %+v, %v; want the same machine", again, err)
	}
	if _, err := agents.RegisterLocal(ctx, f.st, uuid.Nil, "internal", "studio", "darwin", "arm64", "eu-fra-1", testutil.T0); err == nil {
		t.Error("a machine moved region")
	}
	if _, err := agents.RegisterLocal(ctx, f.st, uuid.Nil, "nobody", "studio", "darwin", "arm64", domain.RegionLocal, testutil.T0); !domain.IsNotFound(err) {
		t.Errorf("an owner tenant that doesn't exist: %v; want not found", err)
	}
	if f.machine.Sharing != domain.SharingPrivate {
		t.Errorf("a registered machine is %s; machines are private by default", f.machine.Sharing)
	}

	// Once its ID is known, the machine is found by ID: a new hostname is only a new label, never
	// a new, empty machine whose agent would think its workloads were gone.
	renamed, err := agents.RegisterLocal(ctx, f.st, f.machine.ID, "internal", "studio-on-wifi", "darwin", "arm64", domain.RegionLocal, testutil.T0)
	if err != nil || renamed.ID != f.machine.ID {
		t.Errorf("after a hostname change = %+v, %v; want the same machine", renamed, err)
	}
	if _, err := agents.RegisterLocal(ctx, f.st, f.machine.ID, "acme", "studio", "darwin", "arm64", domain.RegionLocal, testutil.T0); err == nil {
		t.Error("the machine's owner changed")
	}
	if _, err := agents.RegisterLocal(ctx, f.st, f.machine.ID, "internal", "studio", "darwin", "arm64", "eu-fra-1", testutil.T0); err == nil {
		t.Error("the machine moved region")
	}
	fresh := uuid.New() // an ID from a database that was replaced
	if m, err := agents.RegisterLocal(ctx, f.st, fresh, "internal", "laptop", "darwin", "arm64", domain.RegionLocal, testutil.T0); err != nil || m.ID != fresh {
		t.Errorf("a saved ID this database doesn't have = %+v, %v; want it registered with that ID", m, err)
	}
}

// A standby that takes over late gives machines the reconnect window from when it starts leading,
// not from when its process started: it doesn't read their absence as their being lost.
func TestALateLeaderLetsMachinesReconnect(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	clock := testutil.NewClock(testutil.T0)
	hub := agents.New(f.st, testutil.Catalog(t), agents.Options{AllowSim: true}, slog.New(slog.DiscardHandler), clock.Now)
	clock.Advance(time.Hour) // standing by
	hub.Lead()
	if obs, reason, _ := hub.Observe(ctx, cluster.Ref{ID: uuid.New(), MachineID: f.machine.ID}); obs != cluster.ObservedUnknown || reason != "" {
		t.Errorf("just after taking over: %s %q; want unknown, deciding nothing", obs, reason)
	}
	if c, _ := hub.Capacity(ctx); !c.Settling {
		t.Error("just after taking over, before the machine reconnects: want settling")
	}
}

// A refused report waits for a change like any other sync, instead of being asked for again at
// once in a tight loop.
func TestRefusedReportsDontSpin(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	hub := agents.New(f.st, testutil.Catalog(t), agents.Options{PollTimeout: 200 * time.Millisecond, AllowSim: true}, slog.New(slog.DiscardHandler), nil)
	bad := mac()
	bad.Accelerators[0].Count = 2
	first, err := hub.Conn(f.machine.ID).Sync(ctx, protocol.SyncRequest{Version: protocol.Version, Inventory: bad})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := hub.Conn(f.machine.ID).Sync(ctx, protocol.SyncRequest{Version: protocol.Version, Generation: first.Generation, Inventory: bad}); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < 150*time.Millisecond {
		t.Errorf("an up-to-date agent with a refused report was answered after %s; want it to wait", took)
	}
}

// A status from before the agent applied a workload's latest change isn't the answer: an old
// failure doesn't fail a restart.
func TestStaleStatusesAreNotTheAnswer(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	w := f.place(t, f.owner, domain.KindNotebook, "cpu-2c-8g", domain.StateProvisioning)
	conn := f.hub.Conn(f.machine.ID)
	failed := []protocol.Status{{ID: w.ID, Attempt: w.Attempt, State: protocol.StateFailed, Code: protocol.CodeExited}}
	resp, err := conn.Sync(ctx, protocol.SyncRequest{Version: protocol.Version, Inventory: mac(), Statuses: failed})
	if err != nil {
		t.Fatal(err)
	}
	ref := cluster.Ref{ID: w.ID, Kind: w.Kind, MachineID: f.machine.ID, State: w.State, Attempt: w.Attempt}
	if obs, _, _ := f.hub.Observe(ctx, ref); obs != cluster.ObservedProgressing {
		t.Errorf("before the agent applied the run: %s; want progressing", obs)
	}
	if _, err := conn.Sync(ctx, protocol.SyncRequest{Version: protocol.Version, Generation: resp.Generation, Applied: resp.Generation, Statuses: failed}); err != nil {
		t.Fatal(err)
	}
	if obs, reason, _ := f.hub.Observe(ctx, ref); obs != cluster.ObservedFailed || reason != "it exited unexpectedly" {
		t.Errorf("after the agent applied it: %s %q; want the failure", obs, reason)
	}
	// A status about an earlier attempt doesn't answer this one, even once applied.
	earlier := []protocol.Status{{ID: w.ID, Attempt: w.Attempt - 1, State: protocol.StateFailed, Code: protocol.CodeExited}}
	if _, err := conn.Sync(ctx, protocol.SyncRequest{Version: protocol.Version, Generation: resp.Generation, Applied: resp.Generation, Statuses: earlier}); err != nil {
		t.Fatal(err)
	}
	if obs, _, _ := f.hub.Observe(ctx, ref); obs != cluster.ObservedProgressing {
		t.Errorf("a failure from an earlier attempt: %s; want progressing", obs)
	}
}

// The access proxy reaches only the in-process agent's workloads, at the loopback address, and only
// while they're ready (ADR-0017). A remote agent's report never names a port here, or keeps a token.
func TestTargetsAreLocalOnly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	w := f.place(t, f.owner, domain.KindNotebook, "cpu-2c-8g", domain.StateRunning)
	sync := func(local bool, st protocol.Status) {
		t.Helper()
		req := protocol.SyncRequest{Version: protocol.Version, Inventory: mac(), Statuses: []protocol.Status{st}}
		var err error
		if local {
			_, err = f.hub.Conn(f.machine.ID).Sync(ctx, req)
		} else {
			_, err = f.hub.Sync(ctx, f.machine.ID, req)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	ready := protocol.Status{ID: w.ID, Attempt: 1, State: protocol.StateReady, Access: &protocol.Access{Port: 41234, Token: "t0ken"}}

	sync(true, ready)
	if addr, token, ok := f.hub.Target(w.ID); !ok || addr != "127.0.0.1:41234" || token != "t0ken" {
		t.Errorf("the local agent's ready workload: %q %q %v", addr, token, ok)
	}
	sync(false, ready)
	if _, _, ok := f.hub.Target(w.ID); ok {
		t.Error("a remote agent's report was taken as a loopback address")
	}
	for name, st := range map[string]protocol.Status{
		"a port out of range": {ID: w.ID, Attempt: 1, State: protocol.StateReady, Access: &protocol.Access{Port: 70000, Token: "t0ken"}},
		"no token":            {ID: w.ID, Attempt: 1, State: protocol.StateReady, Access: &protocol.Access{Port: 41234}},
		"not ready":           {ID: w.ID, Attempt: 1, State: protocol.StateProgressing, Access: &protocol.Access{Port: 41234, Token: "t0ken"}},
	} {
		sync(true, st)
		if _, _, ok := f.hub.Target(w.ID); ok {
			t.Errorf("%s: reachable", name)
		}
	}
	if _, _, ok := f.hub.Target(uuid.New()); ok {
		t.Error("a workload the agent never reported is reachable")
	}
}
