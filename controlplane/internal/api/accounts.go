package api

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/tensorafi/controlplane/internal/auth"
	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
	"github.com/krishnanand20/tensorafi/controlplane/internal/service"
)

// Local accounts (ADR-0012): sign-in, sessions, and API tokens. These routes exist only when the
// server authenticates with auth.Local; GET /v1/auth/me works in every mode.

type meView struct {
	Subject  string `json:"subject"`
	Email    string `json:"email,omitempty"`
	Tenant   string `json:"tenant"`
	AuthMode string `json:"auth_mode"` // local, oidc, or dev
}

type tokenView struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// createdTokenView is the only response that ever contains a token.
type createdTokenView struct {
	tokenView
	Token string `json:"token"`
}

func optional(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func toTokenView(t domain.APIToken) tokenView {
	return tokenView{ID: t.ID, Name: t.Name, Prefix: t.Prefix, CreatedAt: t.CreatedAt,
		ExpiresAt: optional(t.ExpiresAt), LastUsedAt: optional(t.LastUsedAt), RevokedAt: optional(t.RevokedAt)}
}

func (s *Server) authMode() string {
	switch s.auth.(type) {
	case *auth.Local:
		return "local"
	case *auth.OIDC:
		return "oidc"
	}
	return "dev"
}

func (s *Server) me(w http.ResponseWriter, _ *http.Request, p domain.Principal) error {
	writeJSON(w, http.StatusOK, meView{Subject: p.Subject, Email: p.Email, Tenant: p.TenantSlug, AuthMode: s.authMode()})
	return nil
}

// sessionCookie is HttpOnly (scripts can't read it), SameSite=Strict (other sites' requests don't
// carry it), and over HTTPS Secure with the __Host- name (no other host can set or replace it).
// Plain HTTP is allowed only where Transport.Private says passwords may cross the connection.
func (s *Server) sessionCookie(r *http.Request, value string, maxAge int) *http.Cookie {
	t := s.local.Transport
	return &http.Cookie{ //nolint:gosec // Secure is set exactly when the connection is HTTPS (see above)
		Name: t.SessionCookieName(r), Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: t.Secure(r),
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	client := ""
	err := func() error {
		// Also for sign-in: otherwise another site could sign a visitor into the attacker's account.
		var err error
		if client, err = s.local.CheckSignIn(r); err != nil {
			return err
		}
		if err := decodeJSON(r, &in); err != nil {
			return err
		}
		token, err := s.svc.Login(r.Context(), in.Email, in.Password, client)
		if err != nil {
			return err
		}
		p, err := s.svc.PrincipalForSession(r.Context(), token)
		if err != nil {
			return err
		}
		info := requestInfoFrom(r.Context())
		info.tenant, info.subject = p.TenantSlug, p.Subject
		s.log.InfoContext(r.Context(), "signed in", append(info.logAttrs(), "client", client)...)
		http.SetCookie(w, s.sessionCookie(r, token, int(service.SessionTTL/time.Second)))
		writeJSON(w, http.StatusOK, meView{Subject: p.Subject, Email: p.Email, Tenant: p.TenantSlug, AuthMode: "local"})
		return nil
	}()
	if err != nil {
		// Failed and refused sign-ins are logged for the operator (never the password).
		email, _ := domain.NormalizeEmail(in.Email)
		s.log.WarnContext(r.Context(), "sign-in refused", append(requestInfoFrom(r.Context()).logAttrs(),
			"client", client, "peer", r.RemoteAddr, "email", email, "reason", userMessage(err))...)
		s.writeError(w, r, err)
	}
}

// userMessage is the safe part of an error, for logs: a domain error's message, or "internal".
func userMessage(err error) string {
	if c, ok := domain.CodeOf(err); ok {
		return string(c)
	}
	return "internal"
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, _ domain.Principal) error {
	if session, err := s.local.SessionValue(r); err == nil {
		if err := s.svc.Logout(r.Context(), session); err != nil {
			return err
		}
	}
	for _, name := range []string{auth.SessionCookie, auth.SecureSessionCookie} {
		c := s.sessionCookie(r, "", -1) //nolint:gosec // HttpOnly and SameSite=Strict; Secure as below
		c.Name = name
		c.Secure = c.Secure || name == auth.SecureSessionCookie
		http.SetCookie(w, c)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "signed out"})
	return nil
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, p domain.Principal) error {
	var in struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if err := decodeJSON(r, &in); err != nil {
		return err
	}
	client, err := s.local.CheckPassword(r)
	if err != nil {
		return err
	}
	session, _ := s.local.SessionValue(r)
	live, err := s.svc.ChangePassword(r.Context(), p, in.Current, in.New, session, client)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":            "password changed; your other sessions are signed out",
		"api_tokens_active": live, // they keep working: revoke them too if the password leaked
	})
	return nil
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request, p domain.Principal) error {
	tokens, err := s.svc.ListAPITokens(r.Context(), p)
	if err != nil {
		return err
	}
	items := make([]tokenView, len(tokens))
	for i, t := range tokens {
		items[i] = toTokenView(t)
	}
	writeJSON(w, http.StatusOK, listView[tokenView]{Items: items})
	return nil
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request, p domain.Principal) error {
	var in struct {
		Name          string `json:"name"`
		ExpiresInDays int    `json:"expires_in_days"` // 0: never
		Password      string `json:"password"`        // the current one: a token outlives the session
	}
	if err := decodeJSON(r, &in); err != nil {
		return err
	}
	if in.ExpiresInDays < 0 || in.ExpiresInDays > 366 {
		return domain.Errorf(domain.CodeInvalid, "expires_in_days must be between 0 (never) and 366")
	}
	client, err := s.local.CheckPassword(r)
	if err != nil {
		return err
	}
	token, t, err := s.svc.CreateAPIToken(r.Context(), p, in.Name, time.Duration(in.ExpiresInDays)*24*time.Hour, in.Password, client)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, createdTokenView{tokenView: toTokenView(t), Token: token})
	return nil
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request, p domain.Principal) error {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return domain.Errorf(domain.CodeNotFound, "token not found")
	}
	if err := s.svc.RevokeAPIToken(r.Context(), p, id); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
	return nil
}
