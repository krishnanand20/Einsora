// Package agents is the node-agent cluster.Backend (ADR-0013, ADR-0016). Each machine's agent syncs
// with it: it records what the agent reports, and answers with what the machine should run, worked
// out from the store on every sync, so nothing about desired state lives only in memory. What
// agents report never decides a machine's owner, region, or a driver's isolation, and their words
// never reach a timeline.
package agents

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/tensora/tensora/controlplane/internal/agent/protocol"
	"github.com/tensora/tensora/controlplane/internal/catalog"
	"github.com/tensora/tensora/controlplane/internal/cluster"
	"github.com/tensora/tensora/controlplane/internal/domain"
	"github.com/tensora/tensora/controlplane/internal/store"
)

type Options struct {
	// Region is the one region this backend serves: a machine in another is refused (rule 9).
	Region domain.Region
	// PollTimeout is how long a sync waits for a change before answering anyway.
	PollTimeout time.Duration
	// OfflineAfter is how long after its last sync a machine that isn't waiting in one counts as
	// offline.
	OfflineAfter time.Duration
	// LostAfter is how long a machine may stay offline before its running workloads fail: they
	// aren't running, and shouldn't hold quota or be accounted forever.
	LostAfter time.Duration
	// AllowSim accepts agents' sim driver, which runs nothing (development only).
	AllowSim bool
}

const (
	defaultPollTimeout  = 20 * time.Second
	defaultOfflineAfter = 45 * time.Second
	defaultLostAfter    = 10 * time.Minute
	// heartbeatEvery limits how often a machine's last-seen time is written, so syncs stay reads.
	heartbeatEvery = time.Minute
	maxStatuses    = 1000
	maxDrivers     = 16
)

// Offline is the reason a workload waits while its machine is offline.
const Offline = "its machine is offline; waiting for it to reconnect"

type Hub struct {
	store   store.Store
	catalog *catalog.Catalog
	opts    Options
	log     *slog.Logger
	now     func() time.Time
	started time.Time // machines reconnect in the first OfflineAfter; nothing is decided on their absence

	mu       sync.Mutex
	sessions map[uuid.UUID]*session
}

// session is what the hub knows about one machine's agent since the hub started: runtime state,
// rebuilt from the agent's next sync after a restart.
type session struct {
	inventory     protocol.Inventory // validated
	hasInventory  bool
	statuses      map[uuid.UUID]protocol.Status
	lastSync      time.Time
	heartbeatAt   time.Time
	polling       int    // syncs waiting right now
	generation    uint64 // bumped whenever the machine's desired state may have changed
	wake          chan struct{}
	statusesKnown bool // a sync has reported the agent's complete status
	// local means the agent runs in this process, on this machine: its workloads' ports are
	// reachable at the loopback address (ADR-0017). Remote machines are reached through the relay
	// (Phase 2), never by their reported ports.
	local bool
	// inventoryRefused means the last report didn't fit the machine: it isn't offered for placement.
	inventoryRefused bool
	refusedReport    *protocol.Inventory // the last refused report, logged once
	// applied is the generation the agent last finished applying. sent is each workload's attempt
	// and mode in the desired state last sent, and sentAt the generation that first sent them. A
	// status answers the store only for the attempt and mode it expects, once the agent has
	// applied them; until then it's stale (an old failure after a restart, an old "ready" after
	// a stop), and absence means nothing yet.
	applied uint64
	sent    map[uuid.UUID]instruction
	sentAt  map[uuid.UUID]uint64
}

// instruction is what a desired state told an agent to do with one workload.
type instruction struct {
	attempt int64
	mode    protocol.Mode
}

// modeFor is what the agent should do with a workload of kind in state: run it while it's admitted,
// remove it when it's being deleted (and an endpoint whenever it's not running, as it keeps
// nothing), and otherwise park it, keeping its disk.
func modeFor(kind domain.WorkloadKind, state domain.State) protocol.Mode {
	switch {
	case state == domain.StateDeleting || state == domain.StateDeleted:
		return protocol.ModeRemove
	case state == domain.StateProvisioning || state == domain.StateRunning:
		return protocol.ModeRun
	case kind == domain.KindEndpoint:
		return protocol.ModeRemove
	}
	return protocol.ModePark
}

func New(st store.Store, cat *catalog.Catalog, opts Options, log *slog.Logger, now func() time.Time) *Hub {
	if now == nil {
		now = time.Now
	}
	if opts.Region == "" {
		opts.Region = domain.RegionLocal
	}
	if opts.PollTimeout <= 0 {
		opts.PollTimeout = defaultPollTimeout
	}
	if opts.OfflineAfter <= 0 {
		opts.OfflineAfter = defaultOfflineAfter
	}
	if opts.LostAfter <= 0 {
		opts.LostAfter = defaultLostAfter
	}
	return &Hub{store: st, catalog: cat, opts: opts, log: log, now: now, started: now(), sessions: map[uuid.UUID]*session{}}
}

// Session times keep Go's monotonic clock reading (no .UTC(), which strips it), so a change of the
// wall clock can't make a machine look offline or lost. Only the stored LastSeenAt is UTC.

// starting reports whether machines may still be reconnecting after the hub started.
func (h *Hub) starting() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.startingLocked()
}

// startingLocked is starting, for callers holding h.mu.
func (h *Hub) startingLocked() bool { return h.now().Sub(h.started) < h.opts.OfflineAfter }

// Lead starts the reconnect window when the hub's controller starts leading its region: machines'
// agents sync only with a leader, so a standby that takes over late must not read their absence
// until then as their being lost (cluster.Leader).
func (h *Hub) Lead() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.started = h.now()
}

// Conn is the in-process connection for machine id's agent (Solo). Phase 2's HTTPS transport
// calls Sync the same way, with the machine taken from its client certificate.
func (h *Hub) Conn(id uuid.UUID) protocol.Conn { return localConn{h: h, id: id} }

type localConn struct {
	h  *Hub
	id uuid.UUID
}

func (c localConn) Sync(ctx context.Context, req protocol.SyncRequest) (protocol.SyncResponse, error) {
	return c.h.sync(ctx, c.id, req, true)
}

func (h *Hub) session(id uuid.UUID) *session {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[id]
	if !ok {
		// Not 0, which a starting agent sends, nor a small number an agent from before a restart
		// might hold: its first sync is always answered at once.
		s = &session{statuses: map[uuid.UUID]protocol.Status{}, generation: uint64(h.now().UnixNano()), wake: make(chan struct{})}
		h.sessions[id] = s
	}
	return s
}

// Sync records machine id's report and answers with its desired state, waiting up to PollTimeout
// for a change when the agent is already up to date.
func (h *Hub) Sync(ctx context.Context, id uuid.UUID, req protocol.SyncRequest) (protocol.SyncResponse, error) {
	return h.sync(ctx, id, req, false)
}

// sync is Sync, from the agent in this process (local) or another machine's. Only a local agent's
// workloads are reached at the loopback address, so only its statuses keep how to reach them
// (ADR-0017): a remote agent's would send the workload's token off its machine, and name ports on
// this one.
func (h *Hub) sync(ctx context.Context, id uuid.UUID, req protocol.SyncRequest, local bool) (protocol.SyncResponse, error) {
	if req.Version != protocol.Version {
		return protocol.SyncResponse{}, domain.Errorf(domain.CodeInvalid, "protocol version %d isn't supported; this control plane speaks %d", req.Version, protocol.Version)
	}
	if len(req.Statuses) > maxStatuses {
		return protocol.SyncResponse{}, domain.Errorf(domain.CodeInvalid, "too many statuses")
	}
	var m domain.Machine
	if err := h.store.InTx(ctx, func(q store.Queries) error {
		var err error
		m, err = q.MachineByID(ctx, id)
		return err
	}); err != nil {
		return protocol.SyncResponse{}, err
	}
	if m.Region != h.opts.Region {
		return protocol.SyncResponse{}, domain.Errorf(domain.CodeForbidden, "machine %s is in region %s, not this control plane's %s", m.Name, m.Region, h.opts.Region)
	}
	s := h.session(id)
	now := h.now()

	statuses := map[uuid.UUID]protocol.Status{}
	for _, st := range req.Statuses {
		switch st.State {
		case protocol.StateProgressing, protocol.StateReady, protocol.StateStopped, protocol.StateFailed:
		default:
			st.State, st.Code = protocol.StateFailed, "" // an unknown state reads as a problem
		}
		if a := st.Access; a != nil && (!local || st.State != protocol.StateReady || a.Port < 1 || a.Port > 65535 ||
			a.Token == "" || len(a.Token) > 512) {
			st.Access = nil
		}
		statuses[st.ID] = st
	}
	h.mu.Lock()
	s.statuses, s.statusesKnown, s.lastSync, s.applied, s.local = statuses, true, now, req.Applied, local
	h.mu.Unlock()

	if err := h.recordInventory(ctx, s, m, req.Inventory, now); err != nil {
		return protocol.SyncResponse{}, err
	}

	h.mu.Lock()
	gen, wake, has := s.generation, s.wake, s.hasInventory && !s.inventoryRefused
	reported := s.hasInventory || s.inventoryRefused // even a refused report means waiting, not asking again at once
	s.polling++
	h.mu.Unlock()
	if req.Generation == gen && reported {
		timer := time.NewTimer(h.opts.PollTimeout)
		select {
		case <-wake:
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
	}
	h.mu.Lock()
	s.polling--
	s.lastSync = h.now()
	gen = s.generation
	h.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return protocol.SyncResponse{}, err
	}
	desired, err := h.desired(ctx, m)
	if err != nil {
		return protocol.SyncResponse{}, err
	}
	h.mu.Lock()
	h.recordSent(s, gen, desired)
	h.mu.Unlock()
	return protocol.SyncResponse{Generation: gen, Workloads: desired, SendInventory: !has}, nil
}

// recordSent notes when each workload's attempt or mode was first sent. Call with h.mu held.
func (h *Hub) recordSent(s *session, gen uint64, desired []protocol.Desired) {
	if s.sent == nil {
		s.sent, s.sentAt = map[uuid.UUID]instruction{}, map[uuid.UUID]uint64{}
	}
	listed := map[uuid.UUID]bool{}
	for _, d := range desired {
		listed[d.ID] = true
		if in := (instruction{d.Attempt, d.Mode}); s.sent[d.ID] != in {
			s.sent[d.ID], s.sentAt[d.ID] = in, gen
		}
	}
	for id := range s.sent {
		if !listed[id] {
			delete(s.sent, id)
			delete(s.sentAt, id)
		}
	}
}

// recordInventory checks and stores a reported inventory, or writes a heartbeat now and then. A
// report that doesn't fit the machine is refused, and the machine isn't offered for placement until
// a good one arrives; it still gets its desired state, so stops and removals reach it.
func (h *Hub) recordInventory(ctx context.Context, s *session, m domain.Machine, inv *protocol.Inventory, now time.Time) error {
	h.mu.Lock()
	beat := s.hasInventory && now.Sub(s.heartbeatAt) >= heartbeatEvery
	h.mu.Unlock()
	if inv == nil && !beat {
		return nil
	}
	if inv != nil {
		checked, err := h.checkInventory(m, *inv)
		if err != nil {
			h.mu.Lock()
			repeat := s.refusedReport != nil && reflect.DeepEqual(*s.refusedReport, *inv)
			s.inventoryRefused, s.refusedReport = true, inv
			h.mu.Unlock()
			if !repeat {
				h.log.Warn("agent inventory refused; the machine isn't offered for placement until its report fits it",
					"machine", m.ID, "err", err)
			}
			return nil
		}
		m.OS, m.Arch, m.CPUs, m.MemoryMiB, m.Accelerators = checked.OS, checked.Arch, checked.CPUs, checked.MemoryMiB, checked.Accelerators
		inv = &checked
	}
	m.LastSeenAt = now.UTC()
	if err := h.store.InTx(ctx, func(q store.Queries) error { return q.UpdateMachineInventory(ctx, m) }); err != nil {
		return fmt.Errorf("record machine %s: %w", m.ID, err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s.heartbeatAt = now
	if inv != nil {
		s.inventory, s.hasInventory, s.inventoryRefused = *inv, true, false
	}
	return nil
}

// checkInventory validates what an agent reports and keeps only drivers the control plane knows.
func (h *Hub) checkInventory(m domain.Machine, inv protocol.Inventory) (protocol.Inventory, error) {
	m.OS, m.Arch, m.CPUs, m.MemoryMiB, m.Accelerators = inv.OS, inv.Arch, inv.CPUs, inv.MemoryMiB, inv.Accelerators
	if err := m.Validate(); err != nil {
		return inv, err
	}
	if inv.UsableCPUs < 0 || inv.UsableCPUs > inv.CPUs || inv.UsableMemoryMiB < 0 || inv.UsableMemoryMiB > inv.MemoryMiB ||
		len(inv.AgentVersion) > 64 || !printable(inv.AgentVersion) || len(inv.Drivers) > maxDrivers {
		return inv, domain.Errorf(domain.CodeInvalid, "machine %s: its inventory is out of range", m.Name)
	}
	// The accelerators must make sense for the machine: one Apple GPU, only on an Apple Silicon Mac,
	// with no more memory than the Mac has; NVIDIA and AMD GPUs only on Linux. Counts decide how many
	// workloads share nothing, so they're never taken on trust.
	apple := 0
	for _, a := range inv.Accelerators {
		switch a.Kind {
		case domain.AcceleratorApple:
			apple++
			if inv.OS != "darwin" || inv.Arch != "arm64" || a.Count != 1 || apple > 1 || a.MemoryMiB > inv.MemoryMiB {
				return inv, domain.Errorf(domain.CodeInvalid, "machine %s: its Apple GPU report doesn't fit the machine", m.Name)
			}
		case domain.AcceleratorNVIDIA, domain.AcceleratorAMD:
			if inv.OS != "linux" {
				return inv, domain.Errorf(domain.CodeInvalid, "machine %s: %s GPUs are supported on Linux only", m.Name, a.Kind)
			}
		case domain.AcceleratorCPU:
			return inv, domain.Errorf(domain.CodeInvalid, "machine %s: CPUs aren't reported as accelerators", m.Name)
		}
	}
	var drivers []protocol.Driver
	seen := map[string]bool{}
	for _, d := range inv.Drivers {
		spec, ok := domain.LookupDriver(d.Name)
		if !ok || seen[d.Name] || (d.Name == domain.DriverSim && !h.opts.AllowSim) || (spec.OS != "" && spec.OS != inv.OS) {
			h.log.Warn("agent driver ignored", "machine", m.ID, "driver", fmt.Sprintf("%.40q", d.Name))
			continue
		}
		seen[d.Name] = true
		var accelerators []domain.AcceleratorKind
		for _, a := range d.Accelerators {
			if slices.Contains(spec.Accelerators, a) && !slices.Contains(accelerators, a) {
				accelerators = append(accelerators, a)
			}
		}
		drivers = append(drivers, protocol.Driver{Name: d.Name, Accelerators: accelerators})
	}
	inv.Drivers = drivers
	return inv, nil
}

func printable(s string) bool {
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// desired is what machine m should be doing: every workload placed on it, by state. Placed and
// admitted ones run; deleting ones are removed; endpoints that stopped are removed (they keep
// nothing); everything else is parked, keeping its disk.
func (h *Hub) desired(ctx context.Context, m domain.Machine) ([]protocol.Desired, error) {
	var ws []domain.Workload
	if err := h.store.InTx(ctx, func(q store.Queries) error {
		var err error
		ws, err = q.WorkloadsOnMachine(ctx, m.ID)
		return err
	}); err != nil {
		return nil, err
	}
	out := make([]protocol.Desired, 0, len(ws))
	for _, w := range ws {
		d := protocol.Desired{ID: w.ID, Mode: modeFor(w.Kind, w.State), Attempt: w.Attempt, Kind: w.Kind, Driver: w.Driver,
			StrongIsolation: w.TenantID != m.TenantID}
		if err := h.describe(&d, w); err != nil && d.Mode == protocol.ModeRun {
			// Removed from the catalog since. An agent leaves a workload that's already running
			// alone, and refuses to start one it can't be told how to run; the controller decides
			// what happens next. Removing and parking need no description.
			h.log.Warn("workload can't be fully described to its agent", "workload", w.ID, "err", err)
		}
		out = append(out, d)
	}
	return out, nil
}

// describe fills in what the agent needs to run w: its placed class's size (first, so even a
// workload being removed says which GPUs it holds), and its image's runtime for that class's
// accelerator.
func (h *Hub) describe(d *protocol.Desired, w domain.Workload) error {
	class := w.PlacedClass
	if class == "" {
		class = w.GPUClass
	}
	g, ok := h.catalog.GPUClass(class)
	if !ok {
		return fmt.Errorf("class %q isn't in the catalog", class)
	}
	d.Accelerator, d.VCPUs, d.MemoryMiB = g.Accelerator, g.VCPUsPerGPU*w.GPUCount, int64(g.MemoryGiBPerGPU)*1024*int64(w.GPUCount)
	if g.Accelerator != domain.AcceleratorCPU {
		d.Devices = w.GPUCount
	}
	img, ok := h.catalog.Image(w.ImageID)
	if !ok {
		return fmt.Errorf("image %q isn't in the catalog", w.ImageID)
	}
	rt, ok := img.RuntimeFor(g.Accelerator)
	if !ok {
		return fmt.Errorf("image %q has no runtime for %s", w.ImageID, g.Accelerator)
	}
	d.ImageID = img.ID
	d.Runtime = protocol.Runtime{Ref: rt.Ref, Server: rt.Server, ModelFormat: rt.ModelFormat}
	d.ModelURI, d.SSHPublicKey = w.Params[domain.ParamModelURI], w.Params[domain.ParamSSHPublicKey]
	return nil
}

// online reports whether s's agent is in a sync now or synced recently. Call with h.mu held.
func (h *Hub) online(s *session) bool {
	return s.polling > 0 || h.now().Sub(s.lastSync) < h.opts.OfflineAfter
}

// Capacity lists the machines connected now, with their devices per class, the CPUs and memory
// their agents let Tensora use, and their drivers. The controller places on them. Right after the
// hub starts, while machines in its region haven't all reconnected, it says it's settling, so a
// restart doesn't queue workloads for machines that are about to come back.
func (h *Hub) Capacity(ctx context.Context) (cluster.Capacity, error) {
	settling := false
	if h.starting() {
		var machines []domain.Machine
		if err := h.store.InTx(ctx, func(q store.Queries) error {
			var err error
			machines, err = q.MachinesInRegion(ctx, h.opts.Region)
			return err
		}); err != nil {
			return cluster.Capacity{}, err
		}
		h.mu.Lock()
		for _, m := range machines {
			if s, ok := h.sessions[m.ID]; !ok || !s.hasInventory {
				settling = true
			}
		}
		h.mu.Unlock()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := cluster.Capacity{Machines: []cluster.MachineCapacity{}, Settling: settling}
	for id, s := range h.sessions {
		if !s.hasInventory || s.inventoryRefused || !h.online(s) {
			continue
		}
		mc := cluster.MachineCapacity{ID: id, Devices: h.catalog.DevicesOn(s.inventory.Accelerators, h.opts.Region),
			VCPUs: s.inventory.UsableCPUs, MemoryMiB: s.inventory.UsableMemoryMiB}
		for _, d := range s.inventory.Drivers {
			mc.Drivers = append(mc.Drivers, cluster.Driver{Name: d.Name, Accelerators: slices.Clone(d.Accelerators)})
		}
		out.Machines = append(out.Machines, mc)
	}
	slices.SortFunc(out.Machines, func(a, b cluster.MachineCapacity) int { return cmp.Compare(a.ID.String(), b.ID.String()) })
	return out, nil
}

// Run, Park, and Delete wake the machine's agent: its desired state comes from the store, which the
// controller has already changed.
func (h *Hub) Run(_ context.Context, s cluster.Spec) error { h.nudge(s.Workload.MachineID); return nil }
func (h *Hub) Park(_ context.Context, s cluster.Spec) error {
	h.nudge(s.Workload.MachineID)
	return nil
}
func (h *Hub) Delete(_ context.Context, ref cluster.Ref) error {
	h.nudge(ref.MachineID)
	return nil
}

func (h *Hub) nudge(id uuid.UUID) {
	if id == uuid.Nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.sessions[id]; ok {
		s.generation++
		close(s.wake)
		s.wake = make(chan struct{})
	}
}

// Observe reports what the workload's agent last said, with reasons from a fixed vocabulary.
func (h *Hub) Observe(_ context.Context, ref cluster.Ref) (cluster.Observed, string, error) {
	if ref.MachineID == uuid.Nil {
		return cluster.ObservedAbsent, "", nil // never placed on a machine
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[ref.MachineID]
	if !ok || !s.statusesKnown || !h.online(s) {
		offlineSince := h.started
		if ok && s.lastSync.After(offlineSince) {
			offlineSince = s.lastSync
		}
		switch away := h.now().Sub(offlineSince); {
		case !ok && h.startingLocked():
			return cluster.ObservedUnknown, "", nil // reconnecting after a restart: nothing to say yet
		case away > h.opts.LostAfter:
			return cluster.ObservedFailed, fmt.Sprintf("its machine has been offline for over %s", h.opts.LostAfter), nil
		}
		return cluster.ObservedUnknown, Offline, nil
	}
	// Only a report from after the agent applied what the store now expects answers it; anything
	// earlier, even no status at all, is still in progress.
	want := instruction{ref.Attempt, modeFor(ref.Kind, ref.State)}
	if g, sent := s.sentAt[ref.ID]; !sent || s.sent[ref.ID] != want || s.applied < g {
		return cluster.ObservedProgressing, "", nil
	}
	st, ok := s.statuses[ref.ID]
	if !ok {
		return cluster.ObservedAbsent, "", nil
	}
	if want.mode == protocol.ModeRun && st.Attempt != ref.Attempt {
		return cluster.ObservedProgressing, "", nil // about another attempt
	}
	switch st.State {
	case protocol.StateReady:
		return cluster.ObservedReady, "", nil
	case protocol.StateStopped:
		return cluster.ObservedStopped, "", nil
	case protocol.StateFailed:
		return cluster.ObservedFailed, protocol.FailureReason(st.Code), nil
	}
	return cluster.ObservedProgressing, protocol.ProgressReason(st.Code), nil
}

// Target is where the access proxy reaches a ready workload on this machine, and the token its
// server expects (ADR-0017): only for a workload whose agent runs in this process and reported it
// ready with access just now. The token is never stored or logged.
func (h *Hub) Target(id uuid.UUID) (addr, token string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.sessions {
		st, has := s.statuses[id]
		if !has || !s.local || !h.online(s) || st.State != protocol.StateReady || st.Access == nil {
			continue
		}
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(st.Access.Port)), st.Access.Token, true
	}
	return "", "", false
}

// RegisterLocal finds or creates the record of the control plane's own machine, owned by the tenant
// with slug owner, for the in-process agent (Solo). id is the machine's ID from an earlier start, or
// uuid.Nil the first time. A known machine is found by ID alone, so a new hostname is only a new
// label: registering it again as an empty machine would make its workloads look gone. Its owner
// and region never change (rule 9); a mismatch is refused. Phase 2 enrolment registers other
// machines.
func RegisterLocal(ctx context.Context, st store.Store, id uuid.UUID, owner, name, goos, goarch string, region domain.Region, now time.Time) (domain.Machine, error) {
	var m domain.Machine
	err := st.InTx(ctx, func(q store.Queries) error {
		t, err := q.TenantBySlug(ctx, owner)
		if err != nil {
			return err
		}
		if id != uuid.Nil {
			existing, err := q.MachineByID(ctx, id)
			switch {
			case err == nil && existing.TenantID != t.ID:
				return domain.Errorf(domain.CodeConflict, "this machine belongs to another tenant than %s (TENSORA_MACHINE_OWNER); a machine's owner can't change yet", owner)
			case err == nil && existing.Region != region:
				return domain.Errorf(domain.CodeConflict, "this machine is registered in region %s, not %s; a machine never moves region", existing.Region, region)
			case err == nil:
				m = existing
				return nil
			case !domain.IsNotFound(err):
				return err
			} // not in this database: registered below with the same ID
		} else {
			ms, err := q.ListMachines(ctx, t.ID)
			if err != nil {
				return err
			}
			for _, existing := range ms {
				if existing.Name == name {
					if existing.Region != region {
						return domain.Errorf(domain.CodeConflict, "machine %s is registered in region %s, not %s; a machine never moves region", name, existing.Region, region)
					}
					m = existing
					return nil
				}
			}
			id = uuid.Must(uuid.NewV7())
		}
		m = domain.Machine{ID: id, TenantID: t.ID, Name: name, Region: region, OS: goos, Arch: goarch,
			Sharing: domain.SharingPrivate, CreatedAt: now.UTC()}
		if err := m.Validate(); err != nil {
			return err
		}
		return q.CreateMachine(ctx, m)
	})
	return m, err
}
