// Package agent is the node agent (ADR-0013, ADR-0016). It reports its machine and its workloads to
// the control plane, and runs what it's told through drivers. Policy is the control plane's, but
// the agent checks what it's given: it refuses a driver it doesn't have, weak isolation for a
// workload that needs strong, and starting a workload while another holds the GPU it needs, so a
// bug or a race in the control plane can't put two users on one GPU. It deletes a workload's data
// only when told to remove that workload, never because a list left it out.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/agent/protocol"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/redact"
)

// Driver runs workloads one way (ADR-0013): natively on macOS, in containers, or in VMs. Run, Stop,
// and Remove must return promptly: long work, such as pulling an image, carries on in the
// background and shows as progressing in Statuses. The agent gives each call driverTimeout, and
// Statuses statusTimeout. Stop leaves a workload stopped, not failed, even one that had crashed.
// Attempts: before Run returns, the workload's status carries d.Attempt (so an old attempt's
// failure is never reported for the new one), and Statuses reports each workload's attempt,
// kept across agent restarts (so an attempt that ended is never started again).
type Driver interface {
	// Name is the driver's name in domain.LookupDriver.
	Name() string
	// Accelerators are the kinds this driver can use on this machine.
	Accelerators() []domain.AcceleratorKind
	// Run starts the workload d describes, creating it if needed. Idempotent.
	Run(ctx context.Context, d protocol.Desired) error
	// Stop stops the workload and frees its GPUs, keeping everything it stored. Idempotent.
	Stop(ctx context.Context, id uuid.UUID) error
	// Remove removes the workload and everything it stored. Idempotent.
	Remove(ctx context.Context, id uuid.UUID) error
	// Statuses reports every workload the driver has. It's called while other calls are running.
	Statuses(ctx context.Context) ([]protocol.Status, error)
}

// Inventory detects the machine's hardware. Drivers and the agent's version are filled in by the
// agent.
type Inventory func(ctx context.Context) (protocol.Inventory, error)

type Agent struct {
	conn      protocol.Conn
	drivers   []Driver
	inventory Inventory
	version   string
	log       *slog.Logger

	mu       sync.Mutex
	inv      protocol.Inventory // the last detected
	invAt    time.Time
	invSent  protocol.Inventory            // the last the control plane received
	refused  map[uuid.UUID]protocol.Status // given but not run, reported until the next desired state
	applied  uint64                        // the desired state last applied
	reported string                        // digest of the statuses last reported
	// started is the attempt the agent last started each workload for. An attempt is started at
	// most once: one that ended (failed, stopped by itself, gone) isn't restarted until a new one.
	// startFailed keeps why an attempt couldn't start, reported for as long as it's asked for.
	started     map[uuid.UUID]int64
	startFailed map[uuid.UUID]protocol.Status

	// Output (ADR-0024): how far the control plane has each stream, sent one request at a time.
	outMu      sync.Mutex
	out        map[outputKey]*outputCursor
	outFailing bool
	clock      func() time.Time
}

const (
	inventoryEvery = 5 * time.Minute
	firstRetry     = time.Second
	maxRetry       = 30 * time.Second
	// driverTimeout bounds every driver call, so one stuck call can't stall the machine.
	driverTimeout = 2 * time.Minute
	// statusTimeout bounds reading the drivers' statuses, which every sync does.
	statusTimeout = 30 * time.Second
	// statusEvery is how often the agent looks for status changes to report between syncs.
	statusEvery = 2 * time.Second
)

// desiredState is one desired state and the generation it came with.
type desiredState struct {
	gen       uint64
	workloads []protocol.Desired
}

func New(conn protocol.Conn, drivers []Driver, inventory Inventory, version string, log *slog.Logger) *Agent {
	return &Agent{conn: conn, drivers: drivers, inventory: inventory, version: version, log: log,
		refused: map[uuid.UUID]protocol.Status{}, started: map[uuid.UUID]int64{}, startFailed: map[uuid.UUID]protocol.Status{},
		out: map[outputKey]*outputCursor{}, clock: time.Now}
}

// Run syncs until ctx is cancelled, retrying with backoff when the control plane can't be reached.
// Applying runs separately, on the latest desired state, so the agent keeps reporting while a
// driver works: a busy machine never looks offline. Anything worth reporting (a desired state
// applied, a workload's status changing) cuts the waiting sync short, so the control plane hears
// it at once rather than when the long poll ends.
func (a *Agent) Run(ctx context.Context) error {
	a.log.Info("agent started", "drivers", len(a.drivers))
	latest := make(chan desiredState, 1)
	kick := make(chan struct{}, 1)
	report := func() {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
	var workers sync.WaitGroup
	workers.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case d := <-latest:
				if a.Apply(ctx, d.workloads) {
					a.mu.Lock()
					a.applied = d.gen
					a.mu.Unlock()
					report()
				}
			}
		}
	})
	workers.Go(func() { a.watch(ctx, report) })
	workers.Go(func() { a.shipLoop(ctx) })
	defer workers.Wait()
	var gen uint64
	sendInventory := true
	wait := firstRetry
	for {
		attempt, cancel := context.WithCancel(ctx)
		kicked := make(chan bool, 1)
		go func() {
			select {
			case <-kick:
				cancel()
				kicked <- true
			case <-attempt.Done():
				kicked <- false
			}
		}()
		resp, err := a.Sync(attempt, gen, sendInventory)
		cancel()
		if <-kicked && ctx.Err() == nil {
			continue // something changed: report it now
		}
		if ctx.Err() != nil {
			return nil // stopping
		}
		if err != nil {
			a.log.Warn("sync failed; retrying", "err", redact.Secrets(err.Error()), "in", wait)
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
			wait, sendInventory = min(2*wait, maxRetry), true
			continue
		}
		wait = firstRetry
		gen, sendInventory = resp.Generation, resp.SendInventory
		select { // only the latest desired state matters
		case <-latest:
		default:
		}
		latest <- desiredState{gen: resp.Generation, workloads: resp.Workloads}
	}
}

// watch looks at the drivers' statuses every statusEvery, and calls report when they differ from
// what was last reported, such as a workload that finished starting.
func (a *Agent) watch(ctx context.Context, report func()) {
	ticker := time.NewTicker(statusEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		statuses, err := a.statuses(ctx)
		if err != nil {
			continue
		}
		a.mu.Lock()
		changed := digest(a.reportable(statuses)) != a.reported
		a.mu.Unlock()
		if changed {
			report()
		}
	}
}

// reportable is what a report says about statuses: refusals override what the drivers have, for a
// workload the control plane asked for. Call with a.mu held.
func (a *Agent) reportable(statuses map[uuid.UUID]held) []protocol.Status {
	var out []protocol.Status
	for id, st := range statuses {
		if refused, ok := a.refused[id]; ok {
			st.Status = refused
		}
		out = append(out, st.Status)
	}
	for id, st := range a.refused {
		if _, ok := statuses[id]; !ok {
			out = append(out, st)
		}
	}
	return out
}

// digest identifies a set of statuses, whatever their order.
func digest(statuses []protocol.Status) string {
	lines := make([]string, len(statuses))
	for i, st := range statuses {
		lines[i] = fmt.Sprintf("%s %d %s %s", st.ID, st.Attempt, st.State, st.Code)
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}

// Sync sends one report: every workload's status, and the inventory when it changed or is asked for.
func (a *Agent) Sync(ctx context.Context, gen uint64, sendInventory bool) (protocol.SyncResponse, error) {
	req := protocol.SyncRequest{Version: protocol.Version, Generation: gen}
	inv, changed, err := a.detect(ctx)
	if err != nil {
		return protocol.SyncResponse{}, fmt.Errorf("inventory: %w", err)
	}
	if changed || sendInventory {
		req.Inventory = &inv
	}
	// Applied is read before the statuses, so they're never older than what it claims.
	a.mu.Lock()
	req.Applied = a.applied
	a.mu.Unlock()
	statuses, err := a.statuses(ctx)
	if err != nil {
		return protocol.SyncResponse{}, err
	}
	a.mu.Lock()
	// A refusal is what the control plane needs to hear about a workload it asked for, even one a
	// driver still has (such as a notebook it asked to move to another driver).
	req.Statuses = a.reportable(statuses)
	a.reported = digest(req.Statuses)
	a.mu.Unlock()
	resp, err := a.conn.Sync(ctx, req)
	if err == nil && req.Inventory != nil {
		a.mu.Lock()
		a.invSent = *req.Inventory // only now: a sync cut short or failed must send it again
		a.mu.Unlock()
	}
	return resp, err
}

// detect refreshes the inventory every few minutes and reports whether it changed.
func (a *Agent) detect(ctx context.Context) (protocol.Inventory, bool, error) {
	a.mu.Lock()
	cached, at := a.inv, a.invAt
	a.mu.Unlock()
	if !at.IsZero() && time.Since(at) < inventoryEvery {
		a.mu.Lock()
		defer a.mu.Unlock()
		return cached, !reflect.DeepEqual(cached, a.invSent), nil
	}
	inv, err := a.inventory(ctx)
	if err != nil {
		return protocol.Inventory{}, false, err
	}
	inv.AgentVersion = a.version
	for _, d := range a.drivers {
		inv.Drivers = append(inv.Drivers, protocol.Driver{Name: d.Name(), Accelerators: d.Accelerators()})
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	changed := !reflect.DeepEqual(inv, a.invSent)
	a.inv, a.invAt = inv, time.Now()
	return inv, changed, nil
}

// held is one workload a driver has. others are further drivers holding the same ID, which
// shouldn't happen (a move between drivers that didn't finish); such a workload is treated as
// holding every device until it's sorted out.
type held struct {
	protocol.Status
	driver Driver
	others []Driver
}

// statuses is every workload the drivers have, as they have it. The agent decides on this; refusals
// are added only to what it reports.
func (a *Agent) statuses(ctx context.Context) (map[uuid.UUID]held, error) {
	out := map[uuid.UUID]held{}
	for _, d := range a.drivers {
		callCtx, cancel := context.WithTimeout(ctx, statusTimeout)
		sts, err := d.Statuses(callCtx)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("driver %s: %w", d.Name(), err)
		}
		for _, st := range sts {
			if h, ok := out[st.ID]; ok {
				h.others = append(h.others, d)
				if active(st.State) {
					h.State = st.State
				}
				out[st.ID] = h
				continue
			}
			out[st.ID] = held{Status: st, driver: d}
		}
	}
	return out, nil
}

func (a *Agent) driver(name string) Driver {
	for _, d := range a.drivers {
		if d.Name() == name {
			return d
		}
	}
	return nil
}

// Apply converges the machine to the desired state: removals and parks first, which free GPUs,
// then runs. A workload the drivers have that isn't listed is stopped and kept, never removed: the
// list can leave one out only by mistake (another machine's identity, a different database), and
// deleting data is never the safe guess. The operator's log says so. It reports whether it went
// through the whole list; one cut short (it couldn't read the drivers, or it's stopping) doesn't
// count as applied.
func (a *Agent) Apply(ctx context.Context, desired []protocol.Desired) bool {
	refused := map[uuid.UUID]protocol.Status{}
	defer func() {
		a.mu.Lock()
		a.refused = refused
		a.mu.Unlock()
	}()
	listed := map[uuid.UUID]protocol.Desired{}
	refuse := func(id uuid.UUID, state protocol.State, code protocol.Code) {
		// For the attempt asked for, so the control plane never takes it for an older one's.
		refused[id] = protocol.Status{ID: id, Attempt: listed[id].Attempt, State: state, Code: code}
	}
	have, err := a.statuses(ctx)
	if err != nil {
		a.log.Error("can't read workloads; nothing applied", "err", redact.Secrets(err.Error()))
		return false
	}
	order := map[protocol.Mode]int{protocol.ModeRemove: 0, protocol.ModePark: 1, protocol.ModeRun: 2}
	desired = slices.Clone(desired)
	slices.SortStableFunc(desired, func(x, y protocol.Desired) int { return order[x.Mode] - order[y.Mode] })

	for _, d := range desired {
		listed[d.ID] = d
	}
	for id := range a.started {
		if _, ok := listed[id]; !ok {
			delete(a.started, id)
			delete(a.startFailed, id)
		}
	}
	for id, h := range have {
		if _, ok := listed[id]; !ok && active(h.State) {
			a.log.Warn("stopping a workload the control plane doesn't list; its data is kept until it's removed by hand",
				"workload", id, "driver", h.driver.Name())
			a.stop(ctx, h, refuse)
		}
	}
	for _, d := range desired {
		if ctx.Err() != nil {
			return false
		}
		h, has := have[d.ID]
		switch d.Mode {
		case protocol.ModeRemove:
			if has {
				a.remove(ctx, h, refuse)
			}
			continue
		case protocol.ModePark:
			if has && active(h.State) {
				a.stop(ctx, h, refuse)
			}
			continue
		case protocol.ModeRun:
		default:
			refuse(d.ID, protocol.StateFailed, protocol.CodeInvalid)
			continue
		}
		drv := a.driver(d.Driver)
		spec, known := domain.LookupDriver(d.Driver)
		switch {
		case drv == nil || !known:
			refuse(d.ID, protocol.StateFailed, protocol.CodeNoDriver)
			continue
		case d.StrongIsolation && spec.Isolation != domain.IsolationStrong:
			refuse(d.ID, protocol.StateFailed, protocol.CodeIsolation)
			continue
		case has && (h.driver != drv || len(h.others) > 0):
			// Held by another driver than the one it's placed with. An endpoint keeps nothing, so
			// it moves once every other copy is gone; anything else would lose its data, so it's
			// refused.
			if d.Kind != domain.KindEndpoint {
				refuse(d.ID, protocol.StateFailed, protocol.CodeInvalid)
				continue
			}
			if !a.removeOthers(ctx, h, drv, refuse) {
				continue
			}
			current, err := a.statuses(ctx)
			if err != nil {
				refuse(d.ID, protocol.StateProgressing, protocol.CodeDriverProblem)
				continue
			}
			h, has = current[d.ID] // now held by drv alone, if at all
		}
		if has && active(h.State) && h.Attempt == d.Attempt {
			continue // already running; it was checked when it started
		}
		if has && active(h.State) {
			// Still running from another attempt, whose GPUs and spec may not be this one's: it
			// stops first (keeping its data), and this attempt starts through every check once it
			// has.
			a.stop(ctx, h, refuse)
			current, err := a.statuses(ctx)
			if err != nil {
				refuse(d.ID, protocol.StateProgressing, protocol.CodeDriverProblem)
				continue
			}
			if h, has = current[d.ID]; has && active(h.State) {
				refuse(d.ID, protocol.StateProgressing, "") // still stopping; started on a later pass
				continue
			}
		}
		if began, ok := a.started[d.ID]; (ok && began == d.Attempt) || (has && h.Attempt == d.Attempt) {
			// This attempt was started and has ended: the control plane decides what's next. If it
			// couldn't start at all, that's still the answer.
			if failure, ok := a.startFailed[d.ID]; ok && failure.Attempt == d.Attempt {
				refused[d.ID] = failure
			}
			continue
		}
		if !slices.Contains(drv.Accelerators(), d.Accelerator) || !slices.Contains(spec.Kinds, d.Kind) {
			refuse(d.ID, protocol.StateFailed, protocol.CodeInvalid)
			continue
		}
		kind, devices := d.Accelerator, d.Devices
		if d.Driver == domain.DriverMacOSNative && kind == domain.AcceleratorCPU {
			// Native CPU notebooks and endpoints can still use Metal; reserve it before starting them too.
			kind, devices = domain.AcceleratorApple, 1
		}
		if devices > 0 {
			current, err := a.statuses(ctx)
			if err != nil {
				refuse(d.ID, protocol.StateProgressing, protocol.CodeDriverProblem)
				continue
			}
			if free := a.freeDevices(kind, current, listed); free < devices {
				refuse(d.ID, protocol.StateProgressing, protocol.CodeDevicesBusy)
				continue
			}
		}
		a.start(ctx, drv, d, refuse)
	}
	return true
}

// start runs one attempt of a workload, once: a failure to start is kept, and reported for as long
// as that attempt is asked for.
func (a *Agent) start(ctx context.Context, drv Driver, d protocol.Desired, refuse func(uuid.UUID, protocol.State, protocol.Code)) {
	a.started[d.ID] = d.Attempt
	delete(a.startFailed, d.ID)
	callCtx, cancel := context.WithTimeout(ctx, driverTimeout)
	err := drv.Run(callCtx, d)
	cancel()
	if err == nil {
		return
	}
	a.log.Error("driver couldn't run a workload", "workload", d.ID, "driver", d.Driver, "err", redact.Secrets(err.Error()))
	code := protocol.CodeDriverProblem
	var c codedError
	if errors.As(err, &c) {
		code = c.code
	}
	refuse(d.ID, protocol.StateFailed, code)
	a.startFailed[d.ID] = protocol.Status{ID: d.ID, Attempt: d.Attempt, State: protocol.StateFailed, Code: code}
}

func (a *Agent) stop(ctx context.Context, h held, refuse func(uuid.UUID, protocol.State, protocol.Code)) {
	for _, d := range append([]Driver{h.driver}, h.others...) {
		callCtx, cancel := context.WithTimeout(ctx, driverTimeout)
		err := d.Stop(callCtx, h.ID)
		cancel()
		if err != nil {
			a.log.Error("driver couldn't stop a workload", "workload", h.ID, "driver", d.Name(), "err", redact.Secrets(err.Error()))
			refuse(h.ID, protocol.StateProgressing, protocol.CodeDriverProblem)
		}
	}
}

// remove removes the workload from every driver holding it, and reports whether that worked.
func (a *Agent) remove(ctx context.Context, h held, refuse func(uuid.UUID, protocol.State, protocol.Code)) bool {
	ok := true
	for _, d := range append([]Driver{h.driver}, h.others...) {
		ok = a.removeFrom(ctx, d, h.ID, refuse) && ok
	}
	return ok
}

// removeOthers removes an endpoint from every driver but keep, and reports whether that worked.
func (a *Agent) removeOthers(ctx context.Context, h held, keep Driver, refuse func(uuid.UUID, protocol.State, protocol.Code)) bool {
	ok := true
	for _, d := range append([]Driver{h.driver}, h.others...) {
		if d != keep {
			ok = a.removeFrom(ctx, d, h.ID, refuse) && ok
		}
	}
	return ok
}

func (a *Agent) removeFrom(ctx context.Context, d Driver, id uuid.UUID, refuse func(uuid.UUID, protocol.State, protocol.Code)) bool {
	if r, ok := d.(OutputReader); ok {
		// Removing it deletes its output, so send the rest first, once it has stopped writing. If
		// that can't all be sent now (the control plane is unreachable, or there's a lot), it's
		// removed on a later try, rather than its output lost unsaid (ADR-0024). One that won't
		// stop is removed regardless: its output would never end.
		callCtx, cancel := context.WithTimeout(ctx, driverTimeout)
		stopErr := d.Stop(callCtx, id)
		cancel()
		if stopErr != nil {
			a.log.Warn("driver couldn't stop a workload before removing it", "workload", id, "driver", d.Name(), "err", redact.Secrets(stopErr.Error()))
		}
		if !a.shipOutput(ctx, []outputSource{{id, r}}, true, false) && stopErr == nil {
			refuse(id, protocol.StateProgressing, protocol.CodeSendingOutput)
			return false
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, driverTimeout)
	err := d.Remove(callCtx, id)
	cancel()
	if err != nil {
		a.log.Error("driver couldn't remove a workload", "workload", id, "driver", d.Name(), "err", redact.Secrets(err.Error()))
		refuse(id, protocol.StateProgressing, protocol.CodeRemovingFailed)
		return false
	}
	return true
}

func active(s protocol.State) bool { return s == protocol.StateProgressing || s == protocol.StateReady }

// freeDevices is how many devices of kind no active workload holds. Only a workload running the
// attempt it's listed to run, with its devices described and held by one driver, counts as holding
// just its own; any other active workload (not listed, being parked or removed, described without
// devices, held twice, or still running another attempt) is assumed to hold them all until it
// stops: two users never share a GPU.
func (a *Agent) freeDevices(kind domain.AcceleratorKind, current map[uuid.UUID]held, listed map[uuid.UUID]protocol.Desired) int {
	a.mu.Lock()
	total := 0
	for _, acc := range a.inv.Accelerators {
		if acc.Kind == kind {
			total += acc.Count
		}
	}
	a.mu.Unlock()
	used := 0
	for id, h := range current {
		if !active(h.State) {
			continue
		}
		d, ok := listed[id]
		switch {
		case !ok || d.Mode != protocol.ModeRun || d.Accelerator == "" || len(h.others) > 0 || h.Attempt != d.Attempt:
			return 0
		case d.Accelerator == kind:
			used += d.Devices
		case kind == domain.AcceleratorApple && h.driver.Name() == domain.DriverMacOSNative && d.Accelerator == domain.AcceleratorCPU:
			used++
		}
	}
	return total - used
}

// codedError is a driver error with a status code for the control plane.
type codedError struct {
	code protocol.Code
	err  error
}

func (e codedError) Error() string { return e.err.Error() }
func (e codedError) Unwrap() error { return e.err }

// WithCode marks a driver's error with the code the control plane should see, such as
// CodeImage when an image couldn't be fetched.
func WithCode(code protocol.Code, err error) error { return codedError{code: code, err: err} }
