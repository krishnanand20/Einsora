// Package service holds the business operations behind the API. Every method is
// tenant-scoped through the caller's Principal; nothing here talks to Kubernetes.
package service

import (
	"context"
	"runtime"
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/catalog"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/store"
)

type Service struct {
	store   store.Store
	catalog *catalog.Catalog
	now     func() time.Time
	logins  *loginLimiter
	dummy   dummyHash
	// hashSlots bounds concurrent password hashing: each argon2id call takes 64 MiB, so unbounded
	// parallel sign-ins could exhaust memory (on a Mac, the memory the owner's GPU work shares).
	hashSlots chan struct{}
}

func New(st store.Store, cat *catalog.Catalog, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: st, catalog: cat, now: now, logins: newLoginLimiter(),
		hashSlots: make(chan struct{}, min(4, max(1, runtime.NumCPU()/2)))}
}

// withTenant resolves the caller's tenant and runs fn in the same transaction.
func (s *Service) withTenant(ctx context.Context, p domain.Principal, fn func(store.Queries, domain.Tenant) error) error {
	return s.store.InTx(ctx, func(q store.Queries) error {
		t, err := q.TenantBySlug(ctx, p.TenantSlug)
		if domain.IsNotFound(err) {
			return domain.Errorf(domain.CodeForbidden, "account is not a member of tenant %q", p.TenantSlug)
		}
		if err != nil {
			return err
		}
		return fn(q, t)
	})
}

// VisibleCatalog returns the regions, and the classes and images the caller's tenant may launch.
func (s *Service) VisibleCatalog(ctx context.Context, p domain.Principal) (catalog.Catalog, error) {
	out := catalog.Catalog{Regions: s.catalog.Regions, Images: s.catalog.Images}
	err := s.withTenant(ctx, p, func(_ store.Queries, t domain.Tenant) error {
		for _, g := range s.catalog.GPUClasses {
			if g.VisibleTo(t.Tier) {
				out.GPUClasses = append(out.GPUClasses, g)
			}
		}
		return nil
	})
	return out, err
}

func (s *Service) CreateWorkload(ctx context.Context, p domain.Principal, in CreateWorkloadInput) (domain.Workload, error) {
	if err := in.validate(); err != nil {
		return domain.Workload{}, err
	}
	var w domain.Workload
	err := s.withTenant(ctx, p, func(q store.Queries, t domain.Tenant) error {
		region, err := s.checkCatalog(t, in)
		if err != nil {
			return err
		}
		if err := checkQuota(ctx, q, t.ID, in.GPUClass, in.GPUCount); err != nil {
			return err
		}
		now := s.now().UTC()
		w = domain.Workload{
			ID: uuid.Must(uuid.NewV7()), TenantID: t.ID, Name: in.Name, Kind: in.Kind,
			State: domain.StatePending, ImageID: in.ImageID, GPUClass: in.GPUClass,
			GPUCount: in.GPUCount, Params: in.params(), CreatedBy: p.Subject,
			CreatedAt: now, UpdatedAt: now, Region: region,
		}
		if err := q.CreateWorkload(ctx, w); err != nil {
			return err
		}
		return q.AppendEvent(ctx, domain.Event{WorkloadID: w.ID, To: domain.StatePending, Reason: CleanReason("created by " + p.Actor()), At: now})
	})
	return w, err
}

// ListWorkloads returns the caller's workloads. includeDeleted adds deleted ones, whose timeline
// and export stay available (ADR-0015).
func (s *Service) ListWorkloads(ctx context.Context, p domain.Principal, includeDeleted bool) ([]domain.Workload, error) {
	var out []domain.Workload
	err := s.withTenant(ctx, p, func(q store.Queries, t domain.Tenant) error {
		var err error
		if includeDeleted {
			out, err = q.ListWorkloadHistory(ctx, t.ID)
		} else {
			out, err = q.ListWorkloads(ctx, t.ID)
		}
		return err
	})
	return out, err
}

func (s *Service) GetWorkload(ctx context.Context, p domain.Principal, id uuid.UUID) (domain.Workload, error) {
	return s.withWorkload(ctx, p, id, func(store.Queries, domain.Tenant, *domain.Workload) error { return nil })
}

// GetWorkloadHistory is GetWorkload including deleted workloads, for reading their history.
func (s *Service) GetWorkloadHistory(ctx context.Context, p domain.Principal, id uuid.UUID) (domain.Workload, error) {
	var w domain.Workload
	err := s.withTenant(ctx, p, func(q store.Queries, t domain.Tenant) error {
		var err error
		w, err = q.GetWorkloadHistory(ctx, t.ID, id)
		return err
	})
	return w, err
}

// StopWorkload releases the GPU. VMs and notebooks keep their disks; endpoints are torn down.
func (s *Service) StopWorkload(ctx context.Context, p domain.Principal, id uuid.UUID) (domain.Workload, error) {
	return s.withWorkload(ctx, p, id, func(q store.Queries, _ domain.Tenant, w *domain.Workload) error {
		if w.State != domain.StateProvisioning && w.State != domain.StateRunning {
			return domain.Errorf(domain.CodeInvalidTransition, "only provisioning or running workloads can be stopped (this one is %s)", w.State)
		}
		return Transition(ctx, q, w, domain.StateStopping, "stop requested by "+p.Actor(), s.now())
	})
}

// StartWorkload re-queues a stopped or failed workload; it must fit the quota again.
func (s *Service) StartWorkload(ctx context.Context, p domain.Principal, id uuid.UUID) (domain.Workload, error) {
	return s.withWorkload(ctx, p, id, func(q store.Queries, t domain.Tenant, w *domain.Workload) error {
		if w.State != domain.StateStopped && w.State != domain.StateFailed {
			return domain.Errorf(domain.CodeInvalidTransition, "only stopped or failed workloads can be started (this one is %s)", w.State)
		}
		// The catalog or the tenant's tier may have changed since it was created.
		img, okImage := s.catalog.Image(w.ImageID)
		gpu, okClass := s.catalog.GPUClass(w.GPUClass)
		if !okImage || !okClass {
			return domain.Errorf(domain.CodeInvalid, "its image or gpu_class is no longer offered by this installation")
		}
		if _, err := catalog.CheckLaunch(t.Tier, gpu, img, w.Kind, w.Region); err != nil {
			return err
		}
		if err := checkQuota(ctx, q, t.ID, w.GPUClass, w.GPUCount); err != nil {
			return err
		}
		return Transition(ctx, q, w, domain.StatePending, "start requested by "+p.Actor(), s.now())
	})
}

// DeleteWorkload is idempotent: deleting a workload that is already deleting succeeds.
func (s *Service) DeleteWorkload(ctx context.Context, p domain.Principal, id uuid.UUID) (domain.Workload, error) {
	return s.withWorkload(ctx, p, id, func(q store.Queries, _ domain.Tenant, w *domain.Workload) error {
		if w.State == domain.StateDeleting {
			return nil
		}
		return Transition(ctx, q, w, domain.StateDeleting, "delete requested by "+p.Actor(), s.now())
	})
}

// withWorkload loads the caller's workload id and runs fn on it in one transaction.
func (s *Service) withWorkload(ctx context.Context, p domain.Principal, id uuid.UUID,
	fn func(store.Queries, domain.Tenant, *domain.Workload) error,
) (domain.Workload, error) {
	var w domain.Workload
	err := s.withTenant(ctx, p, func(q store.Queries, t domain.Tenant) error {
		var err error
		if w, err = q.GetWorkload(ctx, t.ID, id); err != nil {
			return err
		}
		return fn(q, t, &w)
	})
	return w, err
}

// checkCatalog checks a new workload against the catalog and returns its region: the one
// requested, or the catalog's default (ADR-0009).
func (s *Service) checkCatalog(t domain.Tenant, in CreateWorkloadInput) (domain.Region, error) {
	region := in.Region
	if region == "" {
		region = s.catalog.DefaultRegion()
	}
	if _, ok := s.catalog.Region(region); !ok {
		return "", domain.Errorf(domain.CodeInvalid, "unknown region %q", region)
	}
	img, ok := s.catalog.Image(in.ImageID)
	if !ok || img.Kind != in.Kind {
		return "", domain.Errorf(domain.CodeInvalid, "unknown %s image %q", in.Kind, in.ImageID)
	}
	gpu, ok := s.catalog.GPUClass(in.GPUClass)
	if !ok || !gpu.VisibleTo(t.Tier) {
		return "", domain.Errorf(domain.CodeInvalid, "unknown gpu_class %q", in.GPUClass) // don't confirm hidden classes exist
	}
	if _, err := catalog.CheckLaunch(t.Tier, gpu, img, in.Kind, region); err != nil {
		return "", err
	}
	return region, nil
}

func checkQuota(ctx context.Context, q store.Queries, tenantID uuid.UUID, gpuClass string, want int) error {
	limit, err := q.LockQuota(ctx, tenantID, gpuClass)
	if err != nil {
		return err
	}
	used, err := q.GPUsHeld(ctx, tenantID, gpuClass)
	if err != nil {
		return err
	}
	if used+want > limit {
		return domain.Errorf(domain.CodeQuotaExceeded,
			"%s quota: %d of %d GPUs in use, %d requested", gpuClass, used, limit, want)
	}
	return nil
}
