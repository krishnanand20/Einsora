//go:build darwin

package macosnative

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krishnanand20/einsora/controlplane/internal/agent/protocol"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/redact"
)

// TestRealLlamaEndpoint builds the real llama.cpp runtime (uv environment and the pinned release,
// checked by SHA-256), fetches a GGUF model from a local S3-compatible fixture, validates it, and
// serves completions on Metal and then on the CPU. It downloads about 50 MB the first time and
// changes no machine settings. Give it a small GGUF model, such as ggml-org/models'
// tinyllamas/stories260K.gguf:
//
//	EINSORA_TEST_LLAMA_MODEL=/abs/path/model.gguf go test -run TestRealLlamaEndpoint -v ./internal/agent/drivers/macosnative/
func TestRealLlamaEndpoint(t *testing.T) {
	model := os.Getenv("EINSORA_TEST_LLAMA_MODEL")
	if model == "" {
		t.Skip("set EINSORA_TEST_LLAMA_MODEL to a small GGUF model file")
	}
	if !filepath.IsAbs(model) {
		t.Fatal("EINSORA_TEST_LLAMA_MODEL must be absolute")
	}
	data, err := os.ReadFile(model) //nolint:gosec // the tester's own file
	if err != nil {
		t.Fatal(err)
	}
	uv, err := exec.LookPath("uv")
	if err != nil {
		t.Skip("needs uv")
	}
	name := filepath.Base(model)
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("list-type") == "2" {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = fmt.Fprintf(w, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>models</Name><Prefix>gguf/</Prefix>`+
				`<IsTruncated>false</IsTruncated><Contents><Key>gguf/%s</Key><Size>%d</Size></Contents></ListBucketResult>`, name, len(data))
			return
		}
		if strings.TrimPrefix(r.URL.Path, "/models/gguf/") == name {
			http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
			return
		}
		http.NotFound(w, r)
	}))
	defer s3.Close()
	t.Setenv("AWS_ENDPOINT_URL_S3", s3.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "einsora-local-test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "not-a-real-credential")
	t.Setenv("AWS_DEFAULT_REGION", "us-east-1")

	// One driver directory for both runs, so the environment is built once.
	state := t.TempDir()
	runs, err := os.MkdirTemp("/tmp", "tr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runs) })
	d, err := New(Options{Root: filepath.Join(state, "native"), State: state, Runs: runs, UV: uv, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	for _, accelerator := range []domain.AcceleratorKind{domain.AcceleratorApple, domain.AcceleratorCPU} {
		w := llamaWorkload(accelerator)
		w.ModelURI = "s3://models/gguf"
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		if err := d.Run(ctx, w); err != nil {
			cancel()
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = d.Remove(context.Background(), w.ID) })
		t.Cleanup(func() {
			if t.Failed() {
				for _, log := range []string{"stdout.log", "stderr.log"} {
					if output, err := os.ReadFile(filepath.Join(d.dir(w.ID), "logs", log)); err == nil {
						t.Logf("%s %s:\n%s", accelerator, log, redact.Secrets(tail(output)))
					}
				}
			}
		})
		var access *protocol.Access
		for access == nil && ctx.Err() == nil {
			sts, err := d.Statuses(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range sts {
				switch {
				case s.ID != w.ID:
				case s.State == protocol.StateReady:
					access = s.Access
				case s.State == protocol.StateFailed:
					t.Fatalf("%s: failed: %s", accelerator, protocol.FailureReason(s.Code))
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
		if access == nil {
			t.Fatalf("%s: never ready", accelerator)
		}
		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", access.Socket)
		}}
		client := &http.Client{Transport: transport, Timeout: time.Minute}
		call := func(method, path, body, token string) (int, string) {
			req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			out, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return resp.StatusCode, string(out)
		}
		if code, _ := call(http.MethodPost, "/v1/completions", `{"prompt":"Once upon a time","max_tokens":8}`, ""); code != http.StatusUnauthorized {
			t.Errorf("%s: a request without the server's token got HTTP %d", accelerator, code)
		}
		if code, out := call(http.MethodGet, "/v1/models", "", access.Token); code != http.StatusOK || !strings.Contains(out, `"s3://models/gguf"`) {
			t.Errorf("%s: models: HTTP %d, %s", accelerator, code, out)
		}
		for _, stream := range []bool{false, true} {
			body := fmt.Sprintf(`{"model":"s3://models/gguf","prompt":"Once upon a time","max_tokens":8,"stream":%t}`, stream)
			code, out := call(http.MethodPost, "/v1/completions", body, access.Token)
			if code != http.StatusOK || !strings.Contains(out, `"text"`) || stream && !strings.Contains(out, "data: [DONE]") {
				t.Errorf("%s: completion (stream=%t): HTTP %d, %s", accelerator, stream, code, out)
			}
		}
		if code, _ := call(http.MethodGet, "/slots", "", access.Token); code == http.StatusOK {
			t.Errorf("%s: the slots endpoint is on", accelerator)
		}
		transport.CloseIdleConnections()
		cancel()
		if err := d.Stop(context.Background(), w.ID); err != nil {
			t.Fatal(err)
		}
		status(t, d, w.ID, func(s protocol.Status) bool { return s.State == protocol.StateStopped })
		t.Logf("llama.cpp endpoint on %s: S3 fetch, GGUF policy, completions and SSE passed", accelerator)
	}
}
