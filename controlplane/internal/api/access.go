package api

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/tensora/tensora/controlplane/internal/domain"
)

type accessView struct {
	// URL opens the workload in a browser, once, within a minute (ADR-0017).
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// openWorkload gives the caller a one-time URL to open a running notebook through the access
// proxy, at the workload's own host, w-<id>.localhost, a different site from the dashboard.
func (s *Server) openWorkload(w http.ResponseWriter, r *http.Request, p domain.Principal) error {
	id, err := pathID(r)
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
	u := url.URL{Scheme: "http", Host: fmt.Sprintf("w-%s.localhost:%d", id, s.accessPort), Path: "/_tensora/open",
		RawQuery: url.Values{"ticket": {ticket}}.Encode()}
	writeJSON(w, http.StatusCreated, accessView{URL: u.String(), ExpiresAt: expires})
	return nil
}
