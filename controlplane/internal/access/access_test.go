package access_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tensora/tensora/controlplane/internal/access"
	"github.com/tensora/tensora/controlplane/internal/credential"
	"github.com/tensora/tensora/controlplane/internal/domain"
	"github.com/tensora/tensora/controlplane/internal/store"
	"github.com/tensora/tensora/controlplane/internal/testutil"
)

// targets is the agent backend's view: workload → its server's address and token.
type targets map[uuid.UUID][2]string

func (t targets) Target(id uuid.UUID) (string, string, bool) {
	v, ok := t[id]
	return v[0], v[1], ok
}

type fixture struct {
	proxy    http.Handler
	st       store.Store
	workload uuid.UUID
	tenant   uuid.UUID
	targets  targets
	seen     chan *http.Request // what the workload's server received
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	st := testutil.SeededStore(t)
	ctx := context.Background()
	f := fixture{st: st, workload: uuid.New(), seen: make(chan *http.Request, 10)}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.seen <- r.Clone(context.Background())
		http.SetCookie(w, &http.Cookie{Name: "tensora_access", Value: "planted"})
		w.Header().Add("Set-Cookie", "tensora_access =planted-with-a-space; Path=/") // a browser trims the name
		w.Header().Add("Set-Cookie", "everywhere=1; Domain=localhost; Path=/")       // for every workload's host
		http.SetCookie(w, &http.Cookie{Name: "_xsrf", Value: "jupyter-own"})
		_, _ = io.WriteString(w, "jupyter")
	}))
	t.Cleanup(upstream.Close)
	err := st.InTx(ctx, func(q store.Queries) error {
		tn, err := q.TenantBySlug(ctx, "internal")
		if err != nil {
			return err
		}
		f.tenant = tn.ID
		return q.CreateWorkload(ctx, domain.Workload{ID: f.workload, TenantID: tn.ID, Name: "nb", Kind: domain.KindNotebook,
			State: domain.StateRunning, ImageID: "pytorch-notebook", GPUClass: "cpu-2c-8g", GPUCount: 1, CreatedBy: "t",
			CreatedAt: testutil.T0, UpdatedAt: testutil.T0, Region: domain.RegionLocal})
	})
	if err != nil {
		t.Fatal(err)
	}
	addr := strings.TrimPrefix(upstream.URL, "http://")
	f.targets = targets{f.workload: {addr, "jupyter-token"}}
	f.proxy = access.New(st, f.targets, slog.New(slog.DiscardHandler), nil)
	return f
}

// ticket stores a ticket for the fixture's workload and returns it.
func (f fixture) ticket(t *testing.T) string {
	t.Helper()
	ticket, hash, _ := credential.NewToken("tat_")
	ctx := context.Background()
	if err := f.st.InTx(ctx, func(q store.Queries) error {
		return q.CreateAccessTicket(ctx, domain.AccessTicket{Hash: hash, TenantID: f.tenant, WorkloadID: f.workload,
			Subject: "owner", ExpiresAt: time.Now().Add(time.Minute)})
	}); err != nil {
		t.Fatal(err)
	}
	return ticket
}

func (f fixture) host(id uuid.UUID) string { return "w-" + id.String() + ".localhost:8090" }

func (f fixture) do(method, host, path string, cookie *http.Cookie, header map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://"+host+path, nil)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	for k, v := range header {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.proxy.ServeHTTP(rec, r)
	return rec
}

func TestOpeningAWorkload(t *testing.T) {
	f := newFixture(t)
	host := f.host(f.workload)
	if rec := f.do("GET", host, "/lab", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("without a session: %d", rec.Code)
	}
	ticket := f.ticket(t)
	rec := f.do("GET", host, "/_tensora/open?ticket="+url.QueryEscape(ticket), nil, nil)
	cookies := rec.Result().Cookies()
	if rec.Code != http.StatusOK || len(cookies) != 1 || cookies[0].Name != "tensora_access" || !cookies[0].HttpOnly ||
		cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Domain != "" {
		t.Fatalf("opening: %d %+v; want a host-only, HttpOnly, SameSite=Strict cookie", rec.Code, cookies)
	}
	if !strings.Contains(rec.Body.String(), `url=/`) || rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("opening page = %q; want it to move on from this host, without leaking the ticket", rec.Body)
	}
	session := cookies[0]

	other := &http.Cookie{Name: "_xsrf", Value: "abc"}
	r := httptest.NewRequest("GET", "http://"+host+"/api/contents", nil)
	r.AddCookie(session)
	r.AddCookie(other)
	r.Header.Set("Origin", "http://"+host)
	r.Header.Set("Authorization", "token guessed")
	rec = httptest.NewRecorder()
	f.proxy.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK || rec.Body.String() != "jupyter" {
		t.Fatalf("proxied: %d %q", rec.Code, rec.Body)
	}
	got := <-f.seen
	if got.Header.Get("Authorization") != "token jupyter-token" {
		t.Errorf("the workload got Authorization %q; want its own token, never the browser's", got.Header.Get("Authorization"))
	}
	if strings.Contains(got.Header.Get("Cookie"), "tensora_access") || !strings.Contains(got.Header.Get("Cookie"), "_xsrf=abc") {
		t.Errorf("the workload got cookies %q; want its own, never the proxy's session", got.Header.Get("Cookie"))
	}
	if !strings.HasPrefix(got.Host, "127.0.0.1:") || got.Header.Get("Origin") != "http://"+got.Host {
		t.Errorf("Host %q, Origin %q; want the loopback address the server expects", got.Host, got.Header.Get("Origin"))
	}
	kept := map[string]*http.Cookie{}
	for _, c := range rec.Result().Cookies() {
		kept[c.Name] = c
		if c.Name == "tensora_access" {
			t.Errorf("the workload replaced the proxy's cookie: %v", c)
		}
	}
	if kept["_xsrf"] == nil || kept["everywhere"] == nil || kept["everywhere"].Domain != "" {
		t.Errorf("the workload's cookies: %v; want its own kept, for its host only", rec.Result().Header.Values("Set-Cookie"))
	}

	// A ticket opens once.
	if rec := f.do("GET", host, "/_tensora/open?ticket="+url.QueryEscape(ticket), nil, nil); rec.Code != http.StatusForbidden {
		t.Errorf("a used ticket: %d; want 403", rec.Code)
	}
}

// Sessions and tickets are for one workload: another workload's host refuses them.
func TestSessionsAreForOneWorkload(t *testing.T) {
	f := newFixture(t)
	ticket := f.ticket(t)
	elsewhere := f.host(uuid.New())
	if rec := f.do("GET", elsewhere, "/_tensora/open?ticket="+url.QueryEscape(ticket), nil, nil); rec.Code != http.StatusForbidden {
		t.Errorf("a ticket at another workload's host: %d; want 403", rec.Code)
	}
	rec := f.do("GET", f.host(f.workload), "/_tensora/open?ticket="+url.QueryEscape(f.ticket(t)), nil, nil)
	session := rec.Result().Cookies()[0]
	if rec := f.do("GET", elsewhere, "/", session, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("a session at another workload's host: %d; want 401", rec.Code)
	}
}

func TestOnlyWorkloadHosts(t *testing.T) {
	f := newFixture(t)
	for _, h := range []string{"127.0.0.1:8090", "localhost:8090", "evil.example:8090", "w-not-a-uuid.localhost:8090",
		"w-" + f.workload.String() + ".localhost.evil.example"} {
		if rec := f.do("GET", h, "/", nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("host %q: %d; want 404", h, rec.Code)
		}
	}
}

func TestStoppedWorkloads(t *testing.T) {
	f := newFixture(t)
	gone := uuid.New()
	ctx := context.Background()
	ticket, hash, _ := credential.NewToken("tat_")
	_ = f.st.InTx(ctx, func(q store.Queries) error {
		if err := q.CreateWorkload(ctx, domain.Workload{ID: gone, TenantID: f.tenant, Name: "old", Kind: domain.KindNotebook,
			State: domain.StateStopped, ImageID: "pytorch-notebook", GPUClass: "cpu-2c-8g", GPUCount: 1, CreatedBy: "t",
			CreatedAt: testutil.T0, UpdatedAt: testutil.T0, Region: domain.RegionLocal}); err != nil {
			return err
		}
		return q.CreateAccessTicket(ctx, domain.AccessTicket{Hash: hash, TenantID: f.tenant, WorkloadID: gone, Subject: "o",
			ExpiresAt: time.Now().Add(time.Minute)})
	})
	rec := f.do("GET", f.host(gone), "/_tensora/open?ticket="+url.QueryEscape(ticket), nil, nil)
	if rec.Code != http.StatusServiceUnavailable || len(rec.Result().Cookies()) != 0 {
		t.Errorf("opening a workload with no running server: %d, cookies %v; want 503 and none", rec.Code, rec.Result().Cookies())
	}

	// A session ends when its workload stops: the next run's server has a new token.
	rec = f.do("GET", f.host(f.workload), "/_tensora/open?ticket="+url.QueryEscape(f.ticket(t)), nil, nil)
	session := rec.Result().Cookies()[0]
	running := f.targets[f.workload]
	delete(f.targets, f.workload)
	if rec := f.do("GET", f.host(f.workload), "/", session, nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("while stopped: %d; want 503", rec.Code)
	}
	f.targets[f.workload] = [2]string{running[0], "next-attempt-token"}
	if rec := f.do("GET", f.host(f.workload), "/", session, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("after a restart: %d; want 401", rec.Code)
	}
	f.targets[f.workload] = running
	if rec := f.do("GET", f.host(f.workload), "/", session, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("a session that ended came back: %d; want 401", rec.Code)
	}
}

// Cookies ignore ports, so a page on another port of a workload's host is same-site and its requests
// carry the session: the proxy refuses requests a browser says came from another origin.
func TestRequestsFromElsewhere(t *testing.T) {
	f := newFixture(t)
	host := f.host(f.workload)
	rec := f.do("GET", host, "/_tensora/open?ticket="+url.QueryEscape(f.ticket(t)), nil, nil)
	session := rec.Result().Cookies()[0]
	for _, tt := range []struct {
		name   string
		header map[string]string
		want   int
	}{
		{"another port of its host", map[string]string{"Origin": "http://w-" + f.workload.String() + ".localhost:7777"}, http.StatusForbidden},
		{"another site", map[string]string{"Origin": "http://evil.example"}, http.StatusForbidden},
		{"a page that says null", map[string]string{"Origin": "null"}, http.StatusForbidden},
		{"a same-site fetch", map[string]string{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "cors"}, http.StatusForbidden},
		{"a cross-site image", map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "no-cors"}, http.StatusForbidden},
		{"a navigation from elsewhere", map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate"}, http.StatusOK},
		{"its own page", map[string]string{"Origin": "http://" + host, "Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors"}, http.StatusOK},
		{"no browser headers", nil, http.StatusOK},
	} {
		if rec := f.do("POST", host, "/api/contents", session, tt.header); rec.Code != tt.want {
			t.Errorf("%s: %d; want %d", tt.name, rec.Code, tt.want)
		}
		if tt.want == http.StatusOK {
			<-f.seen
		}
	}
}
