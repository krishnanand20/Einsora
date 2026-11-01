package webui_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/krishnanand20/einsora/controlplane/internal/webui"
)

var dashboard = fstest.MapFS{
	"index.html":         {Data: []byte("<!doctype html><div id=root></div>")},
	"assets/app-1a2b.js": {Data: []byte("console.log(1)")},
	"favicon.svg":        {Data: []byte("<svg/>")},
}

func get(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestDashboardRoutes(t *testing.T) {
	h := webui.Handler(dashboard)
	tests := []struct {
		path, wantBody string
		wantCode       int
	}{
		{"/", "<div id=root>", http.StatusOK},
		{"/workloads/0b1c", "<div id=root>", http.StatusOK}, // the dashboard's own route, on a refresh
		{"/index.html", "<div id=root>", http.StatusOK},
		{"/assets/app-1a2b.js", "console.log", http.StatusOK},
		{"/favicon.svg", "<svg/>", http.StatusOK},
		{"/assets/missing.js", "404", http.StatusNotFound},    // a missing file is not the dashboard
		{"/../../etc/passwd", "<div id=root>", http.StatusOK}, // cleaned: never outside the files
	}
	for _, tt := range tests {
		rec := get(t, h, http.MethodGet, tt.path)
		if rec.Code != tt.wantCode || !strings.Contains(rec.Body.String(), tt.wantBody) {
			t.Errorf("%s: %d %q; want %d with %q", tt.path, rec.Code, rec.Body.String(), tt.wantCode, tt.wantBody)
		}
	}
}

func TestHeaders(t *testing.T) {
	h := webui.Handler(dashboard)
	page := get(t, h, http.MethodGet, "/")
	for k, want := range map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"Cache-Control":          "no-cache",
	} {
		if got := page.Header().Get(k); got != want {
			t.Errorf("%s = %q; want %q", k, got, want)
		}
	}
	if csp := page.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") ||
		!strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP = %q", csp)
	}
	if c := get(t, h, http.MethodGet, "/assets/app-1a2b.js").Header().Get("Cache-Control"); !strings.Contains(c, "immutable") {
		t.Errorf("a hashed asset's Cache-Control = %q; want it cached for good", c)
	}
	if rec := get(t, h, http.MethodPost, "/"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d; want 405", rec.Code)
	}
}

func TestNotBuilt(t *testing.T) {
	empty := fstest.MapFS{".gitkeep": {}}
	if webui.Built(empty) || !webui.Built(dashboard) {
		t.Fatal("Built is wrong")
	}
	if rec := get(t, webui.Handler(empty), http.MethodGet, "/"); rec.Code != http.StatusNotFound ||
		!strings.Contains(rec.Body.String(), "make build") {
		t.Errorf("without a dashboard: %d %q", rec.Code, rec.Body.String())
	}
}
