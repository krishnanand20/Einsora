package api

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/domain"
)

// Logs and transparency (ADR-0015): the full timeline and a complete export of every workload,
// plus a request ID on every response so any log line can be found.

const exportFormat = "einsora.workload-export/v1"

const outputLogsNote = "Workload output (stdout and stderr) isn't captured yet; it will be once the " +
	"runtime log pipeline lands (ADR-0015). Everything else Einsora records about this workload is in this file."

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

type outputLogsView struct {
	Available bool   `json:"available"`
	Note      string `json:"note"`
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
	id, err := pathID(r)
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
	id, err := pathID(r)
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
		OutputLogs:      outputLogsView{Available: false, Note: outputLogsNote},
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
