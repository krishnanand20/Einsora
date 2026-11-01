package access

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/tensora/tensora/controlplane/internal/domain"
	"github.com/tensora/tensora/controlplane/internal/redact"
	"github.com/tensora/tensora/controlplane/internal/store"
)

// TokenResolver is implemented by the service. Endpoint requests accept API tokens only, never
// browser sessions, notebook tickets, or credentials in a URL. Tokens are rechecked every request.
type TokenResolver interface {
	PrincipalForToken(context.Context, string) (domain.Principal, error)
}

func (s *Server) WithAPITokens(tokens TokenResolver) *Server {
	s.tokens = tokens
	return s
}

func endpointError(w http.ResponseWriter, status int, code, message, requestID string) {
	w.Header().Set("X-Request-ID", requestID)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
		"code": code, "message": message, "request_id": requestID,
	}})
}

func (s *Server) endpoint(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	requestID := uuid.NewString()
	if s.tokens == nil || len(r.Header.Values("Authorization")) != 1 || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		endpointError(w, http.StatusUnauthorized, "unauthenticated", "use an API token", requestID)
		return
	}
	p, err := s.tokens.PrincipalForToken(r.Context(), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if err != nil || p.Via != domain.AuthToken {
		endpointError(w, http.StatusUnauthorized, "unauthenticated", "invalid or expired API token", requestID)
		return
	}
	if fromElsewhere(r) {
		endpointError(w, http.StatusForbidden, "forbidden", "this request came from another site", requestID)
		return
	}
	var workload domain.Workload
	err = s.store.InTx(r.Context(), func(q store.Queries) error {
		tenant, err := q.TenantBySlug(r.Context(), p.TenantSlug)
		if err != nil {
			return err
		}
		workload, err = q.GetWorkload(r.Context(), tenant.ID, id)
		return err
	})
	switch {
	case domain.IsNotFound(err), err == nil && (workload.Kind != domain.KindEndpoint || workload.MachineID == uuid.Nil):
		endpointError(w, http.StatusNotFound, "not_found", "endpoint not found", requestID)
		return
	case err != nil:
		s.log.Error("couldn't read an endpoint", "request_id", requestID, "tenant", p.TenantSlug, "subject", p.Subject, "err", redact.Secrets(err.Error()))
		endpointError(w, http.StatusInternalServerError, "internal", "couldn't read endpoint", requestID)
		return
	case workload.State != domain.StateRunning:
		endpointError(w, http.StatusServiceUnavailable, "unavailable", "endpoint isn't running", requestID)
		return
	}
	if (r.Method != http.MethodGet || r.URL.Path != "/v1/models") &&
		(r.Method != http.MethodPost || (r.URL.Path != "/v1/completions" && r.URL.Path != "/v1/chat/completions")) {
		endpointError(w, http.StatusNotFound, "not_found", "endpoint route not found", requestID)
		return
	}
	socket, token, ok := s.targets.Target(id)
	if !ok {
		endpointError(w, http.StatusServiceUnavailable, "unavailable", "endpoint isn't ready on this machine", requestID)
		return
	}
	r = r.Clone(r.Context())
	r.Header.Del("Cookie")
	r.Header.Set("X-Request-ID", requestID)
	proxy := s.proxy(socket, token)
	rewrite := proxy.Rewrite
	proxy.Rewrite = func(pr *httputil.ProxyRequest) {
		rewrite(pr)
		pr.Out.Header.Set("Authorization", "Bearer "+token)
	}
	modify := proxy.ModifyResponse
	proxy.ModifyResponse = func(resp *http.Response) error {
		if err := modify(resp); err != nil {
			return err
		}
		resp.Header.Set("X-Request-ID", requestID)
		resp.Header.Set("X-Tensora-Served-By", "replica")
		resp.Header.Set("X-Tensora-Region", string(workload.Region))
		resp.Header.Set("X-Tensora-Compute-Class", workload.PlacedClass)
		resp.Header.Set("X-Tensora-Fallback", strconv.FormatBool(workload.PlacedClass != workload.GPUClass))
		resp.Header.Del("Set-Cookie")
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		s.log.Warn("couldn't reach an endpoint", "request_id", requestID, "tenant", p.TenantSlug, "subject", p.Subject,
			"workload", id, "err", redact.Secrets(err.Error()))
		endpointError(w, http.StatusBadGateway, "unavailable", "endpoint isn't answering", requestID)
	}
	s.log.Info("endpoint request", "request_id", requestID, "tenant", p.TenantSlug, "subject", p.Actor(), "workload", id)
	proxy.ServeHTTP(w, r)
}
