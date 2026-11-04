package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/api"
	"github.com/krishnanand20/einsora/controlplane/internal/auth"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/output"
	"github.com/krishnanand20/einsora/controlplane/internal/service"
	"github.com/krishnanand20/einsora/controlplane/internal/store"
	"github.com/krishnanand20/einsora/controlplane/internal/testutil"
)

// lockedBuffer is a bytes.Buffer safe to read while a command writes to it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// placedNotebook is tenant internal's notebook "nb", placed on its machine with the sim driver,
// behind the real API, and a function that stores output for it as its agent would.
func placedNotebook(t *testing.T) func(stream domain.OutputStream, offset int64, text string) {
	t.Helper()
	st := testutil.SeededStore(t)
	svc := service.New(st, testutil.Catalog(t), nil)
	srv := httptest.NewServer(api.New(svc, auth.Dev{}, slog.New(slog.DiscardHandler), st.Ping).Handler())
	t.Cleanup(srv.Close)
	t.Setenv("EINSORA_API_URL", srv.URL)
	t.Setenv("EINSORA_DEV_TENANT", "internal")
	ctx := context.Background()
	w, err := svc.CreateWorkload(ctx, testutil.Internal, service.CreateWorkloadInput{
		Name: "nb", Kind: domain.KindNotebook, ImageID: "pytorch-notebook", GPUClass: "cpu-2c-8g", GPUCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = st.InTx(ctx, func(q store.Queries) error {
		tn, err := q.TenantBySlug(ctx, "internal")
		if err != nil {
			return err
		}
		m := domain.Machine{ID: uuid.New(), TenantID: tn.ID, Name: "studio", Region: domain.RegionLocal, OS: "darwin", Arch: "arm64",
			CPUs: 8, MemoryMiB: 16384, Sharing: domain.SharingPrivate, CreatedAt: testutil.T0}
		if err := q.CreateMachine(ctx, m); err != nil {
			return err
		}
		return q.SetPlacement(ctx, w.ID, "cpu-2c-8g", m.ID, domain.DriverSim)
	})
	if err != nil {
		t.Fatal(err)
	}
	w.Attempt = 1
	return func(stream domain.OutputStream, offset int64, text string) {
		t.Helper()
		err := st.InTx(ctx, func(q store.Queries) error {
			_, err := output.Append(ctx, q, w, output.Piece{Stream: stream, Source: "s", Offset: offset, Data: []byte(text)}, 1<<20, time.Now())
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestCLILogs(t *testing.T) {
	write := placedNotebook(t)
	const stdout = "\x1b[32mready\x1b[0m\r\n\x1b]0;title\x07epoch 1\repoch 2\n"
	write(domain.StreamStdout, 0, stdout)
	write(domain.StreamStderr, 10, "late\n") // 10 bytes of stderr never arrived

	var out, notes bytes.Buffer
	if err := runClient(context.Background(), "logs", []string{"nb"}, cli{out: &out, errOut: &notes}); err != nil {
		t.Fatal(err)
	}
	want := "ready\nepoch 1\nepoch 2\n[einsora] 10 bytes of stderr was lost here: it was gone from the machine before Einsora received it\nlate\n"
	if out.String() != want {
		t.Errorf("output %q,\nwant %q", out.String(), want)
	}
	if !strings.Contains(notes.String(), "einsora: 10 bytes of output was lost") {
		t.Errorf("notes: %q", notes.String())
	}

	out.Reset()
	if err := runClient(context.Background(), "logs", []string{"nb", "-json"}, cli{out: &out, errOut: &notes}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var first map[string]any
	if len(lines) != 3 || json.Unmarshal([]byte(lines[0]), &first) != nil || first["stream"] != "stdout" || !strings.Contains(first["text"].(string), "\x1b[32m") {
		t.Errorf("-json lines: %q", out.String())
	}

	// -f keeps printing new output until interrupted.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var followed lockedBuffer
	done := make(chan error, 1)
	go func() { done <- runClient(ctx, "logs", []string{"nb", "-f"}, cli{out: &followed, errOut: &notes}) }()
	time.Sleep(200 * time.Millisecond)
	write(domain.StreamStdout, int64(len(stdout)), "more\n")
	deadline := time.Now().Add(5 * time.Second)
	for !strings.HasSuffix(followed.String(), "late\nmore\n") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil || !strings.HasSuffix(followed.String(), "late\nmore\n") {
		t.Errorf("-f: %v, printed %q", err, followed.String())
	}
}

func TestTerminalText(t *testing.T) {
	for in, want := range map[string]string{
		"plain\ttext\n":                 "plain\ttext\n",
		"\x1b[1;31mred\x1b[0m":          "red",
		"\x1b]52;c;ZXZpbA==\x07copied?": "copied?",
		"\x1b]8;;http://x\x1b\\link":    "link",
		"bell\x07 and \u009b C1":        "bell and  C1",
		"10%\r50%\r100%\r\n":            "10%\n50%\n100%\n",
	} {
		if got := terminalText(in); got != want {
			t.Errorf("terminalText(%q) = %q, want %q", in, got, want)
		}
	}
}
