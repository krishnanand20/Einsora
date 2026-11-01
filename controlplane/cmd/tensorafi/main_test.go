package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/tensora/tensora/controlplane/internal/config"
	"github.com/tensora/tensora/controlplane/internal/domain"
	"github.com/tensora/tensora/controlplane/internal/store/memstore"
	"github.com/tensora/tensora/controlplane/internal/testutil"
)

func TestLogFileGetsACompleteCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tensora.log")
	log, closeLog, err := newLogger(config.Config{Env: "production", LogLevel: slog.LevelDebug, LogFile: path})
	if err != nil {
		t.Fatal(err)
	}
	log.Debug("admission check", "workload", "w1")
	log.Info("workload transitioned", "to", "running")
	closeLog()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"msg":"admission check"`, `"msg":"workload transitioned"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("log file is missing %s:\n%s", want, data)
		}
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("log file mode = %v; logs are readable by their owner only", info.Mode().Perm())
	}
}

func TestLogFileMustBePrivateAndSymlinksAreRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	// A file others can read is refused, with the fix, rather than silently chmodded.
	if _, _, err := newLogger(config.Config{Env: "production", LogFile: path}); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("world-readable log file: err = %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	_, closeLog, err := newLogger(config.Config{Env: "production", LogFile: path})
	if err != nil {
		t.Fatal(err)
	}
	closeLog()

	link := filepath.Join(dir, "link.log")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := newLogger(config.Config{Env: "production", LogFile: link}); err == nil {
		t.Error("a symlinked TENSORA_LOG_FILE must be refused")
	}
}

// A controller for a region the catalog doesn't offer would leave every workload pending.
func TestControllerRegionMustBeInTheCatalog(t *testing.T) {
	cfg := config.Config{Region: "eu-fra-1", ClusterBackend: "fake"}
	_, err := newController(cfg, slog.New(slog.DiscardHandler), memstore.New(), testutil.Catalog(t))
	if err == nil || !strings.Contains(err.Error(), "isn't in the catalog's regions") {
		t.Fatalf("err = %v", err)
	}
	cfg.Region = "local"
	if _, err := newController(cfg, slog.New(slog.DiscardHandler), memstore.New(), testutil.Catalog(t)); err != nil {
		t.Fatal(err)
	}
}

// tensora setup creates the owner once, on the store itself; a second run is refused.
func TestSetupCommand(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TENSORA_DATABASE_URL", "sqlite:"+filepath.Join(dir, "tensora.db"))
	t.Setenv("TENSORA_CATALOG_PATH", testutilCatalogPath(t))
	t.Setenv("TENSORA_AUTH_MODE", "local")
	ctx := context.Background()

	var out bytes.Buffer
	in := strings.NewReader("correct horse battery staple\n")
	if err := runSetup(ctx, []string{"-email", "me@example.com", "-password-stdin"}, in, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Created the owner account me@example.com") || strings.Contains(out.String(), "staple") {
		t.Errorf("output = %q", out.String())
	}
	err := runSetup(ctx, []string{"-email", "other@example.com", "-password-stdin"}, strings.NewReader("another long passphrase\n"), &out)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second setup: %v; want it refused", err)
	}
	if err := runSetup(ctx, []string{"-email", "x@example.com"}, strings.NewReader(""), &out); err == nil {
		t.Error("without a terminal or -password-stdin there's no safe way to read a password")
	}

	// The way back in for a forgotten password.
	out.Reset()
	if err := runSetup(ctx, []string{"-reset-password", "-email", "Me@Example.com", "-password-stdin"},
		strings.NewReader("a brand new passphrase\n"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Set a new password for me@example.com") || strings.Contains(out.String(), "passphrase") {
		t.Errorf("output = %q", out.String())
	}
	err = runSetup(ctx, []string{"-reset-password", "-email", "nobody@example.com", "-password-stdin"},
		strings.NewReader("a brand new passphrase\n"), &out)
	if err == nil {
		t.Error("reset for an unknown email succeeded")
	}
}

func testutilCatalogPath(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		p := filepath.Join(dir, "images", "catalog.yaml")
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("images/catalog.yaml not found")
		}
		dir = parent
	}
}

func TestMachineNameIsASlug(t *testing.T) {
	name := machineName()
	if !domain.ValidSlug(name) {
		t.Errorf("machineName() = %q; want a slug", name)
	}
}

// This machine's ID is kept next to the SQLite database, written once, owner-only.
func TestMachineIDFile(t *testing.T) {
	dir := t.TempDir()
	path, err := machineIDFile(config.Config{DatabaseURL: "sqlite:" + filepath.Join(dir, "tensora.db")})
	if err != nil || path != filepath.Join(dir, "machine-id") {
		t.Fatalf("machineIDFile = %q, %v", path, err)
	}
	if id, err := readMachineID(path); err != nil || id != uuid.Nil {
		t.Fatalf("before the first start: %v, %v", id, err)
	}
	id := uuid.New()
	if err := writeMachineID(path, id); err != nil {
		t.Fatal(err)
	}
	if got, err := readMachineID(path); err != nil || got != id {
		t.Errorf("read back %v, %v", got, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v; want 0600", fi.Mode().Perm())
	}
	if err := writeMachineID(path, uuid.New()); err == nil {
		t.Error("an existing machine ID was replaced")
	}
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readMachineID(path); err == nil {
		t.Error("a garbled machine ID was accepted")
	}
	if path, err := machineIDFile(config.Config{}); err != nil || path != "" {
		t.Errorf("in-memory store: %q, %v; want nothing kept", path, err)
	}
}
