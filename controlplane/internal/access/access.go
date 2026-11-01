// Package access is the access proxy (ADR-0017). It opens a workload's web server in the browser
// at the workload's own host, w-<id>.localhost: a different site from the dashboard and from every
// other workload, so none can use another's cookies. A one-time ticket from the API opens it; the
// proxy then keeps a session in a cookie for that host only, and adds the server's own token on
// the way in, so the token never reaches the browser.
package access

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/tensorafi/controlplane/internal/credential"
	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
	"github.com/krishnanand20/tensorafi/controlplane/internal/store"
)

// Targets finds the Unix socket a running workload's server listens on, and its token: the agent
// backend.
type Targets interface {
	Target(id uuid.UUID) (socket, token string, ok bool)
}

const (
	cookieName  = "tensorafi_access"
	sessionTTL  = 12 * time.Hour
	openPath    = "/_tensorafi/open"
	maxSessions = 10000
)

// host is a workload's own host: w-<workload ID>.localhost, with any port.
var host = regexp.MustCompile(`^w-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.localhost(:[0-9]{1,5})?$`)

type Server struct {
	store   store.Store
	targets Targets
	log     *slog.Logger
	now     func() time.Time
	tokens  TokenResolver

	mu       sync.Mutex
	sessions map[string]session // by the SHA-256 of the cookie's value
}

type session struct {
	workload uuid.UUID
	server   string // the SHA-256 of the token of the server it opened: a new attempt has a new one
	subject  string
	expires  time.Time
}

func New(st store.Store, targets Targets, log *slog.Logger, now func() time.Time) *Server {
	if now == nil {
		now = time.Now
	}
	return &Server{store: st, targets: targets, log: log, now: now, sessions: map[string]session{}}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m := host.FindStringSubmatch(strings.ToLower(r.Host))
	if m == nil {
		http.NotFound(w, r) // only workload hosts: never a name an attacker's page could point here
		return
	}
	id := uuid.MustParse(m[1])
	if r.URL.Path == openPath {
		s.open(w, r, id)
		return
	}
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || strings.HasPrefix(r.URL.Path, "/v1/") {
		s.endpoint(w, r, id)
		return
	}
	key, sess, ok := s.session(r, id)
	if !ok {
		page(w, http.StatusUnauthorized, "Open this workload from the TensoraFi dashboard.")
		return
	}
	if fromElsewhere(r) {
		page(w, http.StatusForbidden, "This request came from another site.")
		return
	}
	socket, token, ok := s.targets.Target(id)
	switch {
	case !ok:
		page(w, http.StatusServiceUnavailable, "This workload isn't running right now.")
		return
	case sess.server != credential.HashToken(token):
		// A session ends when its workload stops: the server that runs next is another one.
		s.mu.Lock()
		delete(s.sessions, key)
		s.mu.Unlock()
		page(w, http.StatusUnauthorized, "This workload has restarted since it was opened. Open it again from the TensoraFi dashboard.")
		return
	}
	s.proxy(socket, token).ServeHTTP(w, r)
}

// open takes a one-time ticket and starts a session for this workload's host. It answers with a
// page that moves on from this host, not a redirect: a SameSite=Strict cookie set during a
// navigation from another site (the dashboard) isn't sent until a navigation from this one.
func (s *Server) open(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	now := s.now().UTC()
	var t domain.AccessTicket
	err := s.store.InTx(r.Context(), func(q store.Queries) error {
		var err error
		t, err = q.UseAccessTicket(r.Context(), credential.HashToken(r.URL.Query().Get("ticket")), now)
		return err
	})
	switch {
	case domain.IsNotFound(err):
		page(w, http.StatusForbidden, "This link has been used or has expired. Open the workload again from the dashboard.")
		return
	case err != nil:
		s.log.Error("couldn't use an access ticket", "err", err)
		page(w, http.StatusInternalServerError, "Something went wrong opening this workload.")
		return
	case t.WorkloadID != id:
		page(w, http.StatusForbidden, "This link is for another workload.")
		return
	}
	_, token, ok := s.targets.Target(id)
	if !ok {
		page(w, http.StatusServiceUnavailable, "This workload isn't running right now.")
		return
	}
	value, hash, err := credential.NewToken("")
	if err != nil {
		page(w, http.StatusInternalServerError, "Something went wrong opening this workload.")
		return
	}
	s.mu.Lock()
	if len(s.sessions) >= maxSessions {
		for k, sess := range s.sessions {
			if !now.Before(sess.expires) {
				delete(s.sessions, k)
			}
		}
	}
	for len(s.sessions) >= maxSessions { // still full: the oldest goes
		var oldest string
		for k, sess := range s.sessions {
			if oldest == "" || sess.expires.Before(s.sessions[oldest].expires) {
				oldest = k
			}
		}
		delete(s.sessions, oldest)
	}
	s.sessions[hash] = session{workload: id, server: credential.HashToken(token), subject: t.Subject, expires: now.Add(sessionTTL)}
	s.mu.Unlock()
	s.log.Info("workload opened", "workload", id, "tenant", t.TenantID, "subject", t.Subject)
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // HttpOnly and SameSite=Strict; plain HTTP on this machine only (*.localhost)
		Name: cookieName, Value: value, Path: "/", MaxAge: int(sessionTTL / time.Second),
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer") // the ticket is in this page's URL
	_, _ = fmt.Fprint(w, `<!doctype html><meta http-equiv="refresh" content="0; url=/"><title>Opening…</title><p>Opening…`)
}

// fromElsewhere reports whether a browser says r comes from a page of another origin and isn't a
// navigation. Cookies ignore ports, so a page on another port of this workload's host (any program
// can listen on one) is same-site, and its requests carry the session: they must not reach the
// workload, which would take them as its own page's (its Origin is rewritten on the way in).
func fromElsewhere(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" && !strings.EqualFold(o, "http://"+r.Host) {
		return true
	}
	site := r.Header.Get("Sec-Fetch-Site")
	return (site == "same-site" || site == "cross-site") && r.Header.Get("Sec-Fetch-Mode") != "navigate"
}

// session finds r's session for workload id, and its key.
func (s *Server) session(r *http.Request, id uuid.UUID) (string, session, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return "", session{}, false
	}
	key := credential.HashToken(c.Value)
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[key]
	if !ok || sess.workload != id || !s.now().UTC().Before(sess.expires) {
		return "", session{}, false
	}
	return key, sess, true
}

// proxy forwards to the workload's server on its Unix socket, as if from localhost (the host its
// server expects), with its token, and without the proxy's cookie: the workload never sees the
// session that reaches it. Connections aren't reused, so one workload's can never carry another's
// requests.
func (s *Server) proxy(socket, token string) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
			DisableKeepAlives: true,
		},
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "http", Host: "localhost"})
			pr.Out.Host = "localhost"
			pr.Out.Header.Set("Authorization", "token "+token)
			if pr.In.Header.Get("Origin") != "" {
				pr.Out.Header.Set("Origin", "http://localhost")
			}
			var kept []string
			for _, c := range pr.In.Cookies() {
				if c.Name != cookieName {
					kept = append(kept, c.String())
				}
			}
			pr.Out.Header.Del("Cookie")
			if len(kept) > 0 {
				pr.Out.Header.Set("Cookie", strings.Join(kept, "; "))
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			// The workload can't replace the proxy's cookie, or set cookies for other hosts: each is
			// parsed as a browser would, and kept host-only. One that doesn't parse is dropped.
			var kept []string
			for _, v := range resp.Header.Values("Set-Cookie") {
				c, err := http.ParseSetCookie(v)
				if err != nil || c.Name == cookieName {
					continue
				}
				c.Domain = ""
				if v := c.String(); v != "" {
					kept = append(kept, v)
				}
			}
			resp.Header.Del("Set-Cookie")
			for _, v := range kept {
				resp.Header.Add("Set-Cookie", v)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if !errors.Is(err, http.ErrAbortHandler) {
				s.log.Warn("couldn't reach a workload", "host", r.Host, "err", err)
			}
			page(w, http.StatusBadGateway, "This workload isn't answering right now.")
		},
	}
}

// page is a short message for the browser.
func page(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, "<!doctype html><title>TensoraFi</title><p>%s", html.EscapeString(msg))
}
