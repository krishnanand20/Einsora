package api

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/service"
)

// machineView is one of the caller's own machines (ADR-0013): its name is the label its agent
// registered, shown only to its owner. It never carries addresses, paths, or another tenant's work.
type machineView struct {
	ID           uuid.UUID            `json:"id"`
	Name         string               `json:"name"`
	Region       domain.Region        `json:"region"`
	OS           string               `json:"os"`
	Arch         string               `json:"arch"`
	CPUs         int                  `json:"cpus"`
	MemoryMiB    int64                `json:"memory_mib"`
	Accelerators []domain.Accelerator `json:"accelerators"`
	Sharing      domain.SharingLevel  `json:"sharing"`
	// Status is online or offline, from when its agent was last seen (seen within the last
	// domain.MachineSeenWithin); last_seen_at is absent until it's been seen at all.
	Status     string     `json:"status"`
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
	Workloads  int        `json:"workloads"` // your workloads holding it now: provisioning, running, stopping, or being deleted
	CreatedAt  time.Time  `json:"created_at"`
}

func toMachineView(m service.MachineInfo) machineView {
	v := machineView{
		ID: m.ID, Name: m.Name, Region: m.Region, OS: m.OS, Arch: m.Arch, CPUs: m.CPUs, MemoryMiB: m.MemoryMiB,
		Accelerators: m.Accelerators, Sharing: m.Sharing, Status: "offline", Workloads: m.Workloads, CreatedAt: m.CreatedAt,
	}
	if v.Accelerators == nil {
		v.Accelerators = []domain.Accelerator{}
	}
	if m.Online {
		v.Status = "online"
	}
	if !m.LastSeenAt.IsZero() {
		seen := m.LastSeenAt
		v.LastSeenAt = &seen
	}
	return v
}

func (s *Server) listMachines(w http.ResponseWriter, r *http.Request, p domain.Principal) error {
	machines, err := s.svc.ListMachines(r.Context(), p)
	if err != nil {
		return err
	}
	out := listView[machineView]{Items: make([]machineView, 0, len(machines))}
	for _, m := range machines {
		out.Items = append(out.Items, toMachineView(m))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) getMachine(w http.ResponseWriter, r *http.Request, p domain.Principal) error {
	id, err := pathID(r, "machine")
	if err != nil {
		return err
	}
	m, err := s.svc.GetMachine(r.Context(), p, id)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, toMachineView(m))
	return nil
}
