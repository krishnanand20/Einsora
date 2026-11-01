package service_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/tensorafi/controlplane/internal/credential"
	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
	"github.com/krishnanand20/tensorafi/controlplane/internal/service"
	"github.com/krishnanand20/tensorafi/controlplane/internal/store"
	"github.com/krishnanand20/tensorafi/controlplane/internal/testutil"
)

// running stores a workload of acme's in the given state, as the controller would leave it: on a
// machine of acme's, or on none (Kubernetes).
func running(t *testing.T, st store.Store, kind domain.WorkloadKind, state domain.State, onMachine bool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	err := st.InTx(ctx, func(q store.Queries) error {
		tn, err := q.TenantBySlug(ctx, testutil.Acme.TenantSlug)
		if err != nil {
			return err
		}
		var machine uuid.UUID
		if onMachine {
			machine = uuid.New()
			if err := q.CreateMachine(ctx, domain.Machine{ID: machine, TenantID: tn.ID, Name: "m-" + machine.String()[:8],
				Region: domain.RegionLocal, OS: "darwin", Arch: "arm64", CPUs: 8, MemoryMiB: 16384,
				Sharing: domain.SharingPrivate, CreatedAt: testutil.T0}); err != nil {
				return err
			}
		}
		return q.CreateWorkload(ctx, domain.Workload{ID: id, TenantID: tn.ID, Name: "w", Kind: kind, State: state,
			ImageID: "pytorch-notebook", GPUClass: "l40s-48gb", GPUCount: 1, CreatedBy: "t", MachineID: machine,
			CreatedAt: testutil.T0, UpdatedAt: testutil.T0, Region: domain.RegionLocal})
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestOpeningAWorkload(t *testing.T) {
	svc, st := setup(t)
	ctx := context.Background()
	id := running(t, st, domain.KindNotebook, domain.StateRunning, true)

	ticket, expires, err := svc.OpenWorkload(ctx, testutil.Acme, id)
	if err != nil || !strings.HasPrefix(ticket, "tat_") || !expires.Equal(testutil.T0.Add(service.AccessTicketTTL)) {
		t.Fatalf("OpenWorkload = %q, %v, %v", ticket, expires, err)
	}
	_ = st.InTx(ctx, func(q store.Queries) error {
		got, err := q.UseAccessTicket(ctx, credential.HashToken(ticket), testutil.T0)
		if err != nil || got.WorkloadID != id || got.Subject != testutil.Acme.Actor() {
			t.Errorf("the stored ticket = %+v, %v", got, err)
		}
		if _, err := q.UseAccessTicket(ctx, credential.HashToken(ticket), testutil.T0); !domain.IsNotFound(err) {
			t.Errorf("a ticket used twice: %v; want not found", err)
		}
		evs, _ := q.Events(ctx, id)
		if len(evs) != 1 || evs[0].From != evs[0].To || evs[0].Reason != "opening in the browser requested by "+testutil.Acme.Actor() {
			t.Errorf("timeline = %+v; want a note saying who opened it", evs)
		}
		return nil
	})

	// Another tenant can't, and can't tell it exists.
	_, _, err = svc.OpenWorkload(ctx, testutil.Globex, id)
	wantCode(t, err, domain.CodeNotFound)
	_, _, err = svc.OpenWorkload(ctx, testutil.Acme, uuid.New())
	wantCode(t, err, domain.CodeNotFound)
}

func TestOnlyRunningNotebooksOnAMachineOpen(t *testing.T) {
	svc, st := setup(t)
	ctx := context.Background()
	tests := []struct {
		name      string
		kind      domain.WorkloadKind
		state     domain.State
		onMachine bool
		want      domain.Code
	}{
		{"a notebook still starting", domain.KindNotebook, domain.StateProvisioning, true, domain.CodeConflict},
		{"a stopped notebook", domain.KindNotebook, domain.StateStopped, true, domain.CodeConflict},
		{"an endpoint still starting", domain.KindEndpoint, domain.StateProvisioning, true, domain.CodeConflict},
		{"a notebook on Kubernetes", domain.KindNotebook, domain.StateRunning, false, domain.CodeInvalid},
	}
	for _, tt := range tests {
		id := running(t, st, tt.kind, tt.state, tt.onMachine)
		if _, _, err := svc.OpenWorkload(ctx, testutil.Acme, id); !hasCode(err, tt.want) {
			t.Errorf("%s: %v; want %s", tt.name, err, tt.want)
		}
	}
}

func TestEndpointAccessDoesNotIssueABrowserTicket(t *testing.T) {
	svc, st := setup(t)
	id := running(t, st, domain.KindEndpoint, domain.StateRunning, true)
	ticket, expires, err := svc.OpenWorkload(context.Background(), testutil.Acme, id)
	if err != nil || ticket != "" || !expires.IsZero() {
		t.Fatalf("endpoint access = %q, %v, %v; want no ticket or expiry", ticket, expires, err)
	}
	_, _, err = svc.OpenWorkload(context.Background(), testutil.Globex, id)
	wantCode(t, err, domain.CodeNotFound)
}

// Expired tickets are cleared as new ones are made, so the table doesn't grow.
func TestExpiredTicketsAreCleared(t *testing.T) {
	st := testutil.SeededStore(t)
	clock := testutil.NewClock(testutil.T0)
	svc := service.New(st, testutil.Catalog(t), clock.Now)
	ctx := context.Background()
	id := running(t, st, domain.KindNotebook, domain.StateRunning, true)
	old, _, err := svc.OpenWorkload(ctx, testutil.Acme, id)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(service.AccessTicketTTL + time.Second)
	if _, _, err := svc.OpenWorkload(ctx, testutil.Acme, id); err != nil {
		t.Fatal(err)
	}
	_ = st.InTx(ctx, func(q store.Queries) error {
		if _, err := q.UseAccessTicket(ctx, credential.HashToken(old), testutil.T0); !domain.IsNotFound(err) {
			t.Errorf("an expired ticket is still there: %v", err)
		}
		return nil
	})
}

func hasCode(err error, want domain.Code) bool {
	got, ok := domain.CodeOf(err)
	return ok && got == want
}
