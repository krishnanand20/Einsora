package api_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/tensorafi/controlplane/internal/api"
	"github.com/krishnanand20/tensorafi/controlplane/internal/auth"
	"github.com/krishnanand20/tensorafi/controlplane/internal/credential"
	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
	"github.com/krishnanand20/tensorafi/controlplane/internal/service"
	"github.com/krishnanand20/tensorafi/controlplane/internal/store"
	"github.com/krishnanand20/tensorafi/controlplane/internal/store/memstore"
	"github.com/krishnanand20/tensorafi/controlplane/internal/testutil"
)

const (
	ownerCreds = `{"email":"owner@example.com","password":"correct horse battery staple"}`
	// ownerPassword ends a JSON object that needs the owner's password, such as a new token's.
	ownerPassword = `,"password":"correct horse battery staple"`
)

// localServer is an API in local auth mode with the owner set up. Test requests come from
// 192.0.2.1, another machine, so it allows plain HTTP, as an operator may on a network they trust.
func localServer(t *testing.T) http.Handler {
	t.Helper()
	h, _ := localServerWith(t, auth.Transport{AllowPlainHTTP: true})
	return h
}

func localServerWith(t *testing.T, tr auth.Transport) (http.Handler, store.Store) {
	t.Helper()
	st := memstore.New()
	svc := service.New(st, testutil.Catalog(t), nil)
	if _, err := svc.SetupOwner(context.Background(), service.SetupInput{
		Email: "owner@example.com", Password: "correct horse battery staple", TenantSlug: "home",
	}); err != nil {
		t.Fatal(err)
	}
	return api.New(svc, auth.NewLocal(svc, tr), slog.New(slog.DiscardHandler), st.Ping).Handler(), st
}

type request struct {
	method, path, body string
	cookie             *http.Cookie
	cookies            []*http.Cookie
	bearer             string
	csrf               bool
	remote             string            // the peer's address; httptest's default is 192.0.2.1
	header             map[string]string // e.g. X-Forwarded-For from a proxy
}

func do(t *testing.T, h http.Handler, r request) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(r.method, r.path, strings.NewReader(r.body))
	if r.remote != "" {
		req.RemoteAddr = r.remote
	}
	for k, v := range r.header {
		req.Header.Set(k, v)
	}
	if r.cookie != nil {
		req.AddCookie(r.cookie)
	}
	for _, c := range r.cookies {
		req.AddCookie(c)
	}
	if r.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+r.bearer)
	}
	if r.csrf {
		req.Header.Set(auth.CSRFHeader, "1")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestBrowserSignInFlow(t *testing.T) {
	h := localServer(t)
	creds := `{"email":"owner@example.com","password":"correct horse battery staple"}`

	if rec := do(t, h, request{method: "POST", path: "/v1/auth/login", body: creds}); rec.Code != http.StatusForbidden {
		t.Fatalf("sign-in without the CSRF header: %d; want 403", rec.Code)
	}
	if rec := do(t, h, request{method: "POST", path: "/v1/auth/login", csrf: true,
		body: `{"email":"owner@example.com","password":"wrong password!!"}`}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", rec.Code)
	}
	rec := do(t, h, request{method: "POST", path: "/v1/auth/login", body: creds, csrf: true})
	if rec.Code != http.StatusOK {
		t.Fatalf("sign-in: %d %s", rec.Code, rec.Body)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != auth.SessionCookie || !cookies[0].HttpOnly ||
		cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].MaxAge <= 0 {
		t.Fatalf("session cookie = %+v; want HttpOnly, SameSite=Strict, with an expiry", cookies)
	}
	session := cookies[0]

	rec = do(t, h, request{method: "GET", path: "/v1/auth/me", cookie: session})
	var me map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	if rec.Code != http.StatusOK || me["email"] != "owner@example.com" || me["tenant"] != "home" || me["auth_mode"] != "local" {
		t.Fatalf("me = %d %v", rec.Code, me)
	}

	// Changing something with only a cookie needs the CSRF header.
	body := `{"name":"nb","kind":"notebook","image_id":"pytorch-notebook","gpu_class":"l40s-48gb","gpu_count":1}`
	if rec := do(t, h, request{method: "POST", path: "/v1/workloads", body: body, cookie: session}); rec.Code != http.StatusForbidden {
		t.Errorf("cookie POST without CSRF header: %d; want 403", rec.Code)
	}
	if rec := do(t, h, request{method: "POST", path: "/v1/workloads", body: body, cookie: session, csrf: true}); rec.Code != http.StatusCreated {
		t.Errorf("cookie POST with CSRF header: %d %s", rec.Code, rec.Body)
	}

	if rec := do(t, h, request{method: "POST", path: "/v1/auth/logout", cookie: session, csrf: true}); rec.Code != http.StatusOK {
		t.Fatalf("sign-out: %d", rec.Code)
	}
	if rec := do(t, h, request{method: "GET", path: "/v1/auth/me", cookie: session}); rec.Code != http.StatusUnauthorized {
		t.Errorf("after sign-out: %d; want 401", rec.Code)
	}
}

func TestAPITokenFlow(t *testing.T) {
	h := localServer(t)
	rec := do(t, h, request{method: "POST", path: "/v1/auth/login", csrf: true,
		body: `{"email":"owner@example.com","password":"correct horse battery staple"}`})
	session := rec.Result().Cookies()[0]

	rec = do(t, h, request{method: "POST", path: "/v1/tokens", body: `{"name":"laptop","expires_in_days":90` + ownerPassword + `}`, cookie: session, csrf: true})
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	token, _ := created["token"].(string)
	if rec.Code != http.StatusCreated || !strings.HasPrefix(token, "tsk_") || created["expires_at"] == nil {
		t.Fatalf("create token: %d %v", rec.Code, created)
	}

	// The token works without cookies or a CSRF header: it isn't something a browser sends by itself.
	if rec := do(t, h, request{method: "POST", path: "/v1/workloads", bearer: token,
		body: `{"name":"nb","kind":"notebook","image_id":"pytorch-notebook","gpu_class":"l40s-48gb","gpu_count":1}`}); rec.Code != http.StatusCreated {
		t.Errorf("POST with an API token: %d %s", rec.Code, rec.Body)
	}

	rec = do(t, h, request{method: "GET", path: "/v1/tokens", bearer: token})
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), token) || !strings.Contains(rec.Body.String(), `"laptop"`) {
		t.Fatalf("list tokens: %d %s; a token is shown only once", rec.Code, rec.Body)
	}

	id, _ := created["id"].(string)
	if rec := do(t, h, request{method: "DELETE", path: "/v1/tokens/" + id, bearer: token}); rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, request{method: "GET", path: "/v1/auth/me", bearer: token}); rec.Code != http.StatusUnauthorized {
		t.Errorf("revoked token: %d; want 401", rec.Code)
	}
}

// signIn signs in as the owner and returns the session cookie.
func signIn(t *testing.T, h http.Handler, r request) *http.Cookie {
	t.Helper()
	r.method, r.csrf = "POST", true
	if r.path == "" {
		r.path = "/v1/auth/login"
	}
	if r.body == "" {
		r.body = ownerCreds
	}
	rec := do(t, h, r)
	if rec.Code != http.StatusOK || len(rec.Result().Cookies()) != 1 {
		t.Fatalf("sign-in: %d %s", rec.Code, rec.Body)
	}
	return rec.Result().Cookies()[0]
}

// Passwords and session cookies cross only private connections: HTTPS, or this machine, unless the
// operator allows plain HTTP. Over HTTPS the cookie gets the __Host- name.
func TestSignInNeedsAPrivateConnection(t *testing.T) {
	proxy := netip.MustParsePrefix("10.0.0.5/32")
	tests := []struct {
		name      string
		tr        auth.Transport
		r         request
		ok        bool
		wantHTTPS bool
	}{
		{"plain HTTP from another machine", auth.Transport{}, request{}, false, false},
		{"this machine", auth.Transport{}, request{remote: "127.0.0.1:5000"}, true, false},
		{"this machine over IPv6", auth.Transport{}, request{remote: "[::1]:5000"}, true, false},
		{"plain HTTP, allowed by the operator", auth.Transport{AllowPlainHTTP: true}, request{}, true, false},
		{"direct HTTPS", auth.Transport{}, request{path: "https://tensorafi.example/v1/auth/login"}, true, true},
		{"HTTPS through a trusted proxy", auth.Transport{TrustedProxies: []netip.Prefix{proxy}},
			request{remote: "10.0.0.5:5000", header: map[string]string{"X-Forwarded-For": "203.0.113.9", "X-Forwarded-Proto": "https"}}, true, true},
		{"a trusted proxy that doesn't say who for", auth.Transport{TrustedProxies: []netip.Prefix{proxy}},
			request{remote: "10.0.0.5:5000", header: map[string]string{"X-Forwarded-Proto": "https"}}, false, false},
		{"plain HTTP through a trusted proxy", auth.Transport{TrustedProxies: []netip.Prefix{proxy}},
			request{remote: "10.0.0.5:5000", header: map[string]string{"X-Forwarded-For": "203.0.113.9", "X-Forwarded-Proto": "http"}}, false, false},
		{"an untrusted peer claiming HTTPS", auth.Transport{},
			request{header: map[string]string{"X-Forwarded-Proto": "https"}}, false, false},
		{"a proxy on this machine, forwarding plain HTTP", auth.Transport{TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}},
			request{remote: "127.0.0.1:5000", header: map[string]string{"X-Forwarded-For": "203.0.113.9"}}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _ := localServerWith(t, tt.tr)
			r := tt.r
			path := r.path
			if path == "" {
				path = "/v1/auth/login"
			}
			r.method, r.path, r.body, r.csrf = "POST", path, ownerCreds, true
			rec := do(t, h, r)
			if !tt.ok {
				if rec.Code != http.StatusForbidden || len(rec.Result().Cookies()) != 0 {
					t.Fatalf("sign-in: %d %v; want 403 and no cookie", rec.Code, rec.Result().Cookies())
				}
				return
			}
			cookies := rec.Result().Cookies()
			if rec.Code != http.StatusOK || len(cookies) != 1 {
				t.Fatalf("sign-in: %d %s", rec.Code, rec.Body)
			}
			c := cookies[0]
			wantName := auth.SessionCookie
			if tt.wantHTTPS {
				wantName = auth.SecureSessionCookie
			}
			if c.Name != wantName || c.Secure != tt.wantHTTPS || c.Path != "/" || c.Domain != "" {
				t.Errorf("cookie = %+v; want %s, Secure=%v, Path=/, no Domain", c, wantName, tt.wantHTTPS)
			}
		})
	}

	// A session cookie isn't accepted over plain HTTP from another machine either.
	h, _ := localServerWith(t, auth.Transport{})
	session := signIn(t, h, request{remote: "127.0.0.1:5000"})
	if rec := do(t, h, request{method: "GET", path: "/v1/auth/me", cookie: session, remote: "127.0.0.1:5000"}); rec.Code != http.StatusOK {
		t.Errorf("session on this machine: %d", rec.Code)
	}
	if rec := do(t, h, request{method: "GET", path: "/v1/auth/me", cookie: session}); rec.Code != http.StatusForbidden {
		t.Errorf("session over plain HTTP from another machine: %d; want 403", rec.Code)
	}
}

// Over HTTPS only the __Host- cookie counts: one with the other name may have been planted by a
// plain-HTTP response, another port, or a sibling subdomain, so it's ignored, not trusted.
func TestOverHTTPSOnlyTheHostCookieCounts(t *testing.T) {
	h, _ := localServerWith(t, auth.Transport{})
	secure := signIn(t, h, request{path: "https://tensorafi.example/v1/auth/login"})
	if secure.Name != auth.SecureSessionCookie {
		t.Fatalf("cookie over HTTPS = %s", secure.Name)
	}
	planted := &http.Cookie{Name: auth.SessionCookie, Value: "planted"}
	if rec := do(t, h, request{method: "GET", path: "https://tensorafi.example/v1/auth/me", cookies: []*http.Cookie{planted, secure}}); rec.Code != http.StatusOK {
		t.Errorf("with a planted plain cookie beside the real one: %d; want it ignored", rec.Code)
	}
	plain := &http.Cookie{Name: auth.SessionCookie, Value: secure.Value}
	if rec := do(t, h, request{method: "GET", path: "https://tensorafi.example/v1/auth/me", cookie: plain}); rec.Code != http.StatusUnauthorized {
		t.Errorf("the session under the plain name over HTTPS: %d; want 401", rec.Code)
	}
}

// Making a token needs the password again: a token outlives the session that makes it.
func TestMakingATokenNeedsThePassword(t *testing.T) {
	h := localServer(t)
	session := signIn(t, h, request{})
	for name, body := range map[string]string{
		"no password":    `{"name":"x"}`,
		"wrong password": `{"name":"x","password":"wrong password!!"}`,
	} {
		if rec := do(t, h, request{method: "POST", path: "/v1/tokens", body: body, cookie: session, csrf: true}); rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d; want 403", name, rec.Code)
		}
	}
}

// A proxy on this machine that isn't listed can't be used to sign in: everyone behind it would
// share one limit on guessing, and what it says about HTTPS can't be believed.
func TestUnlistedProxyCantSignIn(t *testing.T) {
	h, _ := localServerWith(t, auth.Transport{})
	rec := do(t, h, request{method: "POST", path: "/v1/auth/login", body: ownerCreds, csrf: true, remote: "127.0.0.1:5000",
		header: map[string]string{"X-Forwarded-For": "198.51.100.7", "X-Forwarded-Proto": "https"}})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "TENSORAFI_TRUSTED_PROXIES") {
		t.Errorf("sign-in through an unlisted local proxy: %d %s", rec.Code, rec.Body)
	}
}

// Two session cookies mean one may have been planted by another page on this host: refuse both.
func TestConflictingSessionCookiesAreRefused(t *testing.T) {
	h := localServer(t)
	session := signIn(t, h, request{})
	twice := &http.Cookie{Name: auth.SessionCookie, Value: "planted"}
	if rec := do(t, h, request{method: "GET", path: "/v1/auth/me", cookies: []*http.Cookie{twice, session}}); rec.Code != http.StatusUnauthorized {
		t.Errorf("the same cookie twice: %d; want 401", rec.Code)
	}
}

// A stolen API token can't make more tokens or change the password; those need a browser session.
func TestTokensCantMakeTokensOrChangeThePassword(t *testing.T) {
	h := localServer(t)
	session := signIn(t, h, request{})
	rec := do(t, h, request{method: "POST", path: "/v1/tokens", body: `{"name":"script"` + ownerPassword + `}`, cookie: session, csrf: true})
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	token, _ := created["token"].(string)
	if rec.Code != http.StatusCreated || token == "" {
		t.Fatalf("create token: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, request{method: "POST", path: "/v1/tokens", body: `{"name":"more"` + ownerPassword + `}`, bearer: token}); rec.Code != http.StatusForbidden {
		t.Errorf("a token making a token: %d; want 403", rec.Code)
	}
	if rec := do(t, h, request{method: "POST", path: "/v1/auth/password", bearer: token,
		body: `{"current_password":"correct horse battery staple","new_password":"a brand new passphrase"}`}); rec.Code != http.StatusForbidden {
		t.Errorf("a token changing the password: %d; want 403", rec.Code)
	}

	rec = do(t, h, request{method: "POST", path: "/v1/auth/password", cookie: session, csrf: true,
		body: `{"current_password":"correct horse battery staple","new_password":"a brand new passphrase"}`})
	var changed map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &changed)
	if rec.Code != http.StatusOK || changed["api_tokens_active"] != float64(1) {
		t.Errorf("change password: %d %v; want it to say 1 API token still works", rec.Code, changed)
	}
}

// Tokens are their owner's alone: another user, in the same tenant or another, gets 404 (rule 8).
func TestTokensAreTheirOwnersOnly(t *testing.T) {
	h, st := localServerWith(t, auth.Transport{AllowPlainHTTP: true})
	ctx := context.Background()
	hash, _ := credential.HashPassword("another long passphrase")
	err := st.InTx(ctx, func(q store.Queries) error {
		home, err := q.TenantBySlug(ctx, "home")
		if err != nil {
			return err
		}
		other := domain.Tenant{ID: uuid.New(), Slug: "other", Name: "other", Tier: domain.TierTrusted, CreatedAt: time.Now().UTC()}
		if err := q.CreateTenant(ctx, other); err != nil {
			return err
		}
		for email, tenant := range map[string]uuid.UUID{"mate@example.com": home.ID, "stranger@example.com": other.ID} {
			if err := q.CreateUser(ctx, domain.User{ID: uuid.New(), TenantID: tenant, Email: email, PasswordHash: hash,
				CreatedAt: time.Now().UTC()}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	owner := signIn(t, h, request{})
	rec := do(t, h, request{method: "POST", path: "/v1/tokens", body: `{"name":"laptop"` + ownerPassword + `}`, cookie: owner, csrf: true})
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id, _ := created["id"].(string)
	token, _ := created["token"].(string)

	for _, email := range []string{"mate@example.com", "stranger@example.com"} {
		session := signIn(t, h, request{body: `{"email":"` + email + `","password":"another long passphrase"}`})
		if rec := do(t, h, request{method: "DELETE", path: "/v1/tokens/" + id, cookie: session, csrf: true}); rec.Code != http.StatusNotFound {
			t.Errorf("%s revoking the owner's token: %d; want 404", email, rec.Code)
		}
		if rec := do(t, h, request{method: "GET", path: "/v1/tokens", cookie: session}); strings.Contains(rec.Body.String(), id) {
			t.Errorf("%s can see the owner's token: %s", email, rec.Body)
		}
	}
	if rec := do(t, h, request{method: "GET", path: "/v1/auth/me", bearer: token}); rec.Code != http.StatusOK {
		t.Errorf("the owner's token after the others tried: %d", rec.Code)
	}
}

// Refusals for too many attempts say when to come back.
func TestTooManySignInsSayWhenToRetry(t *testing.T) {
	h := localServer(t)
	wrong := `{"email":"owner@example.com","password":"wrong password!!"}`
	for range 10 {
		do(t, h, request{method: "POST", path: "/v1/auth/login", body: wrong, csrf: true})
	}
	rec := do(t, h, request{method: "POST", path: "/v1/auth/login", body: ownerCreds, csrf: true})
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Errorf("sign-in after 10 failures: %d, Retry-After %q; want 429 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestDevModeHasNoSignIn(t *testing.T) {
	h := newHandler(t)
	if rec := do(t, h, request{method: "POST", path: "/v1/auth/login", csrf: true, body: `{}`}); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("dev-mode sign-in route: %d; want it absent", rec.Code)
	}
}
