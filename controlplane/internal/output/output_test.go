package output_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/output"
	"github.com/krishnanand20/einsora/controlplane/internal/store"
	"github.com/krishnanand20/einsora/controlplane/internal/store/memstore"
	"github.com/krishnanand20/einsora/controlplane/internal/testutil"
)

// fakeToken stands in for a notebook server's token. It's made at run time, so secret scanners
// don't mistake the test for a real one.
var fakeToken = strings.Repeat("f", 16)

type fixture struct {
	t  *testing.T
	st store.Store
	w  domain.Workload
}

func newFixture(t *testing.T, region domain.Region) *fixture {
	t.Helper()
	st := memstore.New()
	ctx := context.Background()
	tn := domain.Tenant{ID: uuid.New(), Slug: "alpha", Name: "alpha", Tier: domain.TierTrusted, CreatedAt: testutil.T0}
	w := domain.Workload{ID: uuid.New(), TenantID: tn.ID, Name: "nb", Kind: domain.KindNotebook, State: domain.StateRunning,
		ImageID: "img", GPUClass: "cpu-2c-8g", PlacedClass: "cpu-2c-8g", GPUCount: 1, Params: map[string]string{},
		CreatedBy: "test", CreatedAt: testutil.T0, UpdatedAt: testutil.T0, Region: region}
	err := st.InTx(ctx, func(q store.Queries) error {
		if err := q.CreateTenant(ctx, tn); err != nil {
			return err
		}
		return q.CreateWorkload(ctx, w)
	})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, st: st, w: w}
}

func (f *fixture) append(p output.Piece, maxBytes int64, at time.Time) int64 {
	f.t.Helper()
	var next int64
	err := f.st.InTx(context.Background(), func(q store.Queries) error {
		var err error
		next, err = output.Append(context.Background(), q, f.w, p, maxBytes, at)
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return next
}

func (f *fixture) chunks() []domain.OutputChunk {
	f.t.Helper()
	var out []domain.OutputChunk
	_ = f.st.InTx(context.Background(), func(q store.Queries) error {
		var err error
		out, err = q.OutputForWorkload(context.Background(), f.w.TenantID, f.w.ID, 0, 1000)
		return err
	})
	return out
}

func (f *fixture) state(s domain.OutputStream) domain.OutputState {
	f.t.Helper()
	var out domain.OutputState
	_ = f.st.InTx(context.Background(), func(q store.Queries) error {
		var err error
		out, err = q.OutputState(context.Background(), f.w.ID, s)
		return err
	})
	return out
}

func texts(cs []domain.OutputChunk) string {
	var b strings.Builder
	for _, c := range cs {
		b.WriteString(string(c.Stream) + ":" + c.Text)
	}
	return b.String()
}

const mib = 1 << 20

// What's already stored is skipped, so an agent can resend after a restart without duplicates.
func TestAppendSkipsWhatIsStored(t *testing.T) {
	f := newFixture(t, domain.RegionLocal)
	out := domain.StreamStdout
	if next := f.append(output.Piece{Stream: out, Source: "s1"}, mib, testutil.T0); next != 0 {
		t.Fatalf("a first probe = %d, want 0", next)
	}
	if next := f.append(output.Piece{Stream: out, Source: "s1", Data: []byte("a\n")}, mib, testutil.T0); next != 2 {
		t.Fatalf("next = %d, want 2", next)
	}
	if next := f.append(output.Piece{Stream: out, Source: "s1", Data: []byte("a\nb\n")}, mib, testutil.T0); next != 4 {
		t.Fatalf("next after an overlapping resend = %d, want 4", next)
	}
	if next := f.append(output.Piece{Stream: out, Source: "s1", Offset: 2, Data: []byte("b\n")}, mib, testutil.T0); next != 4 {
		t.Fatalf("next after a stored resend = %d, want 4", next)
	}
	if got := texts(f.chunks()); got != "stdout:a\nstdout:b\n" {
		t.Errorf("stored %q", got)
	}
	if st := f.state(out); st.StoredBytes != 4 || st.Next != 4 || st.LostBytes != 0 {
		t.Errorf("state = %+v", st)
	}
}

// A gap is counted as lost, and a note says where; a new source or a restart starts over at 0,
// with a note saying so.
func TestAppendGapsAndRestarts(t *testing.T) {
	f := newFixture(t, domain.RegionLocal)
	errs := domain.StreamStderr
	f.append(output.Piece{Stream: errs, Source: "s1", Data: []byte("one\n")}, mib, testutil.T0)
	if next := f.append(output.Piece{Stream: errs, Source: "s1", Offset: 10, Data: []byte("two\n")}, mib, testutil.T0); next != 14 {
		t.Fatalf("next after a gap = %d, want 14", next)
	}
	if next := f.append(output.Piece{Stream: errs, Source: "s2", Data: []byte("new file\n")}, mib, testutil.T0); next != 9 {
		t.Fatalf("next for a new source = %d, want 9", next)
	}
	if next := f.append(output.Piece{Stream: errs, Source: "s2", Restart: true, Data: []byte("again\n")}, mib, testutil.T0); next != 6 {
		t.Fatalf("next after a restart = %d, want 6", next)
	}
	want := "stderr:one\neinsora:6 bytes of stderr was lost here: it was gone from the machine before Einsora received it\n" +
		"stderr:two\neinsora:stderr from here on comes from a new file on its machine: a new run, or another machine\n" +
		"stderr:new file\neinsora:stderr from here on starts over: its file on its machine was emptied\nstderr:again\n"
	if got := texts(f.chunks()); got != want {
		t.Errorf("stored %q,\nwant %q", got, want)
	}
	if st := f.state(errs); st.LostBytes != 6 || st.Source != "s2" {
		t.Errorf("state = %+v", st)
	}
}

// Output is stored as valid UTF-8 without NULs, and with credentials removed and counted.
func TestAppendCleans(t *testing.T) {
	f := newFixture(t, domain.RegionLocal)
	raw := "Jupyter Server is running at:\nhttp://127.0.0.1:8888/lab?token=" + fakeToken + "\n\xff\x00ok\n"
	f.append(output.Piece{Stream: domain.StreamStderr, Source: "s", Data: []byte(raw)}, mib, testutil.T0)
	cs := f.chunks()
	if len(cs) != 1 || strings.Contains(cs[0].Text, fakeToken) || cs[0].Redactions != 1 {
		t.Fatalf("stored %+v; want the token removed and counted", cs)
	}
	if !strings.Contains(cs[0].Text, "token=REDACTED") || !strings.Contains(cs[0].Text, "��ok") {
		t.Errorf("stored %q", cs[0].Text)
	}
	// The offset counts what the machine sent, not what was stored.
	if st := f.state(domain.StreamStderr); st.Next != int64(len(raw)) {
		t.Errorf("next = %d, want %d", st.Next, len(raw))
	}
}

// Over its size limit, a workload's oldest output is removed, and counted as trimmed.
func TestAppendTrims(t *testing.T) {
	f := newFixture(t, domain.RegionLocal)
	line := []byte("123456789\n")
	var next int64
	for range 30 {
		next = f.append(output.Piece{Stream: domain.StreamStdout, Source: "s", Offset: next, Data: line}, 100, testutil.T0)
	}
	cs := f.chunks()
	st := f.state(domain.StreamStdout)
	if st.StoredBytes > 100 || st.StoredBytes+st.TrimmedBytes != 300 || st.StoredBytes != int64(10*len(cs)) {
		t.Errorf("state = %+v with %d chunks; want at most 100 bytes kept and the rest counted as trimmed", st, len(cs))
	}
	if next != 300 {
		t.Errorf("next = %d; trimming mustn't change where the agent sends from", next)
	}
}

// Expire removes old output of the region's workloads only, counted as expired.
func TestExpire(t *testing.T) {
	f := newFixture(t, domain.RegionLocal)
	away := newFixture(t, "eu-west")
	away.st = f.st // the same store, a workload in another region
	err := f.st.InTx(context.Background(), func(q store.Queries) error {
		if err := q.CreateTenant(context.Background(), domain.Tenant{ID: away.w.TenantID, Slug: "bravo", Name: "bravo", Tier: domain.TierTrusted, CreatedAt: testutil.T0}); err != nil {
			return err
		}
		return q.CreateWorkload(context.Background(), away.w)
	})
	if err != nil {
		t.Fatal(err)
	}
	f.append(output.Piece{Stream: domain.StreamStdout, Source: "s", Data: []byte("old\n")}, mib, testutil.T0)
	f.append(output.Piece{Stream: domain.StreamStdout, Source: "s", Offset: 4, Data: []byte("new\n")}, mib, testutil.T0.Add(48*time.Hour))
	away.append(output.Piece{Stream: domain.StreamStdout, Source: "s", Data: []byte("elsewhere\n")}, mib, testutil.T0)

	removed, err := output.Expire(context.Background(), f.st, domain.RegionLocal, testutil.T0.Add(24*time.Hour))
	if err != nil || removed != 4 {
		t.Fatalf("Expire = %d, %v; want 4 bytes", removed, err)
	}
	if got := texts(f.chunks()); got != "stdout:new\n" {
		t.Errorf("left %q", got)
	}
	if st := f.state(domain.StreamStdout); st.ExpiredBytes != 4 || st.StoredBytes != 4 {
		t.Errorf("state = %+v", st)
	}
	if got := texts(away.chunks()); got != "stdout:elsewhere\n" {
		t.Errorf("another region's output: %q", got)
	}
}

func TestSize(t *testing.T) {
	for n, want := range map[int64]string{0: "0 bytes", 1023: "1023 bytes", 1536: "1.5 KiB", 64 << 20: "64.0 MiB", 3 << 30: "3.0 GiB"} {
		if got := output.Size(n); got != want {
			t.Errorf("Size(%d) = %q, want %q", n, got, want)
		}
	}
}
