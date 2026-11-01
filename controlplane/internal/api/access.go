package api

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/krishnanand20/einsora/controlplane/internal/domain"
)

type accessView struct {
	// URL is a notebook's one-time browser link or an endpoint's API-token-protected base URL.
	URL       string     `json:"url"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// openWorkload gives the caller a running notebook's one-time link or an endpoint's base URL,
// through the access proxy at the workload's own host, separate from the dashboard.
func (s *Server) openWorkload(w http.ResponseWriter, r *http.Request, p domain.Principal) error {
	id, err := pathID(r, "workload")
	if err != nil {
		return err
	}
	if s.accessPort == 0 {
		return domain.Errorf(domain.CodeInvalid, "opening workloads needs the access proxy, which runs with the agent backend")
	}
	ticket, expires, err := s.svc.OpenWorkload(r.Context(), p, id)
	if err != nil {
		return err
	}
	u := url.URL{Scheme: "http", Host: fmt.Sprintf("w-%s.localhost:%d", id, s.accessPort), Path: "/_einsora/open",
		RawQuery: url.Values{"ticket": {ticket}}.Encode()}
	grant := accessView{}
	if ticket == "" {
		u.Path, u.RawQuery = "/v1", ""
	} else {
		grant.ExpiresAt = &expires
	}
	grant.URL = u.String()
	writeJSON(w, http.StatusCreated, grant)
	return nil
}
