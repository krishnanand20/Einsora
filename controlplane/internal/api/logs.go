package api

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/service"
)

// Logs and transparency (ADR-0015): the full timeline and a complete export of every workload,
// plus a request ID on every response so any log line can be found.

const exportFormat = "einsora.workload-export/v1"

type eventView struct {
	ID     int64        `json:"id"`
	From   domain.State `json:"from,omitempty"`
	To     domain.State `json:"to"`
	Reason string       `json:"reason"`
	At     time.Time    `json:"at"`
}

type usageView struct {
	PeriodStart time.Time     `json:"period_start"`
	GPUClass    string        `json:"gpu_class"` // the class that actually ran
	GPUCount    int           `json:"gpu_count"`
	Seconds     int64         `json:"seconds"`
	GPUSeconds  int64         `json:"gpu_seconds"`
	Region      domain.Region `json:"region"`
	MachineID   string        `json:"machine_id,omitempty"` // the agent machine it ran on
}

type redactionView struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

type outputChunkView struct {
	ID         int64               `json:"id"`     // counts this workload's output from 1: read on with after=id
	Stream     domain.OutputStream `json:"stream"` // stdout, stderr, or einsora (a note from Einsora)
	At         time.Time           `json:"at"`     // when Einsora received it
	Text       string              `json:"text"`
	Redactions int                 `json:"redactions,omitempty"` // credentials replaced with REDACTED
}

type outputStreamView struct {
	Stream       domain.OutputStream `json:"stream"`
	StoredBytes  int64               `json:"stored_bytes"`
	LostBytes    int64               `json:"lost_bytes"`
	TrimmedBytes int64               `json:"trimmed_bytes"`
	ExpiredBytes int64               `json:"expired_bytes"`
}

// outputPageView is a page of a workload's output (ADR-0024). Next is where to read on from
// (after=next); notes say what isn't collected, or was lost or removed.
type outputPageView struct {
	Items     []outputChunkView `json:"items"`
	Next      int64             `json:"next"`
	Collected bool              `json:"collected"`
	Notes     []string          `json:"notes"`
}

// outputLogsView is all of a workload's kept output, in its export.
type outputLogsView struct {
	Available bool               `json:"available"` // its output is collected
	Notes     []string           `json:"notes"`
	Streams   []outputStreamView `json:"streams"`
	Items     []outputChunkView  `json:"items"`
}

func toOutputChunkViews(chunks []domain.OutputChunk) []outputChunkView {
	out := make([]outputChunkView, len(chunks))
	for i, c := range chunks {
		out[i] = outputChunkView{ID: c.Seq, Stream: c.Stream, At: c.At, Text: c.Text, Redactions: c.Redactions}
	}
	return out
}

func notesOrEmpty(notes []string) []string {
	if notes == nil {
		return []string{}
	}
	return notes
}

const (
	// followFor is how long a follow=true read waits for new output before answering with none.
	followFor = 20 * time.Second
	// followEvery matches how often agents send output, so a waiting read costs the store little.
	followEvery = time.Second
	outputPage  = 500
)

// workloadOutput returns a page of a workload's output after the chunk ID after. With follow=true
// and nothing new, it waits up to followFor for more, reading the store (rule 1: never the
// runtime). Another tenant's workload is 404.
func (s *Server) workloadOutput(w http.ResponseWriter, r *http.Request, p domain.Principal) error {
	id, err := pathID(r, "workload")
	if err != nil {
		return err
	}
	q := r.URL.Query()
	after, limit, follow := int64(0), outputPage, false
	if v := q.Get("after"); v != "" {
		if after, err = strconv.ParseInt(v, 10, 64); err != nil || after < 0 {
			return domain.Errorf(domain.CodeInvalid, "after must be a chunk ID: a whole number, 0 or more")
		}
	}
	if v := q.Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > service.MaxOutputPage {
			return domain.Errorf(domain.CodeInvalid, "limit must be between 1 and %d", service.MaxOutputPage)
		}
	}
	if v := q.Get("follow"); v != "" {
		if follow, err = strconv.ParseBool(v); err != nil {
			return domain.Errorf(domain.CodeInvalid, "follow must be true or false")
		}
	}
	deadline := time.Now().Add(followFor)
	for {
		out, err := s.svc.WorkloadOutput(r.Context(), p, id, after, limit)
		if err != nil {
			return err
		}
		if len(out.Chunks) > 0 || !follow || !out.Collected || !time.Now().Before(deadline) {
			next := after
			if len(out.Chunks) > 0 {
				next = out.Chunks[len(out.Chunks)-1].Seq
			}
			writeJSON(w, http.StatusOK, outputPageView{Items: toOutputChunkViews(out.Chunks), Next: next,
				Collected: out.Collected, Notes: notesOrEmpty(out.Notes)})
			return nil
		}
		select {
		case <-r.Context().Done():
			return nil // the client went away
		case <-time.After(followEvery):
		}
	}
}

// toOutputLogsView is a workload's kept output, for its export.
func toOutputLogsView(out service.WorkloadOutput) outputLogsView {
	v := outputLogsView{Available: out.Collected, Notes: notesOrEmpty(out.Notes), Streams: make([]outputStreamView, len(out.States)),
		Items: toOutputChunkViews(out.Chunks)}
	for i, st := range out.States {
		v.Streams[i] = outputStreamView{Stream: st.Stream, StoredBytes: st.StoredBytes, LostBytes: st.LostBytes,
			TrimmedBytes: st.TrimmedBytes, ExpiredBytes: st.ExpiredBytes}
	}
	return v
}

type exportView struct {
	Format     string       `json:"format"`
	ExportedAt time.Time    `json:"exported_at"`
	ExportedBy string       `json:"exported_by"`
	Workload   workloadView `json:"workload"`
	// LastPlacedClass, LastMachineID, and LastDriver are the stored placement, kept for a workload
	// that's waiting again (whose view hides it), so the export stays complete.
	LastPlacedClass string            `json:"last_placed_class,omitempty"`
	LastMachineID   string            `json:"last_machine_id,omitempty"`
	LastDriver      string            `json:"last_driver,omitempty"`
	CreatedBy       string            `json:"created_by"`
	Params          map[string]string `json:"params"`
	Events          []eventView       `json:"events"`
	Usage           []usageView       `json:"usage"`
	OutputLogs      outputLogsView    `json:"output_logs"`
	Redactions      []redactionView   `json:"redactions"`
}

func toEventViews(events []domain.Event) []eventView {
	out := make([]eventView, len(events))
	for i, e := range events {
		out[i] = eventView{ID: e.ID, From: e.From, To: e.To, Reason: e.Reason, At: e.At}
	}
	return out
}

func (s *Server) workloadEvents(w http.ResponseWriter, r *http.Request, p domain.Principal) error {
	id, err := pathID(r, "workload")
	if err != nil {
		return err
	}
	events, err := s.svc.WorkloadEvents(r.Context(), p, id)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, listView[eventView]{Items: toEventViews(events)})
	return nil
}

// exportWorkload returns everything about one workload as a downloadable JSON file.
func (s *Server) exportWorkload(w http.ResponseWriter, r *http.Request, p domain.Principal) error {
	id, err := pathID(r, "workload")
	if err != nil {
		return err
	}
	exp, err := s.svc.ExportWorkload(r.Context(), p, id)
	if err != nil {
		return err
	}
	view := exportView{
		Format:          exportFormat,
		ExportedAt:      exp.ExportedAt,
		ExportedBy:      p.Actor(),
		Workload:        toWorkloadView(exp.Workload),
		LastPlacedClass: exp.Workload.PlacedClass,
		LastMachineID:   idOrEmpty(exp.Workload.MachineID),
		LastDriver:      exp.Workload.Driver,
		CreatedBy:       exp.Workload.CreatedBy,
		Params:          exp.Workload.Params,
		Events:          toEventViews(exp.Events),
		Usage:           make([]usageView, len(exp.Usage)),
		OutputLogs:      toOutputLogsView(exp.Output),
		Redactions:      make([]redactionView, len(exp.Redactions)),
	}
	for i, u := range exp.Usage {
		view.Usage[i] = usageView{PeriodStart: u.PeriodStart, GPUClass: u.GPUClass, GPUCount: u.GPUCount,
			Seconds: u.Seconds, GPUSeconds: u.GPUSeconds(), Region: u.Region, MachineID: idOrEmpty(u.MachineID)}
	}
	for i, rd := range exp.Redactions {
		view.Redactions[i] = redactionView{Field: rd.Field, Reason: rd.Reason}
	}
	filename := fmt.Sprintf("einsora-%s-%s.json", exp.Workload.Name, exp.Workload.ID)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	writeJSON(w, http.StatusOK, view)
	return nil
}

type ctxKey int

const requestInfoKey ctxKey = iota

// requestInfo is what every log line for a request says about it. authed fills in who made it.
type requestInfo struct {
	id       string // generated here, never taken from the caller
	clientID string // the caller's own X-Request-ID, if well formed; logged for correlation only
	tenant   string
	subject  string
}

// validClientRequestID bounds caller-supplied IDs so they're safe to log.
var validClientRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// withRequestID gives every request a fresh ID, returns it in the X-Request-ID response header,
// and puts it in every log line and error body. A caller's X-Request-ID is never reused as the ID,
// so an ID always points at exactly one request; it's logged as client_request_id instead.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := &requestInfo{id: uuid.NewString()}
		if in := r.Header.Get("X-Request-ID"); validClientRequestID.MatchString(in) {
			info.clientID = in
		}
		w.Header().Set("X-Request-ID", info.id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestInfoKey, info)))
	})
}

func requestInfoFrom(ctx context.Context) *requestInfo {
	if info, ok := ctx.Value(requestInfoKey).(*requestInfo); ok {
		return info
	}
	return &requestInfo{}
}

func requestID(ctx context.Context) string { return requestInfoFrom(ctx).id }

// logAttrs identifies the request, and who made it once authenticated, so an operator can check
// that whoever hands them a request ID owns that request.
func (info *requestInfo) logAttrs() []any {
	attrs := []any{"request_id", info.id}
	if info.clientID != "" {
		attrs = append(attrs, "client_request_id", info.clientID)
	}
	if info.tenant != "" {
		attrs = append(attrs, "tenant", info.tenant, "subject", info.subject)
	}
	return attrs
}
