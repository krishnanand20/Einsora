package controller_test

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/tensorafi/controlplane/internal/cluster"
	"github.com/krishnanand20/tensorafi/controlplane/internal/cluster/fake"
	"github.com/krishnanand20/tensorafi/controlplane/internal/controller"
	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
	"github.com/krishnanand20/tensorafi/controlplane/internal/service"
	"github.com/krishnanand20/tensorafi/controlplane/internal/store"
	"github.com/krishnanand20/tensorafi/controlplane/internal/testutil"
)

type harness struct {
	t       *testing.T
	st      store.Store
	svc     *service.Service
	ctrl    *controller.Controller
	backend *fake.Backend
	clock   *testutil.Clock
	logs    *bytes.Buffer // the operator's log
}

func newHarness(t *testing.T, capacity map[string]int) *harness {
	st := testutil.SeededStore(t)
	cat := testutil.Catalog(t)
	clock := testutil.NewClock(testutil.T0)
	b := fake.New(capacity)
	logs := &bytes.Buffer{}
	ctrl := controller.New(st, b, cat, controller.Options{ProvisionTimeout: 10 * time.Minute},
		slog.New(slog.NewTextHandler(logs, nil)), clock.Now)
	return &harness{t: t, st: st, svc: service.New(st, cat, clock.Now), ctrl: ctrl, backend: b, clock: clock, logs: logs}
}

func (h *harness) reconcile() {
	h.t.Helper()
	if err := h.ctrl.ReconcileOnce(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) create(p domain.Principal, name string, gpus int) domain.Workload {
	h.t.Helper()
	w, err := h.svc.CreateWorkload(context.Background(), p, service.CreateWorkloadInput{
		Name: name, Kind: domain.KindNotebook, ImageID: "pytorch-notebook", GPUClass: "l40s-48gb", GPUCount: gpus,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return w
}

func (h *harness) state(p domain.Principal, id uuid.UUID) domain.State {
	h.t.Helper()
	w, err := h.svc.GetWorkload(context.Background(), p, id)
	if domain.IsNotFound(err) {
		return domain.StateDeleted
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return w.State
}

// reasons returns the workload's timeline reasons, as its owner sees them.
func (h *harness) reasons(p domain.Principal, id uuid.UUID) []string {
	h.t.Helper()
	events, err := h.svc.WorkloadEvents(context.Background(), p, id)
	if err != nil {
		h.t.Fatal(err)
	}
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Reason
	}
	return out
}

func (h *harness) expect(p domain.Principal, id uuid.UUID, want domain.State) {
	h.t.Helper()
	if got := h.state(p, id); got != want {
		h.t.Fatalf("workload %s: state = %s, want %s", id, got, want)
	}
}

func TestLifecycleWithContention(t *testing.T) {
	h := newHarness(t, map[string]int{"l40s-48gb": 2})
	ctx := context.Background()

	acme := h.create(testutil.Acme, "big", 2)
	h.reconcile() // admitted, applied, observed ready in the same pass
	h.expect(testutil.Acme, acme.ID, domain.StateRunning)

	globex := h.create(testutil.Globex, "small", 1)
	h.reconcile()
	h.expect(testutil.Globex, globex.ID, domain.StateQueued)

	if _, err := h.svc.StopWorkload(ctx, testutil.Acme, acme.ID); err != nil {
		t.Fatal(err)
	}
	h.reconcile() // GPUs are still held while stopping; the park step finishes the stop
	h.expect(testutil.Acme, acme.ID, domain.StateStopped)
	h.expect(testutil.Globex, globex.ID, domain.StateQueued)

	h.reconcile() // capacity freed → globex admitted
	h.expect(testutil.Globex, globex.ID, domain.StateRunning)

	if _, err := h.svc.DeleteWorkload(ctx, testutil.Acme, acme.ID); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	h.expect(testutil.Acme, acme.ID, domain.StateDeleted)
}

func TestStuckProvisioningFailsAfterTimeoutAndReleasesGPUs(t *testing.T) {
	h := newHarness(t, map[string]int{"l40s-48gb": 1})
	w := h.create(testutil.Acme, "stuck", 1)
	h.backend.SetObserved(w.ID, cluster.ObservedProgressing)

	h.reconcile()
	h.expect(testutil.Acme, w.ID, domain.StateProvisioning)

	h.clock.Advance(11 * time.Minute)
	h.reconcile()
	h.expect(testutil.Acme, w.ID, domain.StateFailed)

	// The failed workload no longer holds capacity, so new work is admitted.
	next := h.create(testutil.Acme, "next", 1)
	h.reconcile()
	h.expect(testutil.Acme, next.ID, domain.StateRunning)
}

func TestWorkloadThatVanishesIsMarkedFailed(t *testing.T) {
	h := newHarness(t, map[string]int{"l40s-48gb": 1})
	w := h.create(testutil.Acme, "nb", 1)
	h.reconcile()
	h.expect(testutil.Acme, w.ID, domain.StateRunning)

	h.backend.SetObserved(w.ID, cluster.ObservedAbsent)
	h.reconcile()
	h.expect(testutil.Acme, w.ID, domain.StateFailed)
}

func TestDeletingQueuedWorkloadNeedsNoClusterObjects(t *testing.T) {
	h := newHarness(t, map[string]int{"l40s-48gb": 0})
	w := h.create(testutil.Acme, "never-ran", 1)
	h.reconcile()
	h.expect(testutil.Acme, w.ID, domain.StateQueued)
	if _, err := h.svc.DeleteWorkload(context.Background(), testutil.Acme, w.ID); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	h.expect(testutil.Acme, w.ID, domain.StateDeleted)
}

func TestWaitingReasonShowsFreeGPUsOnlyToTrustedTenants(t *testing.T) {
	h := newHarness(t, map[string]int{"l40s-48gb": 0})
	invited := h.create(testutil.Acme, "a", 1)
	owner := h.create(testutil.Internal, "b", 1)
	h.reconcile()

	want := map[*domain.Workload]struct {
		p      domain.Principal
		reason string
	}{
		&invited: {testutil.Acme, "waiting for l40s-48gb capacity: requested 1 GPU(s), not enough free right now"},
		&owner:   {testutil.Internal, "waiting for l40s-48gb capacity: requested 1 GPU(s), 0 free"},
	}
	for w, tt := range want {
		reasons := h.reasons(tt.p, w.ID)
		if got := reasons[len(reasons)-1]; got != tt.reason {
			t.Errorf("%s: reason = %q, want %q", tt.p.TenantSlug, got, tt.reason)
		}
	}
}

func TestRuntimeErrorsAreNotedOnceWithoutRawDetails(t *testing.T) {
	h := newHarness(t, map[string]int{"l40s-48gb": 1})
	h.backend.FailRun(errors.New(`admission webhook denied t-acme/w-123: storageUri "s3://AKIA:hunter2@models/m"`))
	w := h.create(testutil.Acme, "nb", 1)
	h.reconcile()
	h.reconcile()
	// Placed first, then run (ADR-0016): it holds its GPU while the runtime is retried, and the
	// provisioning timeout bounds how long.
	h.expect(testutil.Acme, w.ID, domain.StateProvisioning)

	reasons := h.reasons(testutil.Acme, w.ID)
	if len(reasons) != 3 || !strings.HasPrefix(reasons[1], "admitted:") || !strings.Contains(reasons[2], "couldn't start it on the runtime; retrying") {
		t.Fatalf("timeline = %q; want the creation, the admission, and one retry note", reasons)
	}
	for _, r := range reasons {
		if strings.Contains(r, "t-acme") || strings.Contains(r, "hunter2") || strings.Contains(r, "webhook") {
			t.Errorf("raw runtime error reached the timeline: %q", r)
		}
	}
	if logs := h.logs.String(); !strings.Contains(logs, "webhook denied") || strings.Contains(logs, "hunter2") {
		t.Errorf("operator log must keep the error with credentials redacted:\n%s", logs)
	}

	h.backend.FailRun(nil)
	h.reconcile() // runs it again
	h.reconcile() // sees it ready
	h.expect(testutil.Acme, w.ID, domain.StateRunning)
}

func TestWorkloadWhoseClassLeftTheCatalogFailsWithAReason(t *testing.T) {
	// No capacity for the retired class, as with a real backend, which only counts catalog classes.
	h := newHarness(t, map[string]int{})
	ctx := context.Background()
	var w domain.Workload
	err := h.st.InTx(ctx, func(q store.Queries) error {
		tn, err := q.TenantBySlug(ctx, testutil.Acme.TenantSlug)
		if err != nil {
			return err
		}
		w = domain.Workload{
			ID: uuid.Must(uuid.NewV7()), TenantID: tn.ID, Name: "old", Kind: domain.KindNotebook,
			State: domain.StatePending, ImageID: "pytorch-notebook", GPUClass: "retired-gpu", GPUCount: 1,
			Params: map[string]string{}, CreatedBy: "test", CreatedAt: testutil.T0, UpdatedAt: testutil.T0, Region: domain.RegionLocal,
		}
		return q.CreateWorkload(ctx, w)
	})
	if err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	h.expect(testutil.Acme, w.ID, domain.StateFailed)
	reasons := h.reasons(testutil.Acme, w.ID)
	if got := reasons[len(reasons)-1]; got != `can't start: GPU class "retired-gpu" is no longer offered by this installation` {
		t.Errorf("reason = %q", got)
	}
}

func TestUnreadableStatusIsNotedOnceThenTimesOut(t *testing.T) {
	h := newHarness(t, map[string]int{"l40s-48gb": 1})
	w := h.create(testutil.Acme, "nb", 1)
	h.backend.FailObserve(errors.New("the server could not find the requested resource"))
	h.reconcile()
	h.reconcile()
	h.expect(testutil.Acme, w.ID, domain.StateProvisioning)
	reasons := h.reasons(testutil.Acme, w.ID)
	notes := 0
	for _, r := range reasons {
		if strings.HasPrefix(r, "couldn't read its status from the runtime; retrying") {
			notes++
		}
	}
	if notes != 1 {
		t.Fatalf("timeline = %q; want exactly one status note", reasons)
	}

	// It must not hold its GPUs forever: the provisioning timeout still applies.
	h.clock.Advance(11 * time.Minute)
	h.reconcile()
	h.expect(testutil.Acme, w.ID, domain.StateFailed)
	reasons = h.reasons(testutil.Acme, w.ID)
	if got := reasons[len(reasons)-1]; got != "not ready after 10m0s: couldn't read its status from the runtime" {
		t.Errorf("reason = %q", got)
	}
}

func TestCapacityErrorsAreNotedForWaitingWorkloads(t *testing.T) {
	h := newHarness(t, map[string]int{"l40s-48gb": 1})
	h.backend.FailCapacity(errors.New("dial tcp 10.0.0.5:6443: connection refused"))
	w := h.create(testutil.Acme, "nb", 1)
	if err := h.ctrl.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("the capacity error must still be returned for the operator's log")
	}
	reasons := h.reasons(testutil.Acme, w.ID)
	if got := reasons[len(reasons)-1]; !strings.HasPrefix(got, "couldn't check for free GPUs on the runtime; retrying") || strings.Contains(got, "10.0.0.5") {
		t.Errorf("reason = %q", got)
	}

	h.backend.FailCapacity(nil)
	h.reconcile()
	h.expect(testutil.Acme, w.ID, domain.StateRunning)
}

// Admission records the placement in the same step as the transition, so nothing ever runs (or
// is accounted) without saying what it runs on.
func TestAdmissionRecordsPlacement(t *testing.T) {
	h := newHarness(t, map[string]int{"l40s-48gb": 1})
	w := h.create(testutil.Acme, "nb", 1)
	if w.Region != domain.RegionLocal || w.PlacedClass != "" {
		t.Fatalf("new workload: region %q, placed %q; want local and not yet placed", w.Region, w.PlacedClass)
	}
	h.reconcile()
	got, err := h.svc.GetWorkload(context.Background(), testutil.Acme, w.ID)
	if err != nil || got.State != domain.StateRunning || got.PlacedClass != "l40s-48gb" || got.MachineID != uuid.Nil {
		t.Fatalf("admitted workload = %+v, %v", got, err)
	}
}

// insertWorkload writes a workload straight to the store, as an older version or another
// region's API would have.
func (h *harness) insertWorkload(p domain.Principal, w domain.Workload) domain.Workload {
	h.t.Helper()
	ctx := context.Background()
	err := h.st.InTx(ctx, func(q store.Queries) error {
		tn, err := q.TenantBySlug(ctx, p.TenantSlug)
		if err != nil {
			return err
		}
		w.ID, w.TenantID = uuid.Must(uuid.NewV7()), tn.ID
		w.Kind, w.ImageID, w.GPUCount = cmp.Or(w.Kind, domain.KindNotebook), cmp.Or(w.ImageID, "pytorch-notebook"), 1
		w.GPUClass = cmp.Or(w.GPUClass, "l40s-48gb")
		w.Params, w.CreatedBy, w.CreatedAt, w.UpdatedAt = map[string]string{}, "test", testutil.T0, testutil.T0
		return q.CreateWorkload(ctx, w)
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return w
}

// A controller reconciles one region (ADR-0009): another region's workloads are never admitted
// here, or counted against this region's GPUs.
func TestControllerStaysInItsRegion(t *testing.T) {
	h := newHarness(t, map[string]int{"l40s-48gb": 1})
	abroad := h.insertWorkload(testutil.Acme, domain.Workload{Name: "abroad", State: domain.StatePending, Region: "eu-fra-1"})
	h.reconcile()
	h.expect(testutil.Acme, abroad.ID, domain.StatePending)

	home := h.create(testutil.Acme, "home", 1)
	h.reconcile()
	h.expect(testutil.Acme, home.ID, domain.StateRunning)
}

// A workload admitted before placements were recorded is adopted onto the class it requested
// (there were no fallbacks then), says so in its timeline, and goes on to run.
func TestLegacyUnplacedWorkloadIsAdopted(t *testing.T) {
	h := newHarness(t, map[string]int{"l40s-48gb": 1})
	legacy := h.insertWorkload(testutil.Acme, domain.Workload{Name: "legacy", State: domain.StateProvisioning, Region: domain.RegionLocal})
	h.backend.SetObserved(legacy.ID, cluster.ObservedReady)
	h.reconcile()

	got, err := h.svc.GetWorkload(context.Background(), testutil.Acme, legacy.ID)
	if err != nil || got.State != domain.StateRunning || got.PlacedClass != "l40s-48gb" {
		t.Fatalf("legacy workload = %+v, %v; want running and placed on its requested class", got, err)
	}
	reasons := h.reasons(testutil.Acme, legacy.ID)
	if !slices.ContainsFunc(reasons, func(r string) bool { return strings.HasPrefix(r, "placed on l40s-48gb: it was admitted before") }) {
		t.Errorf("timeline = %q; the adoption must be recorded", reasons)
	}

	// It holds the only GPU, so nothing else is admitted onto it.
	next := h.create(testutil.Globex, "next", 1)
	h.reconcile()
	h.expect(testutil.Globex, next.ID, domain.StateQueued)
}

// Admission re-checks the launch rules too: an untrusted tenant's native Mac workload that got
// into the queue anyway (the catalog or its tier changed after create) fails with the reason.
func TestAdmissionRechecksLaunchRules(t *testing.T) {
	h := newHarness(t, map[string]int{"apple-silicon-32gb": 1})
	mac := h.insertWorkload(testutil.Acme, domain.Workload{
		Name: "mac", State: domain.StatePending, Region: domain.RegionLocal, GPUClass: "apple-silicon-32gb",
	})
	h.reconcile()
	h.expect(testutil.Acme, mac.ID, domain.StateFailed)
	reasons := h.reasons(testutil.Acme, mac.ID)
	if got := reasons[len(reasons)-1]; got != `can't start: gpu_class "apple-silicon-32gb" isn't available to this tenant` {
		t.Errorf("reason = %q", got)
	}
}

// An endpoint's admission names the model server, so a change of server is never silent (rule 10).
func TestAdmissionNamesTheServer(t *testing.T) {
	h := newHarness(t, map[string]int{"l40s-48gb": 1})
	w, err := h.svc.CreateWorkload(context.Background(), testutil.Acme, service.CreateWorkloadInput{
		Name: "llm", Kind: domain.KindEndpoint, ImageID: "openai-endpoint", GPUClass: "l40s-48gb", GPUCount: 1,
		ModelURI: "hf://Qwen/Qwen2.5-0.5B-Instruct",
	})
	if err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	if !slices.Contains(h.reasons(testutil.Acme, w.ID), "admitted: 1 × l40s-48gb, served by vllm") {
		t.Errorf("timeline = %q; want the server named at admission", h.reasons(testutil.Acme, w.ID))
	}
}

// A running workload the runtime reports as stopped (shut down from inside, or by the runtime)
// isn't running, so it fails with a reason instead of staying running and accounted.
func TestRunningWorkloadThatStopsByItselfFails(t *testing.T) {
	h := newHarness(t, map[string]int{"l40s-48gb": 1})
	w := h.create(testutil.Acme, "nb", 1)
	h.reconcile()
	h.reconcile()
	h.expect(testutil.Acme, w.ID, domain.StateRunning)
	h.backend.SetObserved(w.ID, cluster.ObservedStopped)
	h.reconcile()
	h.expect(testutil.Acme, w.ID, domain.StateFailed)
	if r := h.reasons(testutil.Acme, w.ID); r[len(r)-1] != "it stopped by itself" {
		t.Errorf("reason = %q", r[len(r)-1])
	}
}

// A stopping workload whose runtime reports it failed (such as a machine offline too long) ends
// stopped: it isn't running either way.
func TestStoppingWorkloadThatFailedEndsStopped(t *testing.T) {
	h := newHarness(t, map[string]int{"l40s-48gb": 1})
	w := h.create(testutil.Acme, "nb", 1)
	h.reconcile()
	h.reconcile()
	if _, err := h.svc.StopWorkload(context.Background(), testutil.Acme, w.ID); err != nil {
		t.Fatal(err)
	}
	h.backend.SetObserved(w.ID, cluster.ObservedFailed)
	h.reconcile()
	h.expect(testutil.Acme, w.ID, domain.StateStopped)
	if r := h.reasons(testutil.Acme, w.ID); r[len(r)-1] != "stopped: forced by test" {
		t.Errorf("reason = %q", r[len(r)-1])
	}
}

// One controller reconciles a region at a time: a second stands by, and takes over once the first
// stops and its lease expires.
func TestOneControllerPerRegion(t *testing.T) {
	st := testutil.SeededStore(t)
	cat := testutil.Catalog(t)
	log := slog.New(slog.DiscardHandler)
	opts := controller.Options{ProvisionTimeout: time.Minute, LeaseTTL: 100 * time.Millisecond}
	first := controller.New(st, fake.New(nil), cat, opts, log, nil)
	second := controller.New(st, fake.New(nil), cat, opts, log, nil)
	ctx1, stop1 := context.WithCancel(context.Background())
	ctx2, stop2 := context.WithCancel(context.Background())
	defer stop2()
	go func() { _ = first.Run(ctx1, 10*time.Millisecond) }()
	select {
	case <-first.Leading():
	case <-time.After(2 * time.Second):
		t.Fatal("the first controller never led")
	}
	done := make(chan error, 1)
	go func() { done <- second.Run(ctx2, 10*time.Millisecond) }()
	select {
	case <-second.Leading():
		t.Fatal("a second controller led while the first held the lease")
	case <-time.After(300 * time.Millisecond):
	}
	stop1()
	select {
	case <-second.Leading():
	case <-time.After(2 * time.Second):
		t.Fatal("the standby never took over")
	}
	stop2()
	if err := <-done; err != nil {
		t.Errorf("the standby, stopped: %v", err)
	}
}

// leaseStore lets a test take a controller's lease away, or make renewing it fail.
type leaseStore struct {
	store.Store
	mode atomic.Value // "", "taken", or "failing"
}

type leaseQueries struct {
	store.Queries
	mode string
}

func (q leaseQueries) TakeLease(ctx context.Context, name, holder string, now time.Time, ttl time.Duration) (bool, error) {
	switch q.mode {
	case "taken":
		return false, nil
	case "failing":
		return false, errors.New("database unreachable")
	}
	return q.Queries.TakeLease(ctx, name, holder, now, ttl)
}

func (s *leaseStore) InTx(ctx context.Context, fn func(store.Queries) error) error {
	mode, _ := s.mode.Load().(string)
	return s.Store.InTx(ctx, func(q store.Queries) error { return fn(leaseQueries{q, mode}) })
}

// A controller that loses its lease, or can't renew it in time, stops, so it never reconciles
// beside one that took over.
func TestAControllerThatLosesItsLeaseStops(t *testing.T) {
	for _, mode := range []string{"taken", "failing"} {
		st := &leaseStore{Store: testutil.SeededStore(t)}
		c := controller.New(st, fake.New(nil), testutil.Catalog(t),
			controller.Options{ProvisionTimeout: time.Minute, LeaseTTL: 90 * time.Millisecond}, slog.New(slog.DiscardHandler), nil)
		done := make(chan error, 1)
		go func() { done <- c.Run(context.Background(), time.Hour) }() // passes are rare: renewing is separate
		<-c.Leading()
		st.mode.Store(mode)
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "lost the region's lease") {
				t.Errorf("%s: err = %v", mode, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: the controller kept running after losing its lease", mode)
		}
	}
}

// An unknown status decides nothing, not even the provisioning timeout (the backend reports a lost
// machine as failed in the end).
func TestUnknownStatusDecidesNothing(t *testing.T) {
	h := newHarness(t, map[string]int{"l40s-48gb": 1})
	w := h.create(testutil.Acme, "nb", 1)
	h.backend.SetObserved(w.ID, cluster.ObservedUnknown)
	h.reconcile()
	h.clock.Advance(time.Hour)
	h.reconcile()
	h.expect(testutil.Acme, w.ID, domain.StateProvisioning)
	if r := h.reasons(testutil.Acme, w.ID); r[len(r)-1] != "forced by test" {
		t.Errorf("the last entry = %q; want the backend's reason as a note", r[len(r)-1])
	}
}

// leader records when its controller starts leading.
type leader struct {
	*fake.Backend
	led chan struct{}
}

func (l leader) Lead() { close(l.led) }

// A backend that needs to know is told when its controller starts leading, before its first pass.
func TestBackendsHearWhenTheirControllerLeads(t *testing.T) {
	b := leader{fake.New(nil), make(chan struct{})}
	c := controller.New(testutil.SeededStore(t), b, testutil.Catalog(t), controller.Options{ProvisionTimeout: time.Minute}, slog.New(slog.DiscardHandler), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx, time.Hour) }()
	select {
	case <-b.led:
	case <-time.After(2 * time.Second):
		t.Fatal("the backend never heard its controller lead")
	}
}
