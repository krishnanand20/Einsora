package service

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/output"
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
	Output     WorkloadOutput // all of it that's kept
	Redactions []Redaction
	ExportedAt time.Time
}

// WorkloadOutput is a workload's output, or a page of it (ADR-0024), with what its reader must be
// told: whether it's collected at all, and how much was lost, trimmed, or expired.
type WorkloadOutput struct {
	Chunks    []domain.OutputChunk
	States    []domain.OutputState
	Collected bool
	Notes     []string
}

// MaxOutputPage is the most chunks one read of a workload's output returns.
const MaxOutputPage = 1000

// WorkloadOutput returns up to limit chunks of a workload's output after the chunk Seq after,
// including for deleted workloads, whose output is kept for the retention period too.
func (s *Service) WorkloadOutput(ctx context.Context, p domain.Principal, id uuid.UUID, after int64, limit int) (WorkloadOutput, error) {
	var out WorkloadOutput
	err := s.withTenant(ctx, p, func(q store.Queries, t domain.Tenant) error {
		w, err := q.GetWorkloadHistory(ctx, t.ID, id)
		if err != nil {
			return err
		}
		if out.Chunks, err = q.OutputForWorkload(ctx, t.ID, id, after, min(max(limit, 1), MaxOutputPage)); err != nil {
			return err
		}
		if out.States, err = q.OutputStatesForWorkload(ctx, t.ID, id); err != nil {
			return err
		}
		out.Collected, out.Notes = outputNotes(w, out.States)
		return nil
	})
	return out, err
}

// outputNotes says whether w's output is collected, and what its reader must know: why it isn't,
// or how much of it was lost or removed (ADR-0015: never silently).
func outputNotes(w domain.Workload, states []domain.OutputState) (bool, []string) {
	var notes []string
	collected := true
	spec, known := domain.LookupDriver(w.Driver)
	switch {
	case w.Kind == domain.KindVM:
		collected = false
		notes = append(notes, "Output from inside a VM is never collected: you have root, so read its logs over SSH. "+
			"Its timeline still shows every step Einsora took.")
	case w.Attempt == 0:
		notes = append(notes, "It hasn't started yet, so there's no output.")
	case w.Driver == "":
		collected = false
		notes = append(notes, "This installation's runtime doesn't send workload output to Einsora yet. Its timeline shows every step Einsora took.")
	case !known || !spec.CapturesOutput:
		collected = false
		notes = append(notes, fmt.Sprintf("The %s driver doesn't send workload output to Einsora yet. Its timeline shows every step Einsora took.", w.Driver))
	}
	var lost, trimmed, expired int64
	for _, st := range states {
		lost, trimmed, expired = lost+st.LostBytes, trimmed+st.TrimmedBytes, expired+st.ExpiredBytes
	}
	if lost > 0 {
		notes = append(notes, output.Size(lost)+" of output was lost before Einsora received it; notes in the output say where.")
	}
	if trimmed > 0 {
		notes = append(notes, output.Size(trimmed)+" of the oldest output was removed to keep this workload within its output limit (EINSORA_OUTPUT_MAX_MIB).")
	}
	if expired > 0 {
		notes = append(notes, output.Size(expired)+" of output was removed after the retention period (EINSORA_OUTPUT_RETENTION).")
	}
	return collected, notes
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
		var after int64
		for {
			page, err := q.OutputForWorkload(ctx, t.ID, id, after, MaxOutputPage)
			if err != nil {
				return err
			}
			out.Output.Chunks = append(out.Output.Chunks, page...)
			if len(page) < MaxOutputPage {
				break
			}
			after = page[len(page)-1].Seq
		}
		if out.Output.States, err = q.OutputStatesForWorkload(ctx, t.ID, id); err != nil {
			return err
		}
		out.Output.Collected, out.Output.Notes = outputNotes(w, out.Output.States)
		w.Params, out.Redactions = redactParams(w.Params)
		redacted := 0
		for _, c := range out.Output.Chunks {
			redacted += c.Redactions
		}
		if redacted > 0 {
			out.Redactions = append(out.Redactions, Redaction{Field: "output_logs",
				Reason: fmt.Sprintf("%d value(s) in the workload's output that may be credentials (in URIs) were replaced with REDACTED when it was received", redacted)})
		}
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
