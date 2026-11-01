package main

import (
	"bytes"
	"context"
	"flag"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tensora/tensora/controlplane/internal/api"
	"github.com/tensora/tensora/controlplane/internal/auth"
	"github.com/tensora/tensora/controlplane/internal/domain"
	"github.com/tensora/tensora/controlplane/internal/service"
	"github.com/tensora/tensora/controlplane/internal/testutil"
)

func TestParseWithIDAcceptsFlagsEitherSide(t *testing.T) {
	for _, args := range [][]string{{"abc", "-o", "out.json"}, {"-o", "out.json", "abc"}} {
		fs := flag.NewFlagSet("export", flag.ContinueOnError)
		out := fs.String("o", "", "")
		id, err := parseWithID(fs, args, "usage")
		if err != nil || id != "abc" || *out != "out.json" {
			t.Errorf("%v → id %q, o %q, err %v", args, id, *out, err)
		}
	}
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	if _, err := parseWithID(fs, nil, "usage"); err == nil || !strings.Contains(err.Error(), "missing workload ID") {
		t.Errorf("missing ID: %v", err)
	}
}

func TestCLIEventsAndExport(t *testing.T) {
	st := testutil.SeededStore(t)
	svc := service.New(st, testutil.Catalog(t), nil)
	srv := httptest.NewServer(api.New(svc, auth.Dev{}, slog.New(slog.DiscardHandler), st.Ping).Handler())
	t.Cleanup(srv.Close)
	t.Setenv("TENSORA_API_URL", srv.URL)
	t.Setenv("TENSORA_DEV_TENANT", "acme")

	ctx := context.Background()
	w, err := svc.CreateWorkload(ctx, testutil.Acme, service.CreateWorkloadInput{
		Name: "nb", Kind: domain.KindNotebook, ImageID: "pytorch-notebook", GPUClass: "l40s-48gb", GPUCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runClient(ctx, "events", []string{w.ID.String()}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "∅ → pending") || !strings.Contains(out.String(), "created by") {
		t.Errorf("events output:\n%s", out.String())
	}

	path := filepath.Join(t.TempDir(), "export.json")
	out.Reset()
	if err := runClient(ctx, "export", []string{w.ID.String(), "-o", path}, &out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(data, []byte(`"format":"tensora.workload-export/v1"`)) {
		t.Fatalf("export file: %v %s", err, data)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("export file mode = %v; exports are private (0600)", info.Mode().Perm())
	}

	// An existing file is never replaced without -f; with -f the result is a fresh 0600 file.
	if err := os.WriteFile(path, []byte("keep me"), 0o644); err != nil { //nolint:gosec // the test needs a too-open file
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	err = runClient(ctx, "export", []string{w.ID.String(), "-o", path}, &out)
	if data, _ := os.ReadFile(path); err == nil || !strings.Contains(err.Error(), "already exists") || string(data) != "keep me" {
		t.Fatalf("export over an existing file without -f: err = %v, file = %q", err, data)
	}
	if err := runClient(ctx, "export", []string{w.ID.String(), "-o", path, "-f"}, &out); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("replaced export mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestPrintableStripsTerminalEscapes(t *testing.T) {
	if got := printable("ready\x1b]52;c;ZXZpbA==\x07"); strings.ContainsAny(got, "\x1b\x07") {
		t.Errorf("printable kept control characters: %q", got)
	}
}
