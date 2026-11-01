package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/catalog"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/service"
)

// Response shapes. web/src/api/types.ts mirrors these — change both together.
// Views deliberately omit cluster internals (device names, node selectors, object names).

type workloadView struct {
	ID        uuid.UUID           `json:"id"`
	Name      string              `json:"name"`
	Kind      domain.WorkloadKind `json:"kind"`
	State     domain.State        `json:"state"`
	ImageID   string              `json:"image_id"`
	GPUClass  string              `json:"gpu_class"` // the class requested
	GPUCount  int                 `json:"gpu_count"`
	CreatedAt time.Time           `json:"created_at"`
	UpdatedAt time.Time           `json:"updated_at"`
	// Where it runs and on what (ADR-0009, ADR-0010, ADR-0016). placed_class is absent until it's
	// admitted, and differs from gpu_class only after an opt-in fallback. On node agents,
	// machine_id and driver say which machine runs it and how.
	Region      domain.Region `json:"region"`
	PlacedClass string        `json:"placed_class,omitempty"`
	MachineID   string        `json:"machine_id,omitempty"`
	Driver      string        `json:"driver,omitempty"`
}

type listView[T any] struct {
	Items []T `json:"items"`
}

type regionView struct {
	ID          domain.Region `json:"id"`
	DisplayName string        `json:"display_name"`
}

type gpuClassView struct {
	ID          string                 `json:"id"`
	DisplayName string                 `json:"display_name"`
	Accelerator domain.AcceleratorKind `json:"accelerator"`
	VRAMGB      int                    `json:"vram_gb"`
	SupportsVM  bool                   `json:"supports_vm"`
	Regions     []domain.Region        `json:"regions"` // where it's offered
}

// runtimeView says what serves an image on which accelerators, without the internal ref.
type runtimeView struct {
	Accelerators []domain.AcceleratorKind `json:"accelerators"`
	Server       string                   `json:"server,omitempty"`
}

type imageView struct {
	ID          string              `json:"id"`
	DisplayName string              `json:"display_name"`
	Kind        domain.WorkloadKind `json:"kind"`
	Description string              `json:"description"`
	Runtimes    []runtimeView       `json:"runtimes"`
}

type catalogView struct {
	Regions    []regionView   `json:"regions"` // the first is the default
	GPUClasses []gpuClassView `json:"gpu_classes"`
	Images     []imageView    `json:"images"`
}

type errorView struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// RequestID matches the X-Request-ID header and the operator's log lines for this request.
	RequestID string `json:"request_id,omitempty"`
}

func toWorkloadView(w domain.Workload) workloadView {
	if w.State == domain.StatePending || w.State == domain.StateQueued {
		// Waiting to be placed again: a placement from an earlier run isn't where it will run.
		w.PlacedClass, w.MachineID, w.Driver = "", uuid.Nil, ""
	}
	return workloadView{
		ID: w.ID, Name: w.Name, Kind: w.Kind, State: w.State, ImageID: w.ImageID,
		GPUClass: w.GPUClass, GPUCount: w.GPUCount, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt,
		Region: w.Region, PlacedClass: w.PlacedClass, MachineID: idOrEmpty(w.MachineID), Driver: w.Driver,
	}
}

func idOrEmpty(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

func toCatalogView(c catalog.Catalog) catalogView {
	v := catalogView{
		Regions:    make([]regionView, len(c.Regions)),
		GPUClasses: make([]gpuClassView, len(c.GPUClasses)),
		Images:     make([]imageView, len(c.Images)),
	}
	for i, r := range c.Regions {
		v.Regions[i] = regionView{ID: r.ID, DisplayName: r.DisplayName}
	}
	for i, g := range c.GPUClasses {
		regions := g.Regions
		if len(regions) == 0 {
			for _, r := range c.Regions {
				regions = append(regions, r.ID)
			}
		}
		v.GPUClasses[i] = gpuClassView{ID: g.ID, DisplayName: g.DisplayName, Accelerator: g.Accelerator, VRAMGB: g.VRAMGB,
			SupportsVM: g.VMDeviceName != "", Regions: regions}
	}
	for i, img := range c.Images {
		runtimes := make([]runtimeView, len(img.Runtimes))
		for j, r := range img.Runtimes {
			runtimes[j] = runtimeView{Accelerators: r.Accelerators, Server: r.Server}
		}
		v.Images[i] = imageView{ID: img.ID, DisplayName: img.DisplayName, Kind: img.Kind, Description: img.Description, Runtimes: runtimes}
	}
	return v
}

var statusByCode = map[domain.Code]int{
	domain.CodeInvalid:           http.StatusBadRequest,
	domain.CodeUnauthenticated:   http.StatusUnauthorized,
	domain.CodeForbidden:         http.StatusForbidden,
	domain.CodeNotFound:          http.StatusNotFound,
	domain.CodeConflict:          http.StatusConflict,
	domain.CodeQuotaExceeded:     http.StatusConflict,
	domain.CodeInvalidTransition: http.StatusConflict,
	domain.CodeTooManyRequests:   http.StatusTooManyRequests,
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var de *domain.Error
	if errors.As(err, &de) {
		status, ok := statusByCode[de.Code]
		if !ok {
			status = http.StatusBadRequest
		}
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "60")
		}
		writeJSON(w, status, errorView{errorDetail{Code: string(de.Code), Message: de.Message, RequestID: requestID(r.Context())}})
		return
	}
	// The full error goes to the operator's log; the user gets the request ID to look it up.
	info := requestInfoFrom(r.Context())
	id := info.id
	s.log.ErrorContext(r.Context(), "request failed",
		append(info.logAttrs(), "method", r.Method, "path", r.URL.Path, "err", service.RedactSecrets(err.Error()))...)
	writeJSON(w, http.StatusInternalServerError, errorView{errorDetail{
		Code: "internal", Message: "internal error; give the operator this request ID to find the full log", RequestID: id,
	}})
}

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return domain.Errorf(domain.CodeInvalid, "invalid JSON body: %v", err)
	}
	if dec.More() {
		return domain.Errorf(domain.CodeInvalid, "body must be a single JSON object")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
