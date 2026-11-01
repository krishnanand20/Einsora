package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/krishnanand20/einsora/controlplane/internal/catalog"
	"github.com/krishnanand20/einsora/controlplane/internal/store/memstore"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestDataDir(t *testing.T) {
	home := func() (string, error) { return "/Users/o", nil }
	tests := []struct {
		name, goos string
		vars       map[string]string
		want       string
	}{
		{"macOS", "darwin", nil, "/Users/o/Library/Application Support/Einsora"},
		{"Linux", "linux", nil, "/Users/o/.local/share/einsora"},
		{"Linux with XDG_DATA_HOME", "linux", map[string]string{"XDG_DATA_HOME": "/data/xdg"}, "/data/xdg/einsora"},
		{"a relative XDG_DATA_HOME is ignored", "linux", map[string]string{"XDG_DATA_HOME": "xdg"}, "/Users/o/.local/share/einsora"},
		{"EINSORA_DATA_DIR wins", "darwin", map[string]string{"EINSORA_DATA_DIR": "/srv/einsora/"}, "/srv/einsora"},
	}
	for _, tt := range tests {
		got, err := dataDir(tt.goos, env(tt.vars), home)
		if err != nil || got != tt.want {
			t.Errorf("%s: %q, %v; want %q", tt.name, got, err, tt.want)
		}
	}
	if _, err := dataDir("linux", env(map[string]string{"EINSORA_DATA_DIR": "einsora"}), home); err == nil {
		t.Error("a relative EINSORA_DATA_DIR was accepted")
	}
	if _, err := dataDir("linux", env(nil), func() (string, error) { return "", errors.New("no home") }); err == nil {
		t.Error("no home directory and no EINSORA_DATA_DIR was accepted")
	}
}

// Solo's defaults apply to whatever EINSORA_* doesn't set, and anything set wins.
func TestSoloConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "einsora")
	cfg, got, err := soloConfig(env(map[string]string{"EINSORA_DATA_DIR": dir, "EINSORA_HTTP_ADDR": "127.0.0.1:9999"}))
	if err != nil {
		t.Fatal(err)
	}
	if got != dir || cfg.Env != "production" || cfg.AuthMode != "local" || cfg.ClusterBackend != "agent" ||
		cfg.DatabaseURL != "sqlite:"+filepath.Join(dir, "einsora.db") || cfg.LogFile != filepath.Join(dir, "einsora.log") ||
		cfg.HTTPAddr != "127.0.0.1:9999" || cfg.CatalogPath != "" {
		t.Errorf("config = %+v (dir %q)", cfg, got)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("data directory: %v, %v; want it made, owner-only", fi, err)
	}
}

func TestDataDirMustBePrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { // past the umask
		t.Fatal(err)
	}
	if err := privateDir(dir); err == nil || !strings.Contains(err.Error(), "chmod 700") {
		t.Errorf("a directory others can read: %v; want it refused, with the fix", err)
	}
}

// The dashboard and the API share one origin: the API's paths go to the API.
func TestWithDashboard(t *testing.T) {
	mark := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(name)) })
	}
	h := withDashboard(mark("api"), mark("dashboard"))
	for path, want := range map[string]string{
		"/v1/workloads": "api", "/v1": "api", "/healthz": "api", "/readyz": "api",
		"/": "dashboard", "/workloads/0b1c": "dashboard", "/assets/app.js": "dashboard", "/v1x": "dashboard",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Body.String() != want {
			t.Errorf("%s went to %s; want %s", path, rec.Body.String(), want)
		}
	}
}

func TestListenSaysWhenThePortIsTaken(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if _, err := listen(l.Addr().String()); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("a taken port: %v; want a clear error", err)
	}
}

// Without a terminal, the first run says how to make the owner's account, and goes on.
func TestFirstRunWithoutATerminal(t *testing.T) {
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := firstRun(context.Background(), memstore.New(), cat, "home", strings.NewReader(""), &out, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "einsora setup -email") {
		t.Errorf("output = %q; want how to create the owner", out.String())
	}
}
