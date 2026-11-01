// Package auth turns an HTTP request into a domain.Principal.
package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
)

type Authenticator interface {
	Authenticate(r *http.Request) (domain.Principal, error)
}

// Dev trusts the X-Dev-Tenant and X-Dev-User headers. config refuses it outside TENSORAFI_ENV=dev.
type Dev struct{}

func (Dev) Authenticate(r *http.Request) (domain.Principal, error) {
	tenant := r.Header.Get("X-Dev-Tenant")
	if tenant == "" {
		return domain.Principal{}, domain.Errorf(domain.CodeUnauthenticated, "dev auth: set the X-Dev-Tenant header")
	}
	user := r.Header.Get("X-Dev-User")
	if user == "" {
		user = "dev@" + tenant
	}
	return domain.Principal{Subject: user, Email: user, TenantSlug: tenant, Via: domain.AuthDev}, nil
}

// OIDC validates bearer JWTs issued by Keycloak. The tenant comes from a custom claim set by
// a Keycloak protocol mapper (see deploy/platform/keycloak/README.md).
type OIDC struct {
	verifier    *oidc.IDTokenVerifier
	tenantClaim string
}

// NewOIDC performs discovery against issuer. ctx must live as long as the process: the
// verifier uses it to refresh signing keys.
func NewOIDC(ctx context.Context, issuer, audience, tenantClaim string) (*OIDC, error) {
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery for %s: %w", issuer, err)
	}
	return &OIDC{verifier: provider.Verifier(&oidc.Config{ClientID: audience}), tenantClaim: tenantClaim}, nil
}

func (o *OIDC) Authenticate(r *http.Request) (domain.Principal, error) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || raw == "" {
		return domain.Principal{}, domain.Errorf(domain.CodeUnauthenticated, "missing bearer token")
	}
	tok, err := o.verifier.Verify(r.Context(), raw)
	if err != nil {
		return domain.Principal{}, domain.Errorf(domain.CodeUnauthenticated, "invalid or expired token")
	}
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return domain.Principal{}, domain.Errorf(domain.CodeUnauthenticated, "unreadable token claims")
	}
	tenant, _ := claims[o.tenantClaim].(string)
	if tenant == "" {
		return domain.Principal{}, domain.Errorf(domain.CodeForbidden, "token has no %s claim", o.tenantClaim)
	}
	email, _ := claims["email"].(string)
	return domain.Principal{Subject: tok.Subject, Email: email, TenantSlug: tenant, Via: domain.AuthOIDC}, nil
}
