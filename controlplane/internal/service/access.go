package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/tensora/tensora/controlplane/internal/credential"
	"github.com/tensora/tensora/controlplane/internal/domain"
	"github.com/tensora/tensora/controlplane/internal/store"
)

// AccessTicketTTL is how long a ticket to open a workload lasts: long enough for the browser to
// follow it, too short to be worth stealing.
const AccessTicketTTL = time.Minute

// OpenWorkload issues a one-time ticket to open a running notebook through the access proxy
// (ADR-0017). Only the workload's tenant can, as with everything else about it (404 for others).
// The ticket itself is returned once; only its hash is stored. Who opened it goes in its timeline,
// like who started or stopped it: opening a notebook is access to its data.
func (s *Service) OpenWorkload(ctx context.Context, p domain.Principal, id uuid.UUID) (string, time.Time, error) {
	ticket, hash, err := credential.NewToken("tat_")
	if err != nil {
		return "", time.Time{}, err
	}
	now := s.now().UTC()
	expires := now.Add(AccessTicketTTL)
	err = s.withTenant(ctx, p, func(q store.Queries, t domain.Tenant) error {
		w, err := q.GetWorkload(ctx, t.ID, id)
		switch {
		case err != nil:
			return err
		case w.Kind != domain.KindNotebook:
			return domain.Errorf(domain.CodeInvalid, "only notebooks open in the browser so far; endpoints get an address with roadmap step 7b")
		case w.State != domain.StateRunning:
			return domain.Errorf(domain.CodeConflict, "it isn't running")
		case w.MachineID == uuid.Nil:
			return domain.Errorf(domain.CodeInvalid, "opening workloads that run on Kubernetes isn't available yet")
		}
		if err := q.DeleteExpiredAccessTickets(ctx, now); err != nil {
			return err
		}
		if err := q.CreateAccessTicket(ctx, domain.AccessTicket{Hash: hash, TenantID: t.ID, WorkloadID: id, Subject: p.Actor(), ExpiresAt: expires}); err != nil {
			return err
		}
		return Note(ctx, q, w, "opening in the browser requested by "+p.Actor(), now)
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return ticket, expires, nil
}
