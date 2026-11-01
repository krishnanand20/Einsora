package service

import (
	"context"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/store"
)

// MachineInfo is one of the caller's tenant's machines, as the store knows it: whether it's online
// comes from when its agent was last seen, never from asking the machine (rule 1).
type MachineInfo struct {
	domain.Machine
	Online bool
	// Workloads counts the tenant's own workloads holding the machine now (provisioning, running,
	// stopping, or being deleted). Another tenant's never show here, even on the tenant's machine.
	Workloads int
}

// ListMachines lists the caller's tenant's machines, by name.
func (s *Service) ListMachines(ctx context.Context, p domain.Principal) ([]MachineInfo, error) {
	var out []MachineInfo
	err := s.withTenant(ctx, p, func(q store.Queries, t domain.Tenant) error {
		machines, err := q.ListMachines(ctx, t.ID)
		if err != nil {
			return err
		}
		counts, err := workloadsByMachine(ctx, q, t.ID)
		if err != nil {
			return err
		}
		now := s.now()
		for _, m := range machines {
			out = append(out, MachineInfo{Machine: m, Online: m.Online(now), Workloads: counts[m.ID]})
		}
		return nil
	})
	return out, err
}

// GetMachine returns one of the caller's tenant's machines. Another tenant's is not found.
func (s *Service) GetMachine(ctx context.Context, p domain.Principal, id uuid.UUID) (MachineInfo, error) {
	var out MachineInfo
	err := s.withTenant(ctx, p, func(q store.Queries, t domain.Tenant) error {
		m, err := q.GetMachine(ctx, t.ID, id)
		if err != nil {
			return err
		}
		counts, err := workloadsByMachine(ctx, q, t.ID)
		if err != nil {
			return err
		}
		out = MachineInfo{Machine: m, Online: m.Online(s.now()), Workloads: counts[m.ID]}
		return nil
	})
	return out, err
}

// workloadsByMachine counts the tenant's workloads holding each machine.
func workloadsByMachine(ctx context.Context, q store.Queries, tenantID uuid.UUID) (map[uuid.UUID]int, error) {
	ws, err := q.ListWorkloads(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	counts := map[uuid.UUID]int{}
	for _, w := range ws {
		if w.MachineID != uuid.Nil && w.State.HoldsCapacity() {
			counts[w.MachineID]++
		}
	}
	return counts, nil
}
