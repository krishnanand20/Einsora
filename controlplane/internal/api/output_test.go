package api_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

// fakeToken stands in for a notebook server's token. It's made at run time, so secret scanners
// don't mistake the test for a real one.
var fakeToken = strings.Repeat("f", 16)

type outputFixture struct {
	t  *testing.T
	st store.Store
	h  http.Handler
	w  domain.Workload // tenant internal's notebook, placed on its machine with the sim driver
}

func newOutputFixture(t *testing.T) *outputFixture {
	t.Helper()
	st := testutil.SeededStore(t)
	svc := service.New(st, testutil.Catalog(t), nil)
	f := &outputFixture{t: t, st: st, h: api.New(svc, auth.Dev{}, slog.New(slog.DiscardHandler), st.Ping).Handler()}
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
	f.w = w
	f.w.Attempt = 1
	return f
}

// write stores output for the workload as its agent would send it.
func (f *outputFixture) write(stream domain.OutputStream, offset int64, text string) {
	f.t.Helper()
	err := f.st.InTx(context.Background(), func(q store.Queries) error {
		_, err := output.Append(context.Background(), q, f.w, output.Piece{Stream: stream, Source: "s", Offset: offset, Data: []byte(text)}, 1<<20, time.Now())
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

type outputPage struct {
	Items []struct {
		ID         int64  `json:"id"`
		Stream     string `json:"stream"`
		Text       string `json:"text"`
		Redactions int    `json:"redactions"`
	} `json:"items"`
	Next      int64    `json:"next"`
	Collected bool     `json:"collected"`
	Notes     []string `json:"notes"`
}

func (f *outputFixture) logs(tenant, query string) (int, outputPage) {
	f.t.Helper()
	req := httptest.NewRequest("GET", "/v1/workloads/"+f.w.ID.String()+"/logs"+query, nil)
	req.Header.Set("X-Dev-Tenant", tenant)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	var page outputPage
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			f.t.Fatalf("logs: %v %s", err, rec.Body)
		}
	}
	return rec.Code, page
}

func TestLogsEndpoint(t *testing.T) {
	f := newOutputFixture(t)
	f.write(domain.StreamStdout, 0, "Jupyter is at http://127.0.0.1:8888/lab?token="+fakeToken+"\n")
	f.write(domain.StreamStderr, 0, "warming up\n")

	code, page := f.logs("internal", "")
	if code != 200 || len(page.Items) != 2 || !page.Collected || len(page.Notes) != 0 {
		t.Fatalf("logs = %d %+v", code, page)
	}
	if first := page.Items[0]; first.Stream != "stdout" || strings.Contains(first.Text, fakeToken) || first.Redactions != 1 {
		t.Errorf("first chunk = %+v; want the token removed and counted", first)
	}
	if page.Next != page.Items[1].ID {
		t.Errorf("next = %d, want the last chunk's ID %d", page.Next, page.Items[1].ID)
	}
	if _, again := f.logs("internal", "?after="+itoa(page.Next)); len(again.Items) != 0 || again.Next != page.Next {
		t.Errorf("after the last = %+v", again)
	}
	if code, _ := f.logs("acme", ""); code != 404 {
		t.Errorf("another tenant's logs: %d, want 404", code)
	}
	for _, q := range []string{"?after=-1", "?after=x", "?limit=0", "?limit=1001", "?follow=maybe"} {
		if code, _ := f.logs("internal", q); code != 400 {
			t.Errorf("%s: %d, want 400", q, code)
		}
	}
}

// follow=true waits for new output rather than answering with nothing.
func TestLogsFollow(t *testing.T) {
	f := newOutputFixture(t)
	f.write(domain.StreamStdout, 0, "first\n")
	_, page := f.logs("internal", "")
	go func() {
		time.Sleep(300 * time.Millisecond)
		f.write(domain.StreamStdout, 6, "second\n")
	}()
	start := time.Now()
	_, next := f.logs("internal", "?follow=true&after="+itoa(page.Next))
	if len(next.Items) != 1 || next.Items[0].Text != "second\n" || time.Since(start) > 5*time.Second {
		t.Errorf("follow = %+v after %v", next, time.Since(start))
	}
}

// Readers are told what isn't collected, and what was lost or removed, never shown silence.
func TestLogsNotes(t *testing.T) {
	f := newOutputFixture(t)
	f.write(domain.StreamStdout, 0, "a\n")
	f.write(domain.StreamStdout, 100, "b\n") // 98 bytes never arrived
	_, page := f.logs("internal", "")
	if len(page.Notes) != 1 || !strings.Contains(page.Notes[0], "98 bytes of output was lost") ||
		len(page.Items) != 3 || page.Items[1].Stream != "einsora" {
		t.Errorf("a gap: %+v", page)
	}

	h := newHandler(t)
	_, created := call(t, h, "POST", "/v1/workloads", "acme", endpointBody)
	code, body := call(t, h, "GET", "/v1/workloads/"+created["id"].(string)+"/logs", "acme", "")
	if notes, _ := body["notes"].([]any); code != 200 || len(notes) != 1 || !strings.Contains(notes[0].(string), "hasn't started yet") {
		t.Errorf("before it starts: %d %v", code, body)
	}
}

// The export carries all of the kept output, its streams' counts, and a redaction entry for what
// was removed from it.
func TestExportIncludesOutput(t *testing.T) {
	f := newOutputFixture(t)
	f.write(domain.StreamStdout, 0, "Jupyter is at http://127.0.0.1:8888/lab?token="+fakeToken+"\n")
	req := httptest.NewRequest("GET", "/v1/workloads/"+f.w.ID.String()+"/export", nil)
	req.Header.Set("X-Dev-Tenant", "internal")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	var exp struct {
		OutputLogs struct {
			Available bool              `json:"available"`
			Items     []json.RawMessage `json:"items"`
			Streams   []struct {
				Stream      string `json:"stream"`
				StoredBytes int64  `json:"stored_bytes"`
			} `json:"streams"`
		} `json:"output_logs"`
		Redactions []struct {
			Field string `json:"field"`
		} `json:"redactions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &exp); err != nil || rec.Code != 200 {
		t.Fatalf("export: %d %v", rec.Code, err)
	}
	o := exp.OutputLogs
	if !o.Available || len(o.Items) != 1 || len(o.Streams) != 1 || o.Streams[0].Stream != "stdout" || o.Streams[0].StoredBytes == 0 {
		t.Errorf("output_logs = %+v", o)
	}
	if len(exp.Redactions) != 1 || exp.Redactions[0].Field != "output_logs" {
		t.Errorf("redactions = %+v; want the token removed from the output listed", exp.Redactions)
	}
	if strings.Contains(rec.Body.String(), fakeToken) {
		t.Error("the export contains the token")
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
