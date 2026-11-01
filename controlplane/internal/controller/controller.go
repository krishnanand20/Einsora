// Package controller reconciles workload intent in the store with the runtime (Kubernetes or node
// agents, through cluster.Backend). It's the only component with runtime access, and the one place
// that decides where workloads run (placement.go). Each pass is idempotent: runtime calls are
// idempotent, and state changes are compare-and-swap transitions.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/catalog"
	"github.com/krishnanand20/einsora/controlplane/internal/cluster"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/service"
	"github.com/krishnanand20/einsora/controlplane/internal/store"
)

// errNotInCatalog marks a workload whose GPU class or image was removed from the catalog.
// Retrying can't fix it; the message is safe to show the owner.
var errNotInCatalog = errors.New("is no longer offered by this installation")

type Options struct {
	// Region is the one region this controller reconciles (ADR-0009). It never sees, counts, or
	// admits another region's workloads. Empty means local, the region of a Solo install.
	Region           domain.Region
	ProvisionTimeout time.Duration
	// LeaseTTL is how long the region's lease lasts between renewals; zero means three intervals,
	// at least 30 seconds.
	LeaseTTL time.Duration
}

type Controller struct {
	store   store.Store
	backend cluster.Backend
	catalog *catalog.Catalog
	opts    Options
	log     *slog.Logger
	now     func() time.Time

	holder  string        // this process, in the region's lease
	leading chan struct{} // closed once this controller holds the lease
	lead    sync.Once
}

func New(st store.Store, b cluster.Backend, cat *catalog.Catalog, opts Options, log *slog.Logger, now func() time.Time) *Controller {
	if now == nil {
		now = time.Now
	}
	if opts.Region == "" {
		opts.Region = domain.RegionLocal
	}
	return &Controller{store: st, backend: b, catalog: cat, opts: opts, log: log, now: now,
		holder: uuid.NewString(), leading: make(chan struct{})}
}

// Leading is closed once this controller holds its region's lease and reconciles. Work that must
// happen once per region, such as running this machine's in-process agent, waits for it.
func (c *Controller) Leading() <-chan struct{} { return c.leading }

// errLostLease stops a controller that no longer holds its region's lease.
var errLostLease = errors.New("lost the region's lease (another controller took over, or it couldn't be renewed in time)")

// Run reconciles every interval until ctx is cancelled. Only one controller reconciles a region
// at a time (ADR-0016): another waits as a standby until the lease is free. The leader renews its
// lease in the background, every third of its time to live; if it can't renew before the lease
// would run out, or finds another holding it, it cancels what it's doing and stops with an error,
// so it never reconciles beside the one that took over, and whatever supervises it starts it again
// as a standby. Call Release once everything that depends on leading has stopped.
func (c *Controller) Run(ctx context.Context, interval time.Duration) error {
	ttl := c.leaseTTL(interval)
	renewed, err := c.waitForLease(ctx, ttl, interval)
	if err != nil || ctx.Err() != nil {
		return err
	}
	leading, stepDown := context.WithCancelCause(ctx)
	defer stepDown(nil)
	go c.renew(leading, stepDown, renewed, ttl)
	c.lead.Do(func() {
		if l, ok := c.backend.(cluster.Leader); ok {
			l.Lead()
		}
		c.log.Info("controller started", "region", c.opts.Region, "interval", interval)
		close(c.leading)
	})
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := c.ReconcileOnce(leading); err != nil && leading.Err() == nil {
			c.log.Error("reconcile pass failed", "err", service.RedactSecrets(err.Error()))
		}
		select {
		case <-leading.Done():
			if cause := context.Cause(leading); errors.Is(cause, errLostLease) {
				return fmt.Errorf("region %s: %w", c.opts.Region, cause)
			}
			return nil
		case <-ticker.C:
		}
	}
}

func (c *Controller) leaseTTL(interval time.Duration) time.Duration {
	if c.opts.LeaseTTL > 0 {
		return c.opts.LeaseTTL
	}
	return max(3*interval, 30*time.Second)
}

func (c *Controller) leaseName() string { return "controller:" + string(c.opts.Region) }

// waitForLease stands by until this controller holds the lease, and returns when it took it.
func (c *Controller) waitForLease(ctx context.Context, ttl, interval time.Duration) (time.Time, error) {
	waiting := false
	for {
		at := c.now()
		held, err := c.takeLease(ctx, at, ttl)
		switch {
		case held:
			return at, nil
		case err != nil && ctx.Err() == nil:
			c.log.Error("couldn't take the region's lease; retrying", "region", c.opts.Region, "err", service.RedactSecrets(err.Error()))
		case err == nil && !waiting:
			c.log.Warn("another controller reconciles this region; standing by until its lease is free", "region", c.opts.Region)
			waiting = true
		}
		select {
		case <-ctx.Done():
			return time.Time{}, nil
		case <-time.After(interval):
		}
	}
}

// renew keeps the lease while leading. It steps down when another holds it, or when it hasn't
// renewed for two thirds of the lease's time, before the lease could run out under it.
func (c *Controller) renew(ctx context.Context, stepDown context.CancelCauseFunc, renewed time.Time, ttl time.Duration) {
	ticker := time.NewTicker(ttl / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		at := c.now()
		held, err := c.takeLease(ctx, at, ttl)
		switch {
		case held:
			renewed = at
		case err == nil:
			stepDown(errLostLease)
			return
		case c.now().Sub(renewed) >= ttl*2/3:
			c.log.Error("couldn't renew the region's lease in time", "err", service.RedactSecrets(err.Error()))
			stepDown(errLostLease)
			return
		}
	}
}

// Release gives up the region's lease, so a standby can take over at once. Call it after Run has
// returned and everything that depended on leading (the in-process agent) has stopped.
func (c *Controller) Release(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := c.store.InTx(ctx, func(q store.Queries) error { return q.ReleaseLease(ctx, c.leaseName(), c.holder) }); err != nil {
		c.log.Warn("couldn't release the region's lease; a standby takes over when it expires", "err", service.RedactSecrets(err.Error()))
	}
}

func (c *Controller) takeLease(ctx context.Context, at time.Time, ttl time.Duration) (bool, error) {
	var held bool
	err := c.store.InTx(ctx, func(q store.Queries) error {
		var err error
		held, err = q.TakeLease(ctx, c.leaseName(), c.holder, at.UTC(), ttl)
		return err
	})
	return held, err
}

// ReconcileOnce runs one pass. Per-workload failures are logged and retried next pass;
// only failures that stop a whole step are returned.
func (c *Controller) ReconcileOnce(ctx context.Context) error {
	return errors.Join(c.admit(ctx), c.observe(ctx), c.park(ctx), c.remove(ctx))
}

// admit places pending and queued workloads where there's room, then runs them. Capacity comes from
// the backend; allocation comes from the store, because every workload goes through Einsora.
func (c *Controller) admit(ctx context.Context) error {
	var waiting, holding []domain.Workload
	var machines []domain.Machine
	err := c.store.InTx(ctx, func(q store.Queries) error {
		var err error
		if waiting, err = q.WorkloadsInStates(ctx, c.opts.Region, domain.StatePending, domain.StateQueued); err != nil || len(waiting) == 0 {
			return err
		}
		if holding, err = q.WorkloadsInStates(ctx, c.opts.Region, domain.StatesWhere(domain.State.HoldsCapacity)...); err != nil {
			return err
		}
		machines, err = q.MachinesInRegion(ctx, c.opts.Region)
		return err
	})
	if err != nil || len(waiting) == 0 {
		return wrap("admit", err)
	}
	capacity, err := c.backend.Capacity(ctx)
	if err != nil {
		for _, w := range waiting {
			c.note(ctx, w, "couldn't check for free GPUs on the runtime; retrying. The operator's log has the details.")
		}
		return wrap("admit: capacity", err)
	}
	if capacity.Settling {
		return nil // machines are still reconnecting: decide nothing on their absence
	}
	free := newPool(c.catalog, capacity, machines, holding)
	// FIFO with backfill: a large request at the head does not block smaller ones behind it.
	// TODO(phase-3): add aging so large requests cannot starve.
	for _, w := range waiting {
		// Checked before capacity: a class that left the catalog has no capacity either, so the
		// workload would otherwise wait forever under a misleading reason. The launch rules are
		// checked again too, in case the catalog or the tenant's tier changed since create.
		runtime, err := c.checkLaunch(ctx, w)
		if errors.Is(err, errRetryLater) {
			continue
		}
		if err != nil {
			c.transition(ctx, w, domain.StateFailed, "can't start: "+userMessage(err))
			continue
		}
		g, _ := c.catalog.GPUClass(w.GPUClass) // checkLaunch found it
		// Placed on the requested class. Fallback placement (ADR-0010) and ADR-0014's sharing rules
		// will choose here too, in the same place.
		pl, n, why, ok := free.choose(w, g)
		c.log.Debug("admission check", "workload", w.ID, "gpu_class", w.GPUClass, "requested", w.GPUCount,
			"free", n, "placed", ok, "machine", pl.machine, "driver", pl.driver)
		if !ok {
			if w.State == domain.StatePending {
				if why == "" {
					why = c.waitingReason(ctx, w, n)
				}
				c.transition(ctx, w, domain.StateQueued, why)
			}
			continue
		}
		free.take(pl, w)
		img, _ := c.catalog.Image(w.ImageID)
		runtime, ok = img.LaunchRuntime(g.Accelerator, pl.driver)
		if !ok {
			c.transition(ctx, w, domain.StateFailed, "can't start: its image has no validated runtime for the selected driver")
			continue
		}
		placed, ok := c.place(ctx, w, pl, runtime.Server)
		if !ok {
			continue // counted anyway; the next pass sees what happened
		}
		if err := c.run(ctx, placed); err != nil {
			c.retryLater(ctx, placed, "start it on the runtime", err)
		}
	}
	return nil
}

// observe promotes ready workloads to running and fails broken or stuck ones.
func (c *Controller) observe(ctx context.Context) error {
	ws, err := c.inStates(ctx, domain.StateProvisioning, domain.StateRunning)
	if err != nil {
		return wrap("observe", err)
	}
	for _, w := range ws {
		ref, err := c.ref(ctx, w)
		var obs cluster.Observed
		var reason string
		if err == nil {
			obs, reason, err = c.backend.Observe(ctx, ref)
		}
		if err != nil {
			// A status that can't be read must not hold GPUs forever: the provisioning timeout
			// still applies.
			if w.State == domain.StateProvisioning && c.now().Sub(w.UpdatedAt) > c.opts.ProvisionTimeout {
				c.logError("observe failed", w, err)
				c.fail(ctx, w, fmt.Sprintf("not ready after %s: couldn't read its status from the runtime", c.opts.ProvisionTimeout))
				continue
			}
			c.retryLater(ctx, w, "read its status from the runtime", err)
			continue
		}
		if w.PlacedClass == "" {
			// Admitted by a version that didn't record placement. Fallbacks didn't exist then, so it
			// runs on the class it requested; record that, so it can be counted and accounted.
			if !c.adoptPlacement(ctx, &w) {
				continue
			}
		}
		switch {
		case obs == cluster.ObservedUnknown:
			// Nothing is decided on it, not even the provisioning timeout: the runtime can't say,
			// such as while its machine reconnects after a restart. A machine that stays away is
			// reported failed by the backend in the end. The owner hears why it's waiting.
			c.noteIf(ctx, w, reason)
			continue
		case obs == cluster.ObservedFailed:
			c.fail(ctx, w, "workload failed: "+reason)
			continue
		case w.State == domain.StateRunning && obs == cluster.ObservedAbsent:
			c.fail(ctx, w, "it disappeared from the runtime")
			continue
		case w.State == domain.StateRunning && obs == cluster.ObservedStopped:
			// Shut down from inside, or stopped by the runtime: it isn't running, so it mustn't be
			// accounted as running.
			c.fail(ctx, w, "it stopped by itself")
			continue
		case w.State == domain.StateProvisioning && obs == cluster.ObservedAbsent:
			// Placed, but the runtime doesn't have it: starting it failed, or hasn't reached the
			// runtime yet. Run is idempotent, so ask again.
			if err := c.run(ctx, w); err != nil {
				c.retryLater(ctx, w, "start it on the runtime", err)
			}
		case w.State == domain.StateProvisioning && obs == cluster.ObservedReady:
			if c.transition(ctx, w, domain.StateRunning, "ready") {
				continue
			}
		}
		// Still provisioning, including when the move to running didn't happen: a workload must
		// never hold GPUs forever.
		if w.State == domain.StateProvisioning && c.now().Sub(w.UpdatedAt) > c.opts.ProvisionTimeout {
			why := fmt.Sprintf("not ready after %s", c.opts.ProvisionTimeout)
			if reason != "" {
				why += ": " + reason
			}
			c.fail(ctx, w, why)
		}
	}
	return nil
}

// adoptPlacement records the requested class as the placement of a workload that holds capacity
// without one, and says so in its timeline.
func (c *Controller) adoptPlacement(ctx context.Context, w *domain.Workload) bool {
	err := c.store.InTx(ctx, func(q store.Queries) error {
		return q.SetPlacement(ctx, w.ID, w.GPUClass, w.MachineID, w.Driver)
	})
	if err != nil {
		c.logError("adopt placement failed", *w, err)
		return false
	}
	w.PlacedClass = w.GPUClass
	c.note(ctx, *w, fmt.Sprintf("placed on %s: it was admitted before placements were recorded", w.GPUClass))
	return true
}

// park frees the GPUs of stopping workloads while keeping their disks.
func (c *Controller) park(ctx context.Context) error {
	ws, err := c.inStates(ctx, domain.StateStopping)
	if err != nil {
		return wrap("park", err)
	}
	for _, w := range ws {
		if err := c.release(ctx, w); err != nil {
			c.retryLater(ctx, w, "stop it on the runtime", err)
			continue
		}
		ref, err := c.ref(ctx, w)
		var obs cluster.Observed
		var reason string
		if err == nil {
			obs, reason, err = c.backend.Observe(ctx, ref)
		}
		if err != nil {
			c.retryLater(ctx, w, "read its status from the runtime", err)
			continue
		}
		switch obs {
		case cluster.ObservedStopped, cluster.ObservedAbsent:
			c.transition(ctx, w, domain.StateStopped, "stopped")
		case cluster.ObservedFailed:
			// It isn't running either way, such as when its machine has been offline too long; it's
			// parked when the machine returns.
			c.transition(ctx, w, domain.StateStopped, "stopped: "+reason)
		case cluster.ObservedUnknown:
			c.noteIf(ctx, w, reason)
		case cluster.ObservedProgressing:
			c.noteAgentProblem(ctx, w, reason)
		}
	}
	return nil
}

// remove deletes cluster objects (including volumes) for deleting workloads.
func (c *Controller) remove(ctx context.Context) error {
	ws, err := c.inStates(ctx, domain.StateDeleting)
	if err != nil {
		return wrap("remove", err)
	}
	for _, w := range ws {
		ref, err := c.ref(ctx, w)
		if err == nil {
			err = c.backend.Delete(ctx, ref)
		}
		if err != nil {
			c.retryLater(ctx, w, "delete it from the runtime", err)
			continue
		}
		obs, reason, err := c.backend.Observe(ctx, ref)
		if err != nil {
			c.retryLater(ctx, w, "read its status from the runtime", err)
			continue
		}
		switch obs {
		case cluster.ObservedAbsent:
			c.transition(ctx, w, domain.StateDeleted, "deleted")
		case cluster.ObservedUnknown:
			c.noteIf(ctx, w, reason)
		case cluster.ObservedProgressing:
			c.noteAgentProblem(ctx, w, reason)
		}
	}
	return nil
}

// noteIf notes reason unless there's nothing to say.
func (c *Controller) noteIf(ctx context.Context, w domain.Workload, reason string) {
	if reason != "" {
		c.note(ctx, w, reason)
	}
}

// noteAgentProblem notes why a stop or delete on a node agent hasn't finished, such as a driver
// that couldn't stop or remove it (rule 14). Kubernetes' progress while stopping ("stopping the
// VM") is routine, so it isn't noted.
func (c *Controller) noteAgentProblem(ctx context.Context, w domain.Workload, reason string) {
	if w.MachineID != uuid.Nil {
		c.noteIf(ctx, w, reason)
	}
}

// run asks the runtime to run a placed workload.
func (c *Controller) run(ctx context.Context, w domain.Workload) error {
	spec, err := c.spec(ctx, w)
	if err != nil {
		return err
	}
	return c.backend.Run(ctx, spec)
}

// release parks a workload's GPUs: VMs halt and notebooks scale to zero (disks kept);
// endpoints are deleted because they have nothing worth keeping.
func (c *Controller) release(ctx context.Context, w domain.Workload) error {
	if w.Kind == domain.KindEndpoint {
		ref, err := c.ref(ctx, w)
		if err != nil {
			return err
		}
		return c.backend.Delete(ctx, ref)
	}
	spec, err := c.spec(ctx, w)
	if err != nil {
		return err
	}
	return c.backend.Park(ctx, spec)
}

// fail releases GPUs before marking the workload failed, so failed workloads never
// hold capacity the scheduler believes is free.
func (c *Controller) fail(ctx context.Context, w domain.Workload, reason string) {
	if err := c.release(ctx, w); err != nil {
		// The failure's reason is recorded with the Failed transition. Leaving it out of the note
		// keeps one note per problem, even when the runtime's status keeps changing.
		c.retryLater(ctx, w, "release its GPUs on the runtime after it failed", err)
		return
	}
	c.transition(ctx, w, domain.StateFailed, reason)
}

// waitingReason says why a workload waits for capacity. Only trusted tenants (a machine's owner
// and their team) see how many GPUs are free. For invited and community users that number would
// show when other people use the machines (docs/architecture/tenancy-and-isolation.md).
func (c *Controller) waitingReason(ctx context.Context, w domain.Workload, free int) string {
	units := "GPU(s)"
	if g, ok := c.catalog.GPUClass(w.GPUClass); ok && g.Accelerator == domain.AcceleratorCPU {
		units = "unit(s)"
	}
	if t, err := c.tenant(ctx, w); err == nil && t.Tier == domain.TierTrusted {
		return fmt.Sprintf("waiting for %s capacity: requested %d %s, %d free", w.GPUClass, w.GPUCount, units, free)
	}
	return fmt.Sprintf("waiting for %s capacity: requested %d %s, not enough free right now", w.GPUClass, w.GPUCount, units)
}

// retryLater handles a runtime action that failed and will be retried next pass. The operator's
// log gets the error; the owner's timeline gets a note saying so, never the raw error, which can
// name internal objects or echo inputs.
func (c *Controller) retryLater(ctx context.Context, w domain.Workload, action string, err error) {
	c.logError("couldn't "+action, w, err)
	if errors.Is(err, errNotInCatalog) {
		c.note(ctx, w, fmt.Sprintf("couldn't %s: %v. Retrying won't help until the operator restores it.", action, err))
		return
	}
	c.note(ctx, w, fmt.Sprintf("couldn't %s; retrying. The operator's log has the details under this workload's ID.", action))
}

// note adds a timeline entry without a state change, unless the workload has moved on or its
// latest entry already says the same, so a retry loop adds one entry rather than one per pass.
func (c *Controller) note(ctx context.Context, w domain.Workload, reason string) {
	reason = service.CleanReason(reason)
	err := c.store.InTx(ctx, func(q store.Queries) error {
		current, err := q.GetWorkloadHistory(ctx, w.TenantID, w.ID)
		if err != nil || current.State != w.State {
			return err
		}
		events, err := q.Events(ctx, w.ID)
		if err != nil {
			return err
		}
		if n := len(events); n > 0 && events[n-1].Reason == reason {
			return nil
		}
		return service.Note(ctx, q, w, reason, c.now())
	})
	if err != nil {
		c.log.Warn("note skipped", "workload", w.ID, "err", err)
	}
}

// logError writes a failure to the operator's log. Runtime errors can echo inputs such as a model
// URI, so credentials are redacted first.
func (c *Controller) logError(msg string, w domain.Workload, err error) {
	c.log.Error(msg, "workload", w.ID, "tenant", w.TenantID, "err", service.RedactSecrets(err.Error()))
}

// place records where the workload was placed and moves it to provisioning, in one transaction,
// so a workload is never provisioning without a placement. The reason says the machine and driver
// on agents, and the model server for endpoints, so where and how it runs is never silent (rule 10).
// It returns the workload as placed.
func (c *Controller) place(ctx context.Context, w domain.Workload, pl placement, server string) (domain.Workload, bool) {
	reason := fmt.Sprintf("admitted: %d × %s", w.GPUCount, pl.class)
	if pl.machineID != uuid.Nil {
		reason += fmt.Sprintf(" on %s with %s", pl.machine, pl.driver)
	}
	if server != "" {
		reason += ", served by " + server
	}
	if g, ok := c.catalog.GPUClass(pl.class); ok && pl.driver == domain.DriverMacOSNative && g.Accelerator == domain.AcceleratorCPU {
		reason += "; Metal remains accessible to native code, so the Apple GPU is reserved exclusively"
	}
	ok := c.transitionWith(ctx, w, domain.StateProvisioning, reason,
		func(q store.Queries, w *domain.Workload) error {
			if err := q.SetPlacement(ctx, w.ID, pl.class, pl.machineID, pl.driver); err != nil {
				return err
			}
			w.PlacedClass, w.MachineID, w.Driver = pl.class, pl.machineID, pl.driver
			return nil
		})
	// As the store now has it: placed, provisioning, and a new attempt.
	w.PlacedClass, w.MachineID, w.Driver, w.State = pl.class, pl.machineID, pl.driver, domain.StateProvisioning
	w.Attempt++
	return w, ok
}

func (c *Controller) transition(ctx context.Context, w domain.Workload, to domain.State, reason string) bool {
	return c.transitionWith(ctx, w, to, reason, nil)
}

// transitionWith runs before (if any) and the transition in one transaction.
func (c *Controller) transitionWith(ctx context.Context, w domain.Workload, to domain.State, reason string,
	before func(store.Queries, *domain.Workload) error,
) bool {
	reason = service.CleanReason(reason)
	from := w.State
	err := c.store.InTx(ctx, func(q store.Queries) error {
		if before != nil {
			if err := before(q, &w); err != nil {
				return err
			}
		}
		return service.Transition(ctx, q, &w, to, reason, c.now())
	})
	if err != nil {
		c.log.Warn("transition skipped", "workload", w.ID, "from", from, "to", to, "err", service.RedactSecrets(err.Error()))
		// A conflict is a concurrent change, such as a delete during provisioning; the next pass
		// sees the new state, and the timeline already shows it. Anything else is a decision that
		// didn't happen, so the owner hears about it (rule 14).
		if code, _ := domain.CodeOf(err); code != domain.CodeConflict {
			c.note(ctx, w, fmt.Sprintf("couldn't move it to %s; retrying. The operator's log has the details under this workload's ID.", to))
		}
		return false
	}
	c.log.Info("workload transitioned", "workload", w.ID, "tenant", w.TenantID, "from", from, "to", to, "reason", reason)
	return true
}

func (c *Controller) inStates(ctx context.Context, states ...domain.State) ([]domain.Workload, error) {
	var ws []domain.Workload
	err := c.store.InTx(ctx, func(q store.Queries) error {
		var err error
		ws, err = q.WorkloadsInStates(ctx, c.opts.Region, states...)
		return err
	})
	return ws, err
}

func (c *Controller) tenant(ctx context.Context, w domain.Workload) (domain.Tenant, error) {
	var t domain.Tenant
	err := c.store.InTx(ctx, func(q store.Queries) error {
		var err error
		t, err = q.TenantByID(ctx, w.TenantID)
		return err
	})
	return t, err
}

// ref names the workload in its runtime.
func (c *Controller) ref(ctx context.Context, w domain.Workload) (cluster.Ref, error) {
	t, err := c.tenant(ctx, w)
	return cluster.Ref{ID: w.ID, TenantSlug: t.Slug, Kind: w.Kind, MachineID: w.MachineID, State: w.State, Attempt: w.Attempt}, err
}

// spec is everything the runtime needs to run w as placed.
func (c *Controller) spec(ctx context.Context, w domain.Workload) (cluster.Spec, error) {
	var t domain.Tenant
	var quotas []domain.Quota
	err := c.store.InTx(ctx, func(q store.Queries) error {
		var err error
		if t, err = q.TenantByID(ctx, w.TenantID); err != nil {
			return err
		}
		quotas, err = q.Quotas(ctx, w.TenantID)
		return err
	})
	if err != nil {
		return cluster.Spec{}, err
	}
	spec := cluster.Spec{Workload: w, TenantSlug: t.Slug, Tier: t.Tier, Quotas: quotas}
	placed := w
	placed.GPUClass = classOf(w)
	gpu, img, runtime, err := c.catalogEntries(placed)
	if err != nil && w.MachineID != uuid.Nil {
		// An agent works out what to run from the store, and parks or removes by ID alone, so a
		// catalog entry removed since doesn't stop a release there.
		return spec, nil
	}
	if err != nil {
		return cluster.Spec{}, err
	}
	spec.Class, spec.Image, spec.Runtime = gpu, img, runtime
	if selected, ok := img.LaunchRuntime(gpu.Accelerator, w.Driver); ok {
		spec.Runtime = selected
	} else if w.MachineID == uuid.Nil {
		return cluster.Spec{}, errors.New("its image has no validated runtime for its driver")
	}
	return spec, nil
}

// errRetryLater means a check couldn't be made this pass; nothing is decided.
var errRetryLater = errors.New("retry later")

// checkLaunch re-applies catalog.CheckLaunch at admission, with the tenant's current tier.
func (c *Controller) checkLaunch(ctx context.Context, w domain.Workload) (catalog.Runtime, error) {
	gpu, img, _, err := c.catalogEntries(w)
	if err != nil {
		return catalog.Runtime{}, err
	}
	t, err := c.tenant(ctx, w)
	if err != nil {
		c.logError("admission: tenant lookup failed", w, err)
		return catalog.Runtime{}, errRetryLater
	}
	return catalog.CheckLaunch(t.Tier, gpu, img, w.Kind, w.Region)
}

// userMessage is the part of an error that's safe to show the workload's owner: a domain error's
// message, or the error itself for the controller's own fixed-vocabulary errors.
func userMessage(err error) string {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Message
	}
	return err.Error()
}

// catalogEntries looks up the workload's class, its image, and the image's runtime for that
// class's accelerator. An operator may have removed any of them since the workload was created.
func (c *Controller) catalogEntries(w domain.Workload) (catalog.GPUClass, catalog.Image, catalog.Runtime, error) {
	gpu, ok := c.catalog.GPUClass(w.GPUClass)
	if !ok {
		return catalog.GPUClass{}, catalog.Image{}, catalog.Runtime{}, fmt.Errorf("GPU class %q %w", w.GPUClass, errNotInCatalog)
	}
	img, ok := c.catalog.Image(w.ImageID)
	if !ok {
		return catalog.GPUClass{}, catalog.Image{}, catalog.Runtime{}, fmt.Errorf("image %q %w", w.ImageID, errNotInCatalog)
	}
	runtime, ok := img.RuntimeFor(gpu.Accelerator)
	if !ok {
		return catalog.GPUClass{}, catalog.Image{}, catalog.Runtime{}, fmt.Errorf("image %q on %s %w", w.ImageID, gpu.Accelerator, errNotInCatalog)
	}
	return gpu, img, runtime, nil
}

func wrap(step string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", step, err)
}
