//go:build linux

package docker

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tensora/tensora/controlplane/internal/agent/protocol"
)

// Opt-in smoke test for an already configured local test daemon and a pinned JupyterLab image.
// It starts and deletes only its own workload; it never configures Docker or the host.
func TestRealDockerNotebook(t *testing.T) {
	image := os.Getenv("TENSORA_TEST_DOCKER_NOTEBOOK_IMAGE")
	if image == "" {
		t.Skip("set TENSORA_TEST_DOCKER_NOTEBOOK_IMAGE to a pinned JupyterLab image on Linux")
	}
	runs, err := os.MkdirTemp("/tmp", "td-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runs) })
	d, err := New(Options{Root: filepath.Join(t.TempDir(), "state"), Runs: runs, Socket: os.Getenv("TENSORA_DOCKER_SOCKET")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	w := desired()
	w.Runtime.Ref = image
	wait := func(want protocol.State) protocol.Status {
		t.Helper()
		for ctx.Err() == nil {
			all, err := d.Statuses(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range all {
				if s.ID == w.ID {
					if s.State == want {
						return s
					}
					if s.State == protocol.StateFailed {
						t.Fatalf("workload failed: %s", s.Code)
					}
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("timed out waiting for Docker workload")
		return protocol.Status{}
	}
	defer func() { _ = d.Remove(context.Background(), w.ID); awaitRemoved(t, d, w.ID) }()
	if err := d.Run(ctx, w); err != nil {
		t.Fatal(err)
	}
	ready := wait(protocol.StateReady)
	call := func(method, body string, s protocol.Status) string {
		t.Helper()
		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", s.Access.Socket)
		}}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		req, err := http.NewRequestWithContext(ctx, method, "http://localhost/api/contents/saved.txt", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "token "+s.Access.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			t.Fatalf("Jupyter HTTP %d", resp.StatusCode)
		}
		return string(out)
	}
	call(http.MethodPut, `{"type":"file","format":"text","content":"saved through Jupyter"}`, ready)
	if err := d.Stop(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	wait(protocol.StateStopped)
	w.Attempt++
	if err := d.Run(ctx, w); err != nil {
		t.Fatal(err)
	}
	ready = wait(protocol.StateReady)
	if !strings.Contains(call(http.MethodGet, "", ready), "saved through Jupyter") {
		t.Fatal("stopping and starting lost notebook data")
	}
}
