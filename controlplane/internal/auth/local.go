package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
)

const (
	// SessionCookie holds a signed-in browser's session token. It's HttpOnly and SameSite=Strict.
	SessionCookie = "tensorafi_session"
	// SecureSessionCookie is its name over HTTPS (see Transport.SessionCookieName).
	SecureSessionCookie = "__Host-tensorafi_session"
	// CSRFHeader must accompany every browser request that changes something. A page on another
	// site can't add a custom header to a request here without a CORS preflight, which this API
	// never allows, so the header proves the request came from the dashboard itself.
	CSRFHeader = "X-TensoraFi-CSRF"
)

// PrincipalResolver turns a credential into an identity; the service implements it.
type PrincipalResolver interface {
	PrincipalForToken(ctx context.Context, token string) (domain.Principal, error)
	PrincipalForSession(ctx context.Context, session string) (domain.Principal, error)
}

// Local authenticates local accounts (ADR-0012): API tokens for the CLI and scripts, and session
// cookies for the dashboard.
type Local struct {
	resolver  PrincipalResolver
	Transport Transport
}

func NewLocal(r PrincipalResolver, t Transport) *Local { return &Local{resolver: r, Transport: t} }

// SessionValue returns the request's session cookie, refusing a connection that isn't private
// enough for it. Over HTTPS only the __Host- cookie counts, so a cookie with the other name,
// planted by a plain-HTTP response, another port, or a sibling subdomain, is ignored. A request
// carrying the counted cookie more than once (one may have been planted) is refused.
func (l *Local) SessionValue(r *http.Request) (string, error) {
	name := l.Transport.SessionCookieName(r)
	var values []string
	for _, c := range r.Cookies() {
		if c.Name == name && c.Value != "" {
			values = append(values, c.Value)
		}
	}
	switch {
	case len(values) == 0:
		return "", domain.Errorf(domain.CodeUnauthenticated, "sign in, or send an API token")
	case len(values) > 1:
		return "", domain.Errorf(domain.CodeUnauthenticated, "conflicting session cookies; sign in again")
	}
	if err := l.Transport.CheckPrivate(r); err != nil {
		return "", err
	}
	return values[0], nil
}

// CheckSignIn refuses a sign-in that can't be made safely: from another site (no CSRF header), over
// plain HTTP from another machine, or through a proxy that can't be checked. It returns the
// caller's address, for slowing down guessing.
func (l *Local) CheckSignIn(r *http.Request) (string, error) {
	if err := RequireCSRF(r); err != nil {
		return "", err
	}
	return l.CheckPassword(r)
}

// CheckPassword refuses a password sent over a connection that isn't private, or from a caller
// whose address can't be told, and returns that address.
func (l *Local) CheckPassword(r *http.Request) (string, error) {
	if err := l.Transport.CheckPrivate(r); err != nil {
		return "", err
	}
	client, ok := l.Transport.Client(r)
	if !ok {
		return "", errUnknownClient
	}
	return client.String(), nil
}

func (l *Local) Authenticate(r *http.Request) (domain.Principal, error) {
	if h := r.Header.Get("Authorization"); h != "" {
		token, ok := strings.CutPrefix(h, "Bearer ")
		if !ok || token == "" {
			return domain.Principal{}, domain.Errorf(domain.CodeUnauthenticated, "use Authorization: Bearer <API token>")
		}
		return l.resolver.PrincipalForToken(r.Context(), token)
	}
	session, err := l.SessionValue(r)
	if err != nil {
		return domain.Principal{}, err
	}
	if err := RequireCSRF(r); err != nil {
		return domain.Principal{}, err
	}
	return l.resolver.PrincipalForSession(r.Context(), session)
}

// RequireCSRF refuses a browser request that changes something unless it carries CSRFHeader.
func RequireCSRF(r *http.Request) error {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return nil
	}
	if r.Header.Get(CSRFHeader) == "" {
		return domain.Errorf(domain.CodeForbidden, "requests from a browser need the %s header", CSRFHeader)
	}
	return nil
}
