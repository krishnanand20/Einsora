//go:build darwin

package macosnative

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tensora/tensora/controlplane/internal/redact"
)

// TestRealEndpoint covers S3 download, safetensors validation, MLX Metal loading, completions,
// streaming, and cleanup using a tiny random model. It needs the optional serve environment but
// makes no external model requests and doesn't change machine settings.
func TestRealEndpoint(t *testing.T) {
	realEnv := os.Getenv("TENSORA_TEST_NATIVE_ENDPOINT_ENV")
	if realEnv == "" {
		t.Skip("set TENSORA_TEST_NATIVE_ENDPOINT_ENV to a Python environment with the serve extra")
	}
	if !filepath.IsAbs(realEnv) {
		t.Fatal("TENSORA_TEST_NATIVE_ENDPOINT_ENV must be absolute")
	}
	python := filepath.Join(realEnv, "bin", "python")
	fixture := t.TempDir()
	script := filepath.Join("..", "..", "..", "..", "..", "ml", "tests", "tiny_mlx_model.py")
	cmd := exec.Command(python, script, fixture)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create tiny model: %v: %s", err, output)
	}
	entries, err := os.ReadDir(fixture)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, entry := range entries {
		files[entry.Name()], err = os.ReadFile(filepath.Join(fixture, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
	}
	// A local S3-compatible read-only fixture exercises the real download path.
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("list-type") == "2" {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = io.WriteString(w, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>models</Name><Prefix>tiny/</Prefix><IsTruncated>false</IsTruncated>`)
			for name, data := range files {
				_, _ = io.WriteString(w, "<Contents><Key>tiny/")
				_ = xml.EscapeText(w, []byte(name))
				_, _ = fmt.Fprintf(w, "</Key><Size>%d</Size></Contents>", len(data))
			}
			_, _ = io.WriteString(w, "</ListBucketResult>")
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/models/tiny/")
		if data, ok := files[name]; ok {
			http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
			return
		}
		http.NotFound(w, r)
	}))
	defer s3.Close()
	t.Setenv("AWS_ENDPOINT_URL_S3", s3.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "tensora-local-test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "not-a-real-credential")
	t.Setenv("AWS_DEFAULT_REGION", "us-east-1")
	d, _ := newDriver(t)
	endpointEnvironment(t, d, fmt.Sprintf("#!/bin/sh\nexec %q \"$@\"\n", python))
	w := endpointWorkload()
	w.ModelURI = "s3://models/tiny"
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := d.Run(ctx, w); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Remove(context.Background(), w.ID) })
	t.Cleanup(func() {
		if t.Failed() {
			if output, err := os.ReadFile(filepath.Join(d.dir(w.ID), "logs", "stderr.log")); err == nil {
				t.Log(redact.Secrets(string(output)))
			}
		}
	})
	ready := status(t, d, w.ID, ready)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", ready.Access.Socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second}
	for _, stream := range []bool{false, true} {
		body := fmt.Sprintf(`{"model":"s3://models/tiny","messages":[{"role":"user","content":"hello"}],"max_tokens":4,"stream":%t,"stream_options":{"include_usage":true}}`, stream)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/v1/chat/completions", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+ready.Access.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		output, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || !bytes.Contains(output, []byte("completion_tokens")) {
			t.Fatalf("real completion (stream=%t): HTTP %d, %q, %v", stream, resp.StatusCode, output, err)
		}
		if stream && !bytes.Contains(output, []byte("data: [DONE]")) {
			t.Fatal("stream didn't finish with [DONE]")
		}
	}
	t.Log("native endpoint: S3 fetch, safetensors policy, MLX Metal, chat completion and SSE passed")
}
