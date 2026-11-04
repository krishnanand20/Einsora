//go:build darwin

package macosnative

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/agent"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
)

var _ agent.OutputReader = (*Driver)(nil)

// ReadOutput reads a workload's log files from an offset, names each file by its inode, and never
// follows a symlink.
func TestReadOutput(t *testing.T) {
	d, _ := newDriver(t)
	ctx := context.Background()
	id := uuid.New()
	if _, _, _, err := d.ReadOutput(ctx, id, domain.StreamStdout, 0, 10); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("before it ran: %v, want not exist", err)
	}
	logs := filepath.Join(d.dir(id), "logs")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logs, "stdout.log"), []byte("hello\nworld\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, source, size, err := d.ReadOutput(ctx, id, domain.StreamStdout, 6, 3)
	if err != nil || string(data) != "wor" || size != 12 || source == "" {
		t.Fatalf("ReadOutput = %q, %q, %d, %v", data, source, size, err)
	}
	if data, again, _, _ := d.ReadOutput(ctx, id, domain.StreamStdout, 12, 3); len(data) != 0 || again != source {
		t.Errorf("at the end: %q, source %q; want nothing, from the same source", data, again)
	}

	// A replaced file is a new source.
	if err := os.Remove(filepath.Join(logs, "stdout.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logs, "stdout.log"), []byte("again\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, replaced, _, err := d.ReadOutput(ctx, id, domain.StreamStdout, 0, 10); err != nil || replaced == source {
		t.Errorf("a replaced file's source = %q (was %q), %v", replaced, source, err)
	}

	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("not output"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(logs, "stderr.log")); err != nil {
		t.Fatal(err)
	}
	if data, _, _, err := d.ReadOutput(ctx, id, domain.StreamStderr, 0, 100); err == nil {
		t.Errorf("followed a symlink: %q", data)
	}
	if _, _, _, err := d.ReadOutput(ctx, id, domain.StreamEinsora, 0, 100); err == nil {
		t.Error("read a stream drivers don't have")
	}
}
