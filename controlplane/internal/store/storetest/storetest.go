// Package storetest is a conformance suite every store.Store implementation must pass.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/store"
)

// Run executes the suite. newStore must return an empty store.
func Run(t *testing.T, newStore func(t *testing.T) store.Store) {
	t.Run("TenantScoping", func(t *testing.T) { testTenantScoping(t, newStore(t)) })
	t.Run("StateUpdateIsCompareAndSwap", func(t *testing.T) { testCAS(t, newStore(t)) })
	t.Run("QuotaAndCapacityAccounting", func(t *testing.T) { testAccounting(t, newStore(t)) })
	t.Run("RollbackOnError", func(t *testing.T) { testRollback(t, newStore(t)) })
	t.Run("EventsAndUsage", func(t *testing.T) { testEventsAndUsage(t, newStore(t)) })
	t.Run("ErrorsAndCrossTenantReads", func(t *testing.T) { testErrorsAndCrossTenant(t, newStore(t)) })
	t.Run("MachinesAndPlacement", func(t *testing.T) { testMachinesAndPlacement(t, newStore(t)) })
	t.Run("LocalAccounts", func(t *testing.T) { testLocalAccounts(t, newStore(t)) })
	t.Run("Leases", func(t *testing.T) { testLeases(t, newStore(t)) })
	t.Run("AccessTickets", func(t *testing.T) { testAccessTickets(t, newStore(t)) })
}

// A ticket opens its workload once, before it expires; a ticket for another tenant's workload is
// refused when it's made.
func testAccessTickets(t *testing.T, st store.Store) {
	ctx := context.Background()
	a, b := tenant(t, st, "alpha"), tenant(t, st, "bravo")
	w := workload(t, st, a, domain.StatePending, 1)
	ticket := domain.AccessTicket{Hash: "h1", TenantID: a.ID, WorkloadID: w.ID, Subject: "alice", ExpiresAt: t0.Add(time.Minute)}
	tx(t, st, func(q store.Queries) error { return q.CreateAccessTicket(ctx, ticket) })
	if code, _ := domain.CodeOf(st.InTx(ctx, func(q store.Queries) error {
		other := ticket
		other.Hash, other.TenantID = "h2", b.ID
		return q.CreateAccessTicket(ctx, other)
	})); code != domain.CodeNotFound {
		t.Errorf("a ticket for another tenant's workload: code %q, want not found", code)
	}
	use := func(hash string, at time.Time) (domain.AccessTicket, error) {
		var got domain.AccessTicket
		err := st.InTx(ctx, func(q store.Queries) error {
			var err error
			got, err = q.UseAccessTicket(ctx, hash, at)
			return err
		})
		return got, err
	}
	got, err := use("h1", t0.Add(30*time.Second))
	if err != nil || got.WorkloadID != w.ID || got.TenantID != a.ID || got.Subject != "alice" {
		t.Errorf("using the ticket = %+v, %v", got, err)
	}
	if _, err := use("h1", t0.Add(31*time.Second)); !domain.IsNotFound(err) {
		t.Errorf("using it twice: %v; want not found", err)
	}
	late := ticket
	late.Hash = "h3"
	tx(t, st, func(q store.Queries) error { return q.CreateAccessTicket(ctx, late) })
	if _, err := use("h3", t0.Add(2*time.Minute)); !domain.IsNotFound(err) {
		t.Errorf("an expired ticket: %v; want not found", err)
	}
	tx(t, st, func(q store.Queries) error { return q.DeleteExpiredAccessTickets(ctx, t0.Add(2*time.Minute)) })
	if _, err := use("missing", t0); !domain.IsNotFound(err) {
		t.Errorf("an unknown ticket: %v", err)
	}
}

// One holder at a time: a lease is free once it expires, and only its holder renews it early.
func testLeases(t *testing.T, st store.Store) {
	ctx := context.Background()
	take := func(holder string, at time.Time) bool {
		t.Helper()
		var ok bool
		tx(t, st, func(q store.Queries) error {
			var err error
			ok, err = q.TakeLease(ctx, "controller:local", holder, at, time.Minute)
			return err
		})
		return ok
	}
	if !take("a", t0) {
		t.Fatal("a free lease wasn't taken")
	}
	if take("b", t0.Add(30*time.Second)) {
		t.Error("b took a's lease before it expired")
	}
	if !take("a", t0.Add(50*time.Second)) {
		t.Error("a couldn't renew its own lease")
	}
	if take("b", t0.Add(100*time.Second)) {
		t.Error("b took a's renewed lease before it expired")
	}
	if !take("b", t0.Add(111*time.Second)) {
		t.Error("b couldn't take an expired lease")
	}
	if take("a", t0.Add(112*time.Second)) {
		t.Error("a took back a lease b holds")
	}
	var other bool
	tx(t, st, func(q store.Queries) error {
		var err error
		other, err = q.TakeLease(ctx, "controller:eu-fra-1", "a", t0.Add(112*time.Second), time.Minute)
		return err
	})
	if !other {
		t.Error("a lease for another region is separate")
	}
	// Only its holder releases a lease; then anyone can take it at once.
	tx(t, st, func(q store.Queries) error { return q.ReleaseLease(ctx, "controller:local", "a") })
	if take("a", t0.Add(113*time.Second)) {
		t.Error("a released b's lease")
	}
	tx(t, st, func(q store.Queries) error { return q.ReleaseLease(ctx, "controller:local", "b") })
	if !take("a", t0.Add(114*time.Second)) {
		t.Error("a released lease wasn't free")
	}
}

// SchemaColumns and SchemaIndexes are every table's columns and every named index that both
// databases must have after all migrations (ADR-0012). sqlitestore and pgstore compare their
// database with them (CheckSchema), so a migration written for only one database fails a test.
var (
	SchemaColumns = map[string][]string{
		"tenants": {"created_at", "id", "name", "slug", "tier"},
		"quotas":  {"gpu_class", "max_gpus", "tenant_id"},
		"machines": {
			"accelerators", "arch", "cpus", "created_at", "id", "last_seen_at", "memory_mib", "name", "os", "region",
			"sharing", "tenant_id",
		},
		"workloads": {
			"attempt", "created_at", "created_by", "driver", "fallback", "fallback_after_s", "gpu_class", "gpu_count", "id",
			"image_id", "kind", "machine_id", "name", "params", "placed_class", "region", "state", "tenant_id", "updated_at",
		},
		"workload_events":  {"at", "from_state", "id", "reason", "to_state", "workload_id"},
		"usage_records":    {"created_at", "gpu_class", "gpu_count", "machine_id", "period_start", "region", "seconds", "tenant_id", "workload_id"},
		"idempotency_keys": {"created_at", "idempotency_key", "request_hash", "response_body", "response_status", "subject", "tenant_id"},
		"leases":           {"expires_at", "holder", "name"},
		"access_tickets":   {"expires_at", "subject", "tenant_id", "ticket_hash", "used_at", "workload_id"},
		"users":            {"created_at", "disabled_at", "email", "id", "password_hash", "tenant_id"},
		"sessions":         {"created_at", "expires_at", "token_hash", "user_id"},
		"api_tokens":       {"created_at", "expires_at", "id", "last_used_at", "name", "prefix", "revoked_at", "token_hash", "user_id"},
	}
	SchemaIndexes = []string{
		"access_tickets_expires_idx", "api_tokens_active_name_idx", "idempotency_keys_created_idx", "machines_region_idx",
		"sessions_expires_idx", "sessions_user_idx", "users_tenant_idx",
		"usage_records_tenant_idx", "workload_events_at_idx", "workload_events_workload_idx",
		"workloads_active_state_idx", "workloads_machine_idx", "workloads_region_state_idx", "workloads_tenant_idx",
	}
)

// CheckSchema compares a database's tables and columns, and its named indexes (not the ones
// backing primary keys or unique constraints), with SchemaColumns and SchemaIndexes.
func CheckSchema(t *testing.T, columns map[string][]string, indexes []string) {
	t.Helper()
	for table := range columns {
		slices.Sort(columns[table])
	}
	if !maps.EqualFunc(columns, SchemaColumns, func(got, want []string) bool { return slices.Equal(got, slices.Sorted(slices.Values(want))) }) {
		t.Errorf("tables and columns = %v,\nwant %v", columns, SchemaColumns)
	}
	want := slices.Sorted(slices.Values(SchemaIndexes))
	slices.Sort(indexes)
	if !slices.Equal(indexes, want) {
		t.Errorf("indexes = %v, want %v", indexes, want)
	}
}

var t0 = time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)

func tx(t *testing.T, st store.Store, fn func(q store.Queries) error) {
	t.Helper()
	if err := st.InTx(context.Background(), fn); err != nil {
		t.Fatal(err)
	}
}

func tenant(t *testing.T, st store.Store, slug string) domain.Tenant {
	t.Helper()
	tn := domain.Tenant{ID: uuid.New(), Slug: slug, Name: slug, Tier: domain.TierUntrusted, CreatedAt: t0}
	tx(t, st, func(q store.Queries) error { return q.CreateTenant(context.Background(), tn) })
	return tn
}

func workload(t *testing.T, st store.Store, tn domain.Tenant, state domain.State, gpus int) domain.Workload {
	t.Helper()
	w := domain.Workload{
		ID: uuid.New(), TenantID: tn.ID, Name: "w", Kind: domain.KindNotebook, State: state,
		ImageID: "img", GPUClass: "l40s-48gb", GPUCount: gpus, Params: map[string]string{},
		CreatedBy: "test", CreatedAt: t0, UpdatedAt: t0, Region: domain.RegionLocal,
	}
	if state.HoldsCapacity() {
		w.PlacedClass = w.GPUClass // as the controller records at admission
	}
	tx(t, st, func(q store.Queries) error { return q.CreateWorkload(context.Background(), w) })
	return w
}

func testTenantScoping(t *testing.T, st store.Store) {
	ctx := context.Background()
	a, b := tenant(t, st, "alpha"), tenant(t, st, "bravo")
	wa := workload(t, st, a, domain.StateRunning, 1)

	tx(t, st, func(q store.Queries) error {
		if _, err := q.GetWorkload(ctx, b.ID, wa.ID); !domain.IsNotFound(err) {
			t.Errorf("tenant B read tenant A's workload: err=%v", err)
		}
		list, err := q.ListWorkloads(ctx, b.ID)
		if err != nil || len(list) != 0 {
			t.Errorf("tenant B list = %v, %v; want empty", list, err)
		}
		got, err := q.GetWorkload(ctx, a.ID, wa.ID)
		if err != nil || got.ID != wa.ID {
			t.Errorf("owner read failed: %v", err)
		}
		return nil
	})

	// Deleted workloads vanish from normal reads but their history stays exportable by the owner only.
	tx(t, st, func(q store.Queries) error {
		return q.UpdateWorkloadState(ctx, wa.ID, domain.StateRunning, domain.StateDeleted, t0)
	})
	tx(t, st, func(q store.Queries) error {
		if _, err := q.GetWorkload(ctx, a.ID, wa.ID); !domain.IsNotFound(err) {
			t.Errorf("deleted workload still returned by GetWorkload: %v", err)
		}
		if got, err := q.GetWorkloadHistory(ctx, a.ID, wa.ID); err != nil || got.State != domain.StateDeleted {
			t.Errorf("owner history read = %+v, %v", got, err)
		}
		if _, err := q.GetWorkloadHistory(ctx, b.ID, wa.ID); !domain.IsNotFound(err) {
			t.Errorf("tenant B read tenant A's deleted workload history: err=%v", err)
		}
		if list, err := q.ListWorkloads(ctx, a.ID); err != nil || len(list) != 0 {
			t.Errorf("deleted workload still listed: %v, %v", list, err)
		}
		if list, err := q.ListWorkloadHistory(ctx, a.ID); err != nil || len(list) != 1 || list[0].State != domain.StateDeleted {
			t.Errorf("owner history list = %v, %v; want the deleted workload", list, err)
		}
		if list, err := q.ListWorkloadHistory(ctx, b.ID); err != nil || len(list) != 0 {
			t.Errorf("tenant B history list = %v, %v; want empty", list, err)
		}
		return nil
	})
}

func testCAS(t *testing.T, st store.Store) {
	ctx := context.Background()
	w := workload(t, st, tenant(t, st, "alpha"), domain.StatePending, 1)
	err := st.InTx(ctx, func(q store.Queries) error {
		return q.UpdateWorkloadState(ctx, w.ID, domain.StateRunning, domain.StateStopping, t0)
	})
	if c, _ := domain.CodeOf(err); c != domain.CodeConflict {
		t.Fatalf("stale update: err=%v, want conflict", err)
	}
	tx(t, st, func(q store.Queries) error {
		return q.UpdateWorkloadState(ctx, w.ID, domain.StatePending, domain.StateQueued, t0.Add(time.Minute))
	})
}

func testAccounting(t *testing.T, st store.Store) {
	ctx := context.Background()
	a := tenant(t, st, "alpha")
	tx(t, st, func(q store.Queries) error {
		return q.SetQuota(ctx, domain.Quota{TenantID: a.ID, GPUClass: "l40s-48gb", MaxGPUs: 4})
	})
	workload(t, st, a, domain.StateQueued, 1)  // quota yes, capacity no
	workload(t, st, a, domain.StateRunning, 2) // quota yes, capacity yes
	workload(t, st, a, domain.StateStopped, 4) // neither
	workload(t, st, a, domain.StateDeleting, 1)

	tx(t, st, func(q store.Queries) error {
		limit, err := q.LockQuota(ctx, a.ID, "l40s-48gb")
		if err != nil || limit != 4 {
			t.Errorf("LockQuota = %d, %v", limit, err)
		}
		if limit, _ := q.LockQuota(ctx, a.ID, "unset"); limit != 0 {
			t.Errorf("unset quota = %d, want 0", limit)
		}
		if held, _ := q.GPUsHeld(ctx, a.ID, "l40s-48gb"); held != 3 {
			t.Errorf("GPUsHeld = %d, want 3", held)
		}
		if alloc, _ := q.GPUsAllocated(ctx, domain.RegionLocal); alloc["l40s-48gb"] != 3 {
			t.Errorf("GPUsAllocated = %v, want 3 (running 2 + deleting 1)", alloc)
		}
		return nil
	})
}

func testRollback(t *testing.T, st store.Store) {
	ctx := context.Background()
	boom := errors.New("boom")
	err := st.InTx(ctx, func(q store.Queries) error {
		if err := q.CreateTenant(ctx, domain.Tenant{ID: uuid.New(), Slug: "ghost", Name: "g", Tier: domain.TierTrusted, CreatedAt: t0}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	tx(t, st, func(q store.Queries) error {
		if _, err := q.TenantBySlug(ctx, "ghost"); !domain.IsNotFound(err) {
			t.Errorf("tenant survived rollback: %v", err)
		}
		return nil
	})
}

func testEventsAndUsage(t *testing.T, st store.Store) {
	ctx := context.Background()
	a := tenant(t, st, "alpha")
	other := tenant(t, st, "bravo")
	w := workload(t, st, a, domain.StateRunning, 2)
	tx(t, st, func(q store.Queries) error {
		for i, to := range []domain.State{domain.StatePending, domain.StateProvisioning, domain.StateRunning} {
			if err := q.AppendEvent(ctx, domain.Event{WorkloadID: w.ID, To: to, Reason: "test", At: t0.Add(time.Duration(i) * time.Minute)}); err != nil {
				return err
			}
		}
		// A note: a timeline entry without a state change.
		return q.AppendEvent(ctx, domain.Event{WorkloadID: w.ID, From: domain.StateRunning, To: domain.StateRunning, Reason: "note", At: t0.Add(3 * time.Minute)})
	})
	tx(t, st, func(q store.Queries) error {
		evs, err := q.Events(ctx, w.ID)
		if err != nil || len(evs) != 4 || evs[2].To != domain.StateRunning || evs[0].From != "" || evs[3].From != domain.StateRunning {
			t.Errorf("events = %+v, %v", evs, err)
		}
		if mine, err := q.EventsForWorkload(ctx, a.ID, w.ID); err != nil || len(mine) != 4 {
			t.Errorf("EventsForWorkload(owner) = %+v, %v", mine, err)
		}
		if theirs, err := q.EventsForWorkload(ctx, other.ID, w.ID); err != nil || len(theirs) != 0 {
			t.Errorf("EventsForWorkload(other tenant) = %+v, %v; want none", theirs, err)
		}
		ws, err := q.WorkloadsForMetering(ctx, t0.Add(24*time.Hour))
		if err != nil || len(ws) != 1 {
			t.Errorf("running workload should be metered even without recent events: %v, %v", ws, err)
		}
		rec := domain.UsageRecord{WorkloadID: w.ID, PeriodStart: t0, TenantID: a.ID, GPUClass: "l40s-48gb", GPUCount: 2, Seconds: 3600, Region: domain.RegionLocal}
		if ok, err := q.InsertUsageIfAbsent(ctx, rec); !ok || err != nil {
			t.Errorf("first insert = %v, %v", ok, err)
		}
		rec.Seconds = 1
		if ok, err := q.InsertUsageIfAbsent(ctx, rec); ok || err != nil {
			t.Errorf("duplicate insert = %v, %v; want no-op", ok, err)
		}
		got, err := q.Usage(ctx, a.ID, t0, t0.Add(time.Hour))
		if err != nil || len(got) != 1 || got[0].Seconds != 3600 || got[0].GPUSeconds() != 7200 {
			t.Errorf("usage = %+v, %v", got, err)
		}
		mine, err := q.UsageForWorkload(ctx, a.ID, w.ID)
		if err != nil || len(mine) != 1 || mine[0].WorkloadID != w.ID {
			t.Errorf("UsageForWorkload(owner) = %+v, %v", mine, err)
		}
		if theirs, err := q.UsageForWorkload(ctx, other.ID, w.ID); err != nil || len(theirs) != 0 {
			t.Errorf("UsageForWorkload(other tenant) = %+v, %v; want none", theirs, err)
		}
		return nil
	})
}

func testErrorsAndCrossTenant(t *testing.T, st store.Store) {
	ctx := context.Background()
	a, b := tenant(t, st, "alpha"), tenant(t, st, "bravo")
	w := workload(t, st, a, domain.StateRunning, 2)
	tx(t, st, func(q store.Queries) error {
		if err := q.SetQuota(ctx, domain.Quota{TenantID: a.ID, GPUClass: "l40s-48gb", MaxGPUs: 4}); err != nil {
			return err
		}
		_, err := q.InsertUsageIfAbsent(ctx, domain.UsageRecord{WorkloadID: w.ID, PeriodStart: t0, TenantID: a.ID,
			GPUClass: "l40s-48gb", GPUCount: 2, Seconds: 60, Region: domain.RegionLocal})
		return err
	})

	orphan := w
	orphan.ID, orphan.TenantID = uuid.New(), uuid.New()
	cases := []struct {
		name string
		fn   func(q store.Queries) error
		want domain.Code
	}{
		{"duplicate tenant slug", func(q store.Queries) error {
			return q.CreateTenant(ctx, domain.Tenant{ID: uuid.New(), Slug: "alpha", Name: "x", Tier: domain.TierTrusted, CreatedAt: t0})
		}, domain.CodeConflict},
		{"duplicate workload", func(q store.Queries) error { return q.CreateWorkload(ctx, w) }, domain.CodeConflict},
		{"quota for a missing tenant", func(q store.Queries) error {
			return q.SetQuota(ctx, domain.Quota{TenantID: uuid.New(), GPUClass: "l40s-48gb", MaxGPUs: 1})
		}, domain.CodeNotFound},
		{"workload for a missing tenant", func(q store.Queries) error { return q.CreateWorkload(ctx, orphan) }, domain.CodeNotFound},
		{"event for a missing workload", func(q store.Queries) error {
			return q.AppendEvent(ctx, domain.Event{WorkloadID: uuid.New(), To: domain.StatePending, Reason: "x", At: t0})
		}, domain.CodeNotFound},
		{"workload without a region", func(q store.Queries) error {
			noRegion := w
			noRegion.ID, noRegion.Region = uuid.New(), ""
			return q.CreateWorkload(ctx, noRegion)
		}, domain.CodeInvalid},
		{"usage without a region", func(q store.Queries) error {
			_, err := q.InsertUsageIfAbsent(ctx, domain.UsageRecord{WorkloadID: w.ID, PeriodStart: t0.Add(time.Hour),
				TenantID: a.ID, GPUClass: "l40s-48gb", GPUCount: 2, Seconds: 60})
			return err
		}, domain.CodeInvalid},
	}
	for _, c := range cases {
		// Each in its own transaction: Postgres aborts a transaction after an error.
		if code, _ := domain.CodeOf(st.InTx(ctx, c.fn)); code != c.want {
			t.Errorf("%s: code = %q, want %q", c.name, code, c.want)
		}
	}

	tx(t, st, func(q store.Queries) error {
		// The owner sees its quota, holdings, and usage, so the empty results below mean scoping,
		// not a broken query.
		if qs, err := q.Quotas(ctx, a.ID); err != nil || len(qs) != 1 {
			t.Errorf("owner quotas = %v, %v", qs, err)
		}
		if held, err := q.GPUsHeld(ctx, a.ID, "l40s-48gb"); err != nil || held != 2 {
			t.Errorf("owner GPUsHeld = %d, %v", held, err)
		}
		if u, err := q.Usage(ctx, a.ID, t0.Add(-time.Hour), t0.Add(time.Hour)); err != nil || len(u) != 1 {
			t.Errorf("owner usage = %v, %v", u, err)
		}
		if qs, err := q.Quotas(ctx, b.ID); err != nil || len(qs) != 0 {
			t.Errorf("tenant B quotas = %v, %v; want none", qs, err)
		}
		if limit, err := q.LockQuota(ctx, b.ID, "l40s-48gb"); err != nil || limit != 0 {
			t.Errorf("tenant B LockQuota = %d, %v; want 0", limit, err)
		}
		if held, err := q.GPUsHeld(ctx, b.ID, "l40s-48gb"); err != nil || held != 0 {
			t.Errorf("tenant B GPUsHeld = %d, %v; want 0", held, err)
		}
		if u, err := q.Usage(ctx, b.ID, t0.Add(-time.Hour), t0.Add(time.Hour)); err != nil || len(u) != 0 {
			t.Errorf("tenant B usage = %v, %v; want none", u, err)
		}
		return nil
	})
}

func testMachinesAndPlacement(t *testing.T, st store.Store) {
	ctx := context.Background()
	a, b := tenant(t, st, "alpha"), tenant(t, st, "bravo")
	m := domain.Machine{
		ID: uuid.New(), TenantID: a.ID, Name: "studio-mac", Region: "eu-fra-1", OS: "darwin", Arch: "arm64",
		CPUs: 16, MemoryMiB: 131072,
		Accelerators: []domain.Accelerator{{Kind: domain.AcceleratorApple, Model: "Apple M3 Max", Count: 1, MemoryMiB: 131072}},
		Sharing:      domain.SharingInvite, LastSeenAt: t0.Add(time.Minute), CreatedAt: t0,
	}
	quiet := m
	quiet.ID, quiet.Name, quiet.LastSeenAt, quiet.Accelerators = uuid.New(), "linux-box", time.Time{}, nil
	tx(t, st, func(q store.Queries) error {
		if err := q.CreateMachine(ctx, m); err != nil {
			return err
		}
		return q.CreateMachine(ctx, quiet)
	})

	cases := []struct {
		name string
		fn   func(q store.Queries) error
		want domain.Code
	}{
		{"duplicate machine name in a tenant", func(q store.Queries) error {
			dup := m
			dup.ID = uuid.New()
			return q.CreateMachine(ctx, dup)
		}, domain.CodeConflict},
		{"machine for a missing tenant", func(q store.Queries) error {
			orphan := m
			orphan.ID, orphan.TenantID = uuid.New(), uuid.New()
			return q.CreateMachine(ctx, orphan)
		}, domain.CodeNotFound},
		{"same name in another tenant is fine", func(q store.Queries) error {
			other := m
			other.ID, other.TenantID = uuid.New(), b.ID
			return q.CreateMachine(ctx, other)
		}, ""},
		{"tenant B reads tenant A's machine", func(q store.Queries) error {
			_, err := q.GetMachine(ctx, b.ID, m.ID)
			return err
		}, domain.CodeNotFound},
	}
	for _, c := range cases {
		if code, _ := domain.CodeOf(st.InTx(ctx, c.fn)); code != c.want {
			t.Errorf("%s: code = %q, want %q", c.name, code, c.want)
		}
	}

	w := domain.Workload{
		ID: uuid.New(), TenantID: a.ID, Name: "w", Kind: domain.KindEndpoint, State: domain.StatePending,
		ImageID: "img", GPUClass: "l40s-48gb", GPUCount: 1, Params: map[string]string{}, CreatedBy: "test",
		CreatedAt: t0, UpdatedAt: t0, Region: "eu-fra-1", Fallback: []string{"a100-80gb", "cpu"}, FallbackAfter: 10 * time.Minute,
	}
	tx(t, st, func(q store.Queries) error {
		got, err := q.GetMachine(ctx, a.ID, m.ID)
		if err != nil || got.Region != m.Region || got.Sharing != m.Sharing || !got.LastSeenAt.Equal(m.LastSeenAt) ||
			got.CPUs != 16 || got.MemoryMiB != 131072 || len(got.Accelerators) != 1 || got.Accelerators[0] != m.Accelerators[0] {
			t.Errorf("machine round trip = %+v, %v", got, err)
		}
		list, err := q.ListMachines(ctx, a.ID)
		if err != nil || len(list) != 2 || list[0].Name != "linux-box" || !list[0].LastSeenAt.IsZero() || len(list[0].Accelerators) != 0 {
			t.Errorf("owner machines = %+v, %v; want both, by name, with a never-seen machine's time zero", list, err)
		}
		if list, err := q.ListMachines(ctx, b.ID); err != nil || len(list) != 1 {
			t.Errorf("tenant B machines = %+v, %v; want only its own", list, err)
		}
		return q.CreateWorkload(ctx, w)
	})

	// Placement: recorded at admission, and capacity is counted by the class that runs.
	tx(t, st, func(q store.Queries) error {
		got, err := q.GetWorkload(ctx, a.ID, w.ID)
		if err != nil || got.Region != "eu-fra-1" || got.PlacedClass != "" || got.MachineID != uuid.Nil ||
			!slices.Equal(got.Fallback, w.Fallback) || got.FallbackAfter != 10*time.Minute {
			t.Errorf("unplaced workload round trip = %+v, %v", got, err)
		}
		if err := q.SetPlacement(ctx, w.ID, "a100-80gb", m.ID, domain.DriverMacOSNative); err != nil {
			return err
		}
		return q.UpdateWorkloadState(ctx, w.ID, domain.StatePending, domain.StateProvisioning, t0)
	})
	tx(t, st, func(q store.Queries) error {
		got, err := q.GetWorkload(ctx, a.ID, w.ID)
		if err != nil || got.PlacedClass != "a100-80gb" || got.MachineID != m.ID || got.Driver != domain.DriverMacOSNative || got.Attempt != 1 {
			t.Errorf("placed workload = %+v, %v; want the placement and its first attempt", got, err)
		}
		if on, err := q.WorkloadsOnMachine(ctx, m.ID); err != nil || len(on) != 1 || on[0].ID != w.ID {
			t.Errorf("WorkloadsOnMachine = %+v, %v; want the placed workload", on, err)
		}
		if on, err := q.WorkloadsOnMachine(ctx, quiet.ID); err != nil || len(on) != 0 {
			t.Errorf("WorkloadsOnMachine(another machine) = %+v, %v; want none", on, err)
		}
		if alloc, err := q.GPUsAllocated(ctx, "eu-fra-1"); err != nil || alloc["a100-80gb"] != 1 || alloc["l40s-48gb"] != 0 {
			t.Errorf("GPUsAllocated = %v, %v; want the placed class counted, not the requested one", alloc, err)
		}
		// Regions are separate: the local controller neither sees nor counts eu-fra-1's workloads.
		if alloc, err := q.GPUsAllocated(ctx, domain.RegionLocal); err != nil || len(alloc) != 0 {
			t.Errorf("GPUsAllocated(local) = %v, %v; want nothing from another region", alloc, err)
		}
		if ws, err := q.WorkloadsInStates(ctx, domain.RegionLocal, domain.StateProvisioning); err != nil || len(ws) != 0 {
			t.Errorf("WorkloadsInStates(local) = %v, %v; want nothing from another region", ws, err)
		}
		if ws, err := q.WorkloadsInStates(ctx, "eu-fra-1", domain.StateProvisioning); err != nil || len(ws) != 1 {
			t.Errorf("WorkloadsInStates(eu-fra-1) = %v, %v", ws, err)
		}
		rec := domain.UsageRecord{WorkloadID: w.ID, PeriodStart: t0, TenantID: a.ID, GPUClass: "a100-80gb",
			GPUCount: 1, Seconds: 60, Region: "eu-fra-1", MachineID: m.ID}
		if _, err := q.InsertUsageIfAbsent(ctx, rec); err != nil {
			return err
		}
		usage, err := q.UsageForWorkload(ctx, a.ID, w.ID)
		if err != nil || len(usage) != 1 || usage[0].Region != "eu-fra-1" || usage[0].MachineID != m.ID {
			t.Errorf("usage with location = %+v, %v", usage, err)
		}
		return nil
	})
	for name, fn := range map[string]func(q store.Queries) error{
		"unknown machine": func(q store.Queries) error {
			return q.SetPlacement(ctx, w.ID, "a100-80gb", uuid.New(), domain.DriverLibvirt)
		},
		"unknown workload": func(q store.Queries) error { return q.SetPlacement(ctx, uuid.New(), "a100-80gb", uuid.Nil, "") },
	} {
		if !domain.IsNotFound(st.InTx(ctx, fn)) {
			t.Errorf("SetPlacement with an %s must be not found", name)
		}
	}

	// A machine may host only workloads in its own region, and only its owner's unless it's in a
	// community pool (domain.Machine.CanHost).
	elsewhere := m
	elsewhere.ID, elsewhere.Name, elsewhere.Region = uuid.New(), "local-box", domain.RegionLocal
	bravos := m
	bravos.ID, bravos.TenantID, bravos.Name = uuid.New(), b.ID, "bravo-box"
	tx(t, st, func(q store.Queries) error {
		if err := q.CreateMachine(ctx, elsewhere); err != nil {
			return err
		}
		return q.CreateMachine(ctx, bravos)
	})
	community := bravos
	community.ID, community.Name, community.Sharing = uuid.New(), "bravo-pool", domain.SharingCommunity
	tx(t, st, func(q store.Queries) error { return q.CreateMachine(ctx, community) })
	for name, tt := range map[string]struct {
		machine uuid.UUID
		driver  string
		want    domain.Code
	}{
		"machine in another region":                    {elsewhere.ID, domain.DriverLibvirt, domain.CodeInvalid},
		"another tenant's invite machine":              {bravos.ID, domain.DriverLibvirt, domain.CodeForbidden},
		"weak isolation on another tenant's community": {community.ID, domain.DriverMacOSNative, domain.CodeForbidden},
		"an unknown driver":                            {m.ID, "made-up", domain.CodeInvalid},
		"a driver without a machine":                   {uuid.Nil, domain.DriverDocker, domain.CodeInvalid},
		"strong isolation on a community machine":      {community.ID, domain.DriverMacOSVZ, ""},
	} {
		code, _ := domain.CodeOf(st.InTx(ctx, func(q store.Queries) error {
			return q.SetPlacement(ctx, w.ID, "a100-80gb", tt.machine, tt.driver)
		}))
		if code != tt.want {
			t.Errorf("SetPlacement on a %s: code = %q, want %q", name, code, tt.want)
		}
	}

	// An agent's report updates only the inventory and when it was last seen, never the owner,
	// name, region, or sharing.
	tx(t, st, func(q store.Queries) error {
		report := quiet
		report.TenantID, report.Name, report.Region, report.Sharing = b.ID, "renamed", domain.RegionLocal, domain.SharingCommunity
		report.OS, report.Arch, report.CPUs, report.MemoryMiB, report.LastSeenAt = "linux", "amd64", 32, 65536, t0.Add(time.Hour)
		report.Accelerators = []domain.Accelerator{{Kind: domain.AcceleratorNVIDIA, Model: "NVIDIA L40S", Count: 2, MemoryMiB: 46068}}
		if err := q.UpdateMachineInventory(ctx, report); err != nil {
			return err
		}
		got, err := q.GetMachine(ctx, a.ID, quiet.ID)
		if err != nil || got.Name != quiet.Name || got.Region != quiet.Region || got.Sharing != quiet.Sharing ||
			got.OS != "linux" || got.CPUs != 32 || got.MemoryMiB != 65536 || !got.LastSeenAt.Equal(t0.Add(time.Hour)) ||
			len(got.Accelerators) != 1 || got.Accelerators[0].Count != 2 {
			t.Errorf("after an inventory report = %+v, %v", got, err)
		}
		if byID, err := q.MachineByID(ctx, quiet.ID); err != nil || byID.Name != quiet.Name || byID.CPUs != 32 {
			t.Errorf("MachineByID = %+v, %v", byID, err)
		}
		if _, err := q.MachineByID(ctx, uuid.New()); !domain.IsNotFound(err) {
			t.Errorf("MachineByID(unknown): %v; want not found", err)
		}
		if err := q.UpdateMachineInventory(ctx, domain.Machine{ID: uuid.New()}); !domain.IsNotFound(err) {
			t.Errorf("inventory for an unknown machine: %v; want not found", err)
		}
		inRegion, err := q.MachinesInRegion(ctx, "eu-fra-1")
		if err != nil || len(inRegion) != 5 {
			t.Errorf("MachinesInRegion(eu-fra-1) = %d machines, %v; want 5, across tenants", len(inRegion), err)
		}
		for _, x := range inRegion {
			if x.Region != "eu-fra-1" {
				t.Errorf("MachinesInRegion returned %s in %s", x.Name, x.Region)
			}
		}
		return nil
	})

	// A workload holding capacity without a recorded placement (admitted by an older version) is
	// counted under the class it requested, never left out.
	legacy := workload(t, st, a, domain.StatePending, 2)
	tx(t, st, func(q store.Queries) error {
		return q.UpdateWorkloadState(ctx, legacy.ID, domain.StatePending, domain.StateDeleting, t0)
	})
	tx(t, st, func(q store.Queries) error {
		if alloc, err := q.GPUsAllocated(ctx, domain.RegionLocal); err != nil || alloc["l40s-48gb"] != 2 {
			t.Errorf("GPUsAllocated with an unplaced deleting workload = %v, %v; want it counted (2)", alloc, err)
		}
		return nil
	})
}

// UpgradeFixture is a version-1 database's contents, as SQL for an upgrade test: workloads whose
// event histories decide whether migration 00003 treats them as admitted. now is the dialect's
// expression for a timestamp ('…' text for SQLite, now() for Postgres).
func UpgradeFixture(now string) string {
	const tenant = "00000000-0000-0000-0000-0000000000a1"
	histories := []struct {
		id, name, state string
		events          []string
	}{
		{"00000000-0000-0000-0000-000000000001", "run", "running", []string{"pending", "provisioning", "running"}},
		{"00000000-0000-0000-0000-000000000002", "wait", "queued", []string{"pending", "queued"}},
		{"00000000-0000-0000-0000-000000000003", "restarted", "queued",
			[]string{"pending", "provisioning", "running", "stopping", "stopped", "pending", "queued"}},
		{"00000000-0000-0000-0000-000000000004", "early-delete", "deleting", []string{"pending", "deleting"}},
		{"00000000-0000-0000-0000-000000000005", "old-endpoint", "queued", []string{"pending", "queued"}},
	}
	sql := fmt.Sprintf("INSERT INTO tenants (id, slug, name, tier, created_at) VALUES ('%s', 'acme', 'Acme', 'trusted', %s);\n", tenant, now)
	for _, h := range histories {
		kind, image := "notebook", "i"
		if h.name == "old-endpoint" {
			kind, image = "endpoint", "vllm" // the endpoint image's id before roadmap step 4
		}
		sql += fmt.Sprintf("INSERT INTO workloads (id, tenant_id, name, kind, state, image_id, gpu_class, gpu_count, created_by, created_at, updated_at)"+
			" VALUES ('%s', '%s', '%s', '%s', '%s', '%s', 'l40s-48gb', 1, 's', %s, %s);\n", h.id, tenant, h.name, kind, h.state, image, now, now)
		from := "NULL"
		for _, to := range h.events {
			sql += fmt.Sprintf("INSERT INTO workload_events (workload_id, from_state, to_state, reason, at) VALUES ('%s', %s, '%s', 'test', %s);\n",
				h.id, from, to, now)
			from = "'" + to + "'"
		}
	}
	return sql
}

// BadUsageAfter00002 is a usage record as the 00002 gap could write it: no class.
func BadUsageAfter00002(periodStart string) string {
	return fmt.Sprintf("INSERT INTO usage_records (workload_id, period_start, tenant_id, gpu_class, gpu_count, seconds)"+
		" VALUES ('00000000-0000-0000-0000-000000000003', %s, '00000000-0000-0000-0000-0000000000a1', '', 1, 2400);", periodStart)
}

// CheckUpgraded checks a database built from UpgradeFixture and BadUsageAfter00002 after it was
// migrated to the latest version.
func CheckUpgraded(t *testing.T, st store.Store) {
	t.Helper()
	ctx := context.Background()
	tx(t, st, func(q store.Queries) error {
		ws, err := q.WorkloadsInStates(ctx, domain.RegionLocal, domain.AllStates()...)
		if err != nil {
			return err
		}
		placed := map[string]string{}
		for _, w := range ws {
			if w.Region != domain.RegionLocal {
				t.Errorf("%s: region %q, want local", w.Name, w.Region)
			}
			if w.Name == "old-endpoint" && w.ImageID != "openai-endpoint" {
				t.Errorf("old endpoint image = %q, want it renamed to openai-endpoint", w.ImageID)
			}
			placed[w.Name] = w.PlacedClass
		}
		want := map[string]string{"run": "l40s-48gb", "wait": "", "restarted": "l40s-48gb", "early-delete": "", "old-endpoint": ""}
		if !maps.Equal(placed, want) {
			t.Errorf("placed classes after upgrade = %v, want %v (admitted means it ever moved to provisioning)", placed, want)
		}
		// running (placed) + deleting (unplaced, counted under its requested class)
		if alloc, err := q.GPUsAllocated(ctx, domain.RegionLocal); err != nil || alloc["l40s-48gb"] != 2 {
			t.Errorf("GPUsAllocated after upgrade = %v, %v; want 2", alloc, err)
		}
		tn, err := q.TenantBySlug(ctx, "acme")
		if err != nil {
			return err
		}
		usage, err := q.Usage(ctx, tn.ID, time.Time{}, time.Now().Add(24*time.Hour))
		if err != nil || len(usage) != 1 || usage[0].GPUClass != "l40s-48gb" {
			t.Errorf("usage after upgrade = %+v, %v; the empty class must be repaired", usage, err)
		}
		return nil
	})
}

func testLocalAccounts(t *testing.T, st store.Store) {
	ctx := context.Background()
	a := tenant(t, st, "alpha")
	// Placeholder hashes: the store keeps whatever it's given; hashing is the service's job.
	alice := domain.User{ID: uuid.New(), TenantID: a.ID, Email: "alice@alpha.test", PasswordHash: "$argon2id$x", CreatedAt: t0} //nolint:gosec // test fixture
	bob := domain.User{ID: uuid.New(), TenantID: a.ID, Email: "bob@alpha.test", PasswordHash: "$argon2id$y", CreatedAt: t0}     //nolint:gosec // test fixture
	tx(t, st, func(q store.Queries) error {
		if err := q.LockUsers(ctx); err != nil {
			return err
		}
		if n, err := q.CountUsers(ctx); err != nil || n != 0 {
			t.Errorf("CountUsers before = %d, %v", n, err)
		}
		if err := q.CreateUser(ctx, alice); err != nil {
			return err
		}
		if err := q.LockUser(ctx, alice.ID); err != nil {
			t.Errorf("LockUser: %v", err)
		}
		return q.CreateUser(ctx, bob)
	})
	for name, tt := range map[string]struct {
		fn   func(q store.Queries) error
		want domain.Code
	}{
		"duplicate email": {func(q store.Queries) error {
			dup := alice
			dup.ID = uuid.New()
			return q.CreateUser(ctx, dup)
		}, domain.CodeConflict},
		"lock a missing user": {func(q store.Queries) error {
			return q.LockUser(ctx, uuid.New())
		}, domain.CodeNotFound},
		"user for a missing tenant": {func(q store.Queries) error {
			orphan := alice
			orphan.ID, orphan.TenantID, orphan.Email = uuid.New(), uuid.New(), "orphan@x.test"
			return q.CreateUser(ctx, orphan)
		}, domain.CodeNotFound},
		"password for an unknown user": {func(q store.Queries) error { return q.SetPasswordHash(ctx, uuid.New(), "h") }, domain.CodeNotFound},
	} {
		if code, _ := domain.CodeOf(st.InTx(ctx, tt.fn)); code != tt.want {
			t.Errorf("%s: code = %q, want %q", name, code, tt.want)
		}
	}

	tx(t, st, func(q store.Queries) error {
		if n, err := q.CountUsers(ctx); err != nil || n != 2 {
			t.Errorf("CountUsers = %d, %v", n, err)
		}
		got, err := q.UserByEmail(ctx, "alice@alpha.test")
		if err != nil || got.ID != alice.ID || !got.DisabledAt.IsZero() || !got.CreatedAt.Equal(t0) {
			t.Errorf("UserByEmail = %+v, %v", got, err)
		}
		if _, err := q.UserByEmail(ctx, "nobody@alpha.test"); !domain.IsNotFound(err) {
			t.Errorf("unknown email: %v", err)
		}
		if err := q.SetPasswordHash(ctx, alice.ID, "$argon2id$new"); err != nil {
			return err
		}
		if got, err := q.UserByID(ctx, alice.ID); err != nil || got.PasswordHash != "$argon2id$new" {
			t.Errorf("after SetPasswordHash = %+v, %v", got, err)
		}

		// Sessions: sign out everywhere but here, and sweep expired ones.
		for i, hash := range []string{"s-here", "s-phone", "s-old"} {
			expires := t0.Add(time.Hour)
			if hash == "s-old" {
				expires = t0.Add(-time.Minute)
			}
			if err := q.CreateSession(ctx, domain.Session{TokenHash: hash, UserID: alice.ID, CreatedAt: t0.Add(time.Duration(i)), ExpiresAt: expires}); err != nil {
				return err
			}
		}
		if err := q.CreateSession(ctx, domain.Session{TokenHash: "s-bob", UserID: bob.ID, CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)}); err != nil {
			return err
		}
		if err := q.DeleteExpiredSessions(ctx, t0); err != nil {
			return err
		}
		if _, err := q.SessionByTokenHash(ctx, "s-old"); !domain.IsNotFound(err) {
			t.Errorf("expired session survived the sweep: %v", err)
		}
		if err := q.DeleteOtherSessions(ctx, alice.ID, "s-here"); err != nil {
			return err
		}
		if s, err := q.SessionByTokenHash(ctx, "s-here"); err != nil || s.UserID != alice.ID || !s.ExpiresAt.Equal(t0.Add(time.Hour)) {
			t.Errorf("kept session = %+v, %v", s, err)
		}
		if _, err := q.SessionByTokenHash(ctx, "s-phone"); !domain.IsNotFound(err) {
			t.Errorf("other session survived: %v", err)
		}
		if _, err := q.SessionByTokenHash(ctx, "s-bob"); err != nil {
			t.Errorf("another user's session was deleted: %v", err)
		}
		if err := q.DeleteSession(ctx, "s-here"); err != nil {
			return err
		}
		if _, err := q.SessionByTokenHash(ctx, "s-here"); !domain.IsNotFound(err) {
			t.Errorf("deleted session survived: %v", err)
		}
		return nil
	})

	// API tokens: names are unique among live tokens, and users see and revoke only their own.
	tok := domain.APIToken{ID: uuid.New(), UserID: alice.ID, Name: "laptop", Prefix: "tsk_abcd", TokenHash: "h-laptop", CreatedAt: t0, ExpiresAt: t0.Add(24 * time.Hour)}
	tx(t, st, func(q store.Queries) error { return q.CreateAPIToken(ctx, tok) })
	dupName := tok
	dupName.ID, dupName.TokenHash = uuid.New(), "h-other"
	if code, _ := domain.CodeOf(st.InTx(ctx, func(q store.Queries) error { return q.CreateAPIToken(ctx, dupName) })); code != domain.CodeConflict {
		t.Errorf("duplicate live token name: code = %q, want conflict", code)
	}
	tx(t, st, func(q store.Queries) error {
		got, err := q.APITokenByHash(ctx, "h-laptop")
		if err != nil || got.ID != tok.ID || !got.ExpiresAt.Equal(tok.ExpiresAt) || !got.LastUsedAt.IsZero() || !got.RevokedAt.IsZero() {
			t.Errorf("APITokenByHash = %+v, %v", got, err)
		}
		if err := q.TouchAPIToken(ctx, tok.ID, t0.Add(time.Minute)); err != nil {
			return err
		}
		if list, err := q.ListAPITokens(ctx, alice.ID); err != nil || len(list) != 1 || !list[0].LastUsedAt.Equal(t0.Add(time.Minute)) {
			t.Errorf("alice's tokens = %+v, %v", list, err)
		}
		if list, err := q.ListAPITokens(ctx, bob.ID); err != nil || len(list) != 0 {
			t.Errorf("bob sees tokens that aren't his: %+v, %v", list, err)
		}
		return nil
	})
	if !domain.IsNotFound(st.InTx(ctx, func(q store.Queries) error { return q.RevokeAPIToken(ctx, bob.ID, tok.ID, t0) })) {
		t.Error("revoking another user's token must be not found")
	}
	tx(t, st, func(q store.Queries) error {
		if err := q.RevokeAPIToken(ctx, alice.ID, tok.ID, t0.Add(time.Hour)); err != nil {
			return err
		}
		if err := q.RevokeAPIToken(ctx, alice.ID, tok.ID, t0.Add(2*time.Hour)); err != nil { // a no-op
			return err
		}
		got, err := q.APITokenByHash(ctx, "h-laptop")
		if err != nil || !got.RevokedAt.Equal(t0.Add(time.Hour)) {
			t.Errorf("revoked token = %+v, %v; want the first revocation time kept", got, err)
		}
		return q.CreateAPIToken(ctx, dupName) // the name is free again
	})
}
