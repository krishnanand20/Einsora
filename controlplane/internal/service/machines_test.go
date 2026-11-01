package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/service"
	"github.com/krishnanand20/einsora/controlplane/internal/store"
	"github.com/krishnanand20/einsora/controlplane/internal/testutil"
)

// A tenant sees only its own machines, with only its own workloads counted on them; another tenant's
// machine is not found.
func TestMachinesAreTheTenantsOwn(t *testing.T) {
	st := testutil.SeededStore(t)
	clock := testutil.NewClock(testutil.T0)
	svc := service.New(st, testutil.Catalog(t), clock.Now)
	ctx := context.Background()
	studio, laptop := uuid.New(), uuid.New()
	err := st.InTx(ctx, func(q store.Queries) error {
		acme, err := q.TenantBySlug(ctx, "acme")
		if err != nil {
			return err
		}
		for _, m := range []domain.Machine{
			{ID: studio, TenantID: acme.ID, Name: "studio", Region: domain.RegionLocal, OS: "darwin", Arch: "arm64", CPUs: 12,
				MemoryMiB: 36 * 1024, Sharing: domain.SharingPrivate, LastSeenAt: testutil.T0.Add(-time.Minute), CreatedAt: testutil.T0},
			{ID: laptop, TenantID: acme.ID, Name: "laptop", Region: domain.RegionLocal, OS: "darwin", Arch: "arm64",
				Sharing: domain.SharingPrivate, LastSeenAt: testutil.T0.Add(-time.Hour), CreatedAt: testutil.T0},
		} {
			if err := q.CreateMachine(ctx, m); err != nil {
				return err
			}
		}
		for i, state := range []domain.State{domain.StateRunning, domain.StateProvisioning, domain.StateStopped} {
			if err := q.CreateWorkload(ctx, domain.Workload{ID: uuid.New(), TenantID: acme.ID, Name: "w", Kind: domain.KindNotebook,
				State: state, ImageID: "pytorch-notebook", GPUClass: "l40s-48gb", GPUCount: 1, CreatedBy: "t", MachineID: studio,
				CreatedAt: testutil.T0.Add(time.Duration(i) * time.Second), UpdatedAt: testutil.T0, Region: domain.RegionLocal}); err != nil {
				return err
			}
		}
		// Another tenant's workload on acme's machine (as a guest will be, with sharing): it must
		// never count, or show, in acme's view.
		globex, err := q.TenantBySlug(ctx, "globex")
		if err != nil {
			return err
		}
		return q.CreateWorkload(ctx, domain.Workload{ID: uuid.New(), TenantID: globex.ID, Name: "guest", Kind: domain.KindNotebook,
			State: domain.StateRunning, ImageID: "pytorch-notebook", GPUClass: "l40s-48gb", GPUCount: 1, CreatedBy: "t", MachineID: studio,
			CreatedAt: testutil.T0, UpdatedAt: testutil.T0, Region: domain.RegionLocal})
	})
	if err != nil {
		t.Fatal(err)
	}

	machines, err := svc.ListMachines(ctx, testutil.Acme)
	if err != nil || len(machines) != 2 {
		t.Fatalf("acme's machines = %+v, %v", machines, err)
	}
	byName := map[string]service.MachineInfo{}
	for _, m := range machines {
		byName[m.Name] = m
	}
	if s := byName["studio"]; !s.Online || s.Workloads != 2 {
		t.Errorf("studio = online %v, %d workloads; want online, 2 (not the stopped one, nor globex's)", s.Online, s.Workloads)
	}
	if l := byName["laptop"]; l.Online || l.Workloads != 0 {
		t.Errorf("laptop = online %v, %d workloads; want offline (seen an hour ago), 0", l.Online, l.Workloads)
	}

	if others, err := svc.ListMachines(ctx, testutil.Globex); err != nil || len(others) != 0 {
		t.Errorf("globex sees %+v, %v; want none of acme's", others, err)
	}
	_, err = svc.GetMachine(ctx, testutil.Globex, studio)
	wantCode(t, err, domain.CodeNotFound)
	if m, err := svc.GetMachine(ctx, testutil.Acme, studio); err != nil || m.Name != "studio" {
		t.Errorf("acme's own machine = %+v, %v", m, err)
	}
}
