package controller_test

import (
	"context"
	"testing"
	"time"

	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/output"
	"github.com/krishnanand20/einsora/controlplane/internal/store"
	"github.com/krishnanand20/einsora/controlplane/internal/testutil"
)

// The leader removes output older than the retention period (14 days by default), and counts it.
func TestExpireOutput(t *testing.T) {
	h := newHarness(t, map[string]int{"l40s-48gb": 4})
	ctx := context.Background()
	w := h.create(testutil.Acme, "nb", 1)
	write := func(offset int64, text string, at time.Time) {
		t.Helper()
		err := h.st.InTx(ctx, func(q store.Queries) error {
			_, err := output.Append(ctx, q, w, output.Piece{Stream: domain.StreamStdout, Source: "s", Offset: offset, Data: []byte(text)}, 1<<20, at)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	write(0, "old\n", testutil.T0)
	write(4, "new\n", testutil.T0.Add(13*24*time.Hour))
	h.clock.Advance(15 * 24 * time.Hour)
	h.ctrl.ExpireOutput(ctx)

	out, err := h.svc.WorkloadOutput(ctx, testutil.Acme, w.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Chunks) != 1 || out.Chunks[0].Text != "new\n" || len(out.States) != 1 || out.States[0].ExpiredBytes != 4 {
		t.Errorf("after expiry: %+v", out)
	}
	found := false
	for _, n := range out.Notes {
		found = found || n == "4 bytes of output was removed after the retention period (EINSORA_OUTPUT_RETENTION)."
	}
	if !found {
		t.Errorf("notes = %q; want the removal said", out.Notes)
	}
}
