package service

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/redact"
	"github.com/krishnanand20/einsora/controlplane/internal/store"
)

// Logs and transparency (ADR-0015): users can read and export everything Einsora records about
// their workloads. Nothing is filtered out except secrets, and every redaction is reported.

// Redaction records something removed from an export, so removals are never silent.
type Redaction struct {
	Field  string
	Reason string
}

// WorkloadExport is everything Einsora records about one workload.
type WorkloadExport struct {
	Workload   domain.Workload // Params already redacted
	Events     []domain.Event
	Usage      []domain.UsageRecord
	Redactions []Redaction
	ExportedAt time.Time
}

// WorkloadEvents returns a workload's complete timeline: every state change and note with its
// reason and time, including for deleted workloads.
func (s *Service) WorkloadEvents(ctx context.Context, p domain.Principal, id uuid.UUID) ([]domain.Event, error) {
	var events []domain.Event
	err := s.withTenant(ctx, p, func(q store.Queries, t domain.Tenant) error {
		if _, err := q.GetWorkloadHistory(ctx, t.ID, id); err != nil {
			return err
		}
		var err error
		events, err = q.EventsForWorkload(ctx, t.ID, id)
		return err
	})
	return events, err
}

// ExportWorkload gathers the workload, its full timeline, and its usage records.
func (s *Service) ExportWorkload(ctx context.Context, p domain.Principal, id uuid.UUID) (WorkloadExport, error) {
	var out WorkloadExport
	err := s.withTenant(ctx, p, func(q store.Queries, t domain.Tenant) error {
		w, err := q.GetWorkloadHistory(ctx, t.ID, id)
		if err != nil {
			return err
		}
		if out.Events, err = q.EventsForWorkload(ctx, t.ID, id); err != nil {
			return err
		}
		if out.Usage, err = q.UsageForWorkload(ctx, t.ID, id); err != nil {
			return err
		}
		w.Params, out.Redactions = redactParams(w.Params)
		out.Workload = w
		return nil
	})
	out.ExportedAt = s.now().UTC()
	return out, err
}

// redactParams removes secrets from workload inputs: credentials embedded in a URI. SSH public
// keys are public by design.
func redactParams(params map[string]string) (map[string]string, []Redaction) {
	out := maps.Clone(params)
	var redactions []Redaction
	for _, key := range slices.Sorted(maps.Keys(out)) {
		clean, reasons := redact.URI(out[key])
		out[key] = clean
		for _, reason := range reasons {
			redactions = append(redactions, Redaction{Field: "params." + key, Reason: reason})
		}
	}
	return out, redactions
}

// RedactSecrets removes credentials from every URI inside free text (redact.Secrets). Use it on
// anything that may echo user input before it reaches a log line or a timeline reason.
func RedactSecrets(text string) string { return redact.Secrets(text) }

const maxReasonBytes = 2000

// CleanReason makes text safe to record as a timeline reason, which the workload's owner reads
// in a terminal or browser: control characters become spaces, credentials in URIs are redacted,
// and very long text is cut with a marker saying so (never silently).
func CleanReason(reason string) string {
	reason = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(reason, "\uFFFD"))
	reason = RedactSecrets(reason)
	if len(reason) <= maxReasonBytes {
		return reason
	}
	cut := maxReasonBytes
	for cut > 0 && !utf8.RuneStart(reason[cut]) {
		cut--
	}
	return reason[:cut] + " … [cut at 2000 bytes]"
}
