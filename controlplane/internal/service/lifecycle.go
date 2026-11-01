package service

import (
	"context"
	"time"

	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
	"github.com/krishnanand20/tensorafi/controlplane/internal/store"
)

// Transition is the only way a workload changes state (AGENTS.md rule 7). It validates
// the step, compare-and-swaps the row, and appends the event usage accounting depends on.
// The reason is shown to the workload's owner, so it's cleaned first (CleanReason).
// On success w reflects the new state.
func Transition(ctx context.Context, q store.Queries, w *domain.Workload, to domain.State, reason string, at time.Time) error {
	if !domain.CanTransition(w.State, to) {
		return domain.Errorf(domain.CodeInvalidTransition, "cannot go from %s to %s", w.State, to)
	}
	if err := w.CheckPlaced(to); err != nil {
		return err
	}
	at = at.UTC()
	if err := q.UpdateWorkloadState(ctx, w.ID, w.State, to, at); err != nil {
		return err
	}
	if err := q.AppendEvent(ctx, domain.Event{WorkloadID: w.ID, From: w.State, To: to, Reason: CleanReason(reason), At: at}); err != nil {
		return err
	}
	w.State, w.UpdatedAt = to, at
	return nil
}

// Note adds a timeline entry without changing state (From == To), for decisions that aren't a
// state change, such as "still waiting" or "retrying". Usage accounting ignores notes, because
// only changes into or out of a metered state matter to it.
func Note(ctx context.Context, q store.Queries, w domain.Workload, reason string, at time.Time) error {
	return q.AppendEvent(ctx, domain.Event{WorkloadID: w.ID, From: w.State, To: w.State, Reason: CleanReason(reason), At: at.UTC()})
}
