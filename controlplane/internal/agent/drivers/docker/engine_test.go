package docker

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestLocalEngineDoesNotFollowRedirectsOrUseRemoteHosts(t *testing.T) {
	var contacted atomic.Bool
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { contacted.Store(true) }))
	defer remote.Close()
	dir, err := os.MkdirTemp("/tmp", "tde")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "docker.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	local := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, remote.URL, http.StatusTemporaryRedirect)
	}))
	local.Listener = listener
	local.Start()
	defer local.Close()
	e := localEngine(path)
	defer e.close()
	if _, err := e.detect(context.Background()); err == nil {
		t.Error("accepted redirected Engine API response")
	}
	if contacted.Load() {
		t.Error("Docker API followed a redirect away from its Unix socket")
	}
}
