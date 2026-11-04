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
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/api"
	"github.com/krishnanand20/einsora/controlplane/internal/auth"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/service"
	"github.com/krishnanand20/einsora/controlplane/internal/store"
	"github.com/krishnanand20/einsora/controlplane/internal/testutil"
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
	if _, err := parseWithID(fs, nil, "usage"); err == nil || !strings.Contains(err.Error(), "missing workload name or ID") {
		t.Errorf("missing ID: %v", err)
	}
}

func TestCLIEventsAndExport(t *testing.T) {
	st := testutil.SeededStore(t)
	svc := service.New(st, testutil.Catalog(t), nil)
	srv := httptest.NewServer(api.New(svc, auth.Dev{}, slog.New(slog.DiscardHandler), st.Ping).Handler())
	t.Cleanup(srv.Close)
	t.Setenv("EINSORA_API_URL", srv.URL)
	t.Setenv("EINSORA_DEV_TENANT", "acme")

	ctx := context.Background()
	w, err := svc.CreateWorkload(ctx, testutil.Acme, service.CreateWorkloadInput{
		Name: "nb", Kind: domain.KindNotebook, ImageID: "pytorch-notebook", GPUClass: "l40s-48gb", GPUCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runClient(ctx, "events", []string{w.ID.String()}, cli{out: &out}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "∅ → pending") || !strings.Contains(out.String(), "created by") {
		t.Errorf("events output:\n%s", out.String())
	}

	path := filepath.Join(t.TempDir(), "export.json")
	out.Reset()
	if err := runClient(ctx, "export", []string{w.ID.String(), "-o", path}, cli{out: &out}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(data, []byte(`"format":"einsora.workload-export/v1"`)) {
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
	err = runClient(ctx, "export", []string{w.ID.String(), "-o", path}, cli{out: &out})
	if data, _ := os.ReadFile(path); err == nil || !strings.Contains(err.Error(), "already exists") || string(data) != "keep me" {
		t.Fatalf("export over an existing file without -f: err = %v, file = %q", err, data)
	}
	if err := runClient(ctx, "export", []string{w.ID.String(), "-o", path, "-f"}, cli{out: &out}); err != nil {
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

// einsora machines lists the caller's own machines through the real API, and only theirs.
func TestCLIMachines(t *testing.T) {
	st := testutil.SeededStore(t)
	svc := service.New(st, testutil.Catalog(t), nil)
	srv := httptest.NewServer(api.New(svc, auth.Dev{}, slog.New(slog.DiscardHandler), st.Ping).Handler())
	t.Cleanup(srv.Close)
	t.Setenv("EINSORA_API_URL", srv.URL)
	ctx := context.Background()
	err := st.InTx(ctx, func(q store.Queries) error {
		tn, err := q.TenantBySlug(ctx, "acme")
		if err != nil {
			return err
		}
		return q.CreateMachine(ctx, domain.Machine{ID: uuid.New(), TenantID: tn.ID, Name: "studio", Region: domain.RegionLocal,
			OS: "darwin", Arch: "arm64", CPUs: 12, MemoryMiB: 36 * 1024, Sharing: domain.SharingPrivate,
			Accelerators: []domain.Accelerator{{Kind: domain.AcceleratorApple, Model: "Apple M3 Pro", Count: 1, MemoryMiB: 36 * 1024}},
			LastSeenAt:   time.Now(), CreatedAt: testutil.T0})
	})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	t.Setenv("EINSORA_DEV_TENANT", "acme")
	if err := runClient(ctx, "machines", nil, cli{out: &out}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"studio", "online", "darwin/arm64", "36 GiB", "1 × Apple M3 Pro"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output is missing %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	t.Setenv("EINSORA_DEV_TENANT", "globex")
	if err := runClient(ctx, "machines", nil, cli{out: &out}); err != nil || strings.Contains(out.String(), "studio") {
		t.Errorf("another tenant sees: %q, %v", out.String(), err)
	}
}
