package agents_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/agent"
	"github.com/krishnanand20/einsora/controlplane/internal/agent/drivers/sim"
	"github.com/krishnanand20/einsora/controlplane/internal/agent/protocol"
	"github.com/krishnanand20/einsora/controlplane/internal/cluster/agents"
	"github.com/krishnanand20/einsora/controlplane/internal/controller"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/service"
	"github.com/krishnanand20/einsora/controlplane/internal/store"
	"github.com/krishnanand20/einsora/controlplane/internal/testutil"
)

// fakeToken stands in for a notebook server's token. It's made at run time, so secret scanners
// don't mistake the test for a real one.
var fakeToken = strings.Repeat("f", 16)

// storedOutput is everything kept of a workload's output, as one string per stream.
func storedOutput(t *testing.T, st store.Store, tenant domain.Tenant, id uuid.UUID) map[domain.OutputStream]string {
	t.Helper()
	out := map[domain.OutputStream]string{}
	err := st.InTx(context.Background(), func(q store.Queries) error {
		cs, err := q.OutputForWorkload(context.Background(), tenant.ID, id, 0, 1000)
		for _, c := range cs {
			out[c.Stream] += c.Text
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The hub keeps output only for workloads placed on the machine that sends it, and refuses
// requests that break the protocol's limits.
func TestOutputIsKeptOnlyForTheMachinesWorkloads(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	w := f.place(t, f.owner, domain.KindNotebook, "apple-silicon-32gb", domain.StateRunning)
	elsewhere := uuid.New()
	req := func(chunks ...protocol.OutputChunk) protocol.OutputRequest {
		return protocol.OutputRequest{Version: protocol.Version, Chunks: chunks}
	}

	resp, err := f.hub.Output(ctx, f.machine.ID, req(
		protocol.OutputChunk{ID: w.ID, Stream: domain.StreamStdout, Source: "inode-1"},
		protocol.OutputChunk{ID: w.ID, Stream: domain.StreamStderr, Source: "inode-2", Data: []byte("loading\n")},
		protocol.OutputChunk{ID: elsewhere, Stream: domain.StreamStdout, Source: "inode-3", Data: []byte("not yours\n")},
	))
	if err != nil {
		t.Fatal(err)
	}
	want := []protocol.OutputCursor{
		{ID: w.ID, Stream: domain.StreamStdout, Source: "inode-1", Next: 0},
		{ID: w.ID, Stream: domain.StreamStderr, Source: "inode-2", Next: 8},
		{ID: elsewhere, Stream: domain.StreamStdout, Ignored: true},
	}
	if len(resp.Streams) != len(want) {
		t.Fatalf("streams = %+v", resp.Streams)
	}
	for i := range want {
		if resp.Streams[i] != want[i] {
			t.Errorf("stream %d = %+v, want %+v", i, resp.Streams[i], want[i])
		}
	}
	if got := storedOutput(t, f.st, f.owner, w.ID); got[domain.StreamStderr] != "loading\n" || got[domain.StreamStdout] != "" {
		t.Errorf("stored %q", got)
	}
	if got := storedOutput(t, f.st, f.guest, w.ID); len(got) != 0 {
		t.Errorf("another tenant reads %q", got)
	}

	ok := protocol.OutputChunk{ID: w.ID, Stream: domain.StreamStdout, Source: "s"}
	for name, bad := range map[string]protocol.OutputRequest{
		"version":     {Version: protocol.Version + 1, Chunks: []protocol.OutputChunk{ok}},
		"stream":      req(protocol.OutputChunk{ID: w.ID, Stream: domain.StreamEinsora, Source: "s"}),
		"no source":   req(protocol.OutputChunk{ID: w.ID, Stream: domain.StreamStdout}),
		"source":      req(protocol.OutputChunk{ID: w.ID, Stream: domain.StreamStdout, Source: "s\x1b[2J"}),
		"big chunk":   req(protocol.OutputChunk{ID: w.ID, Stream: domain.StreamStdout, Source: "s", Data: make([]byte, protocol.MaxOutputChunk+1)}),
		"offset":      req(protocol.OutputChunk{ID: w.ID, Stream: domain.StreamStdout, Source: "s", Offset: -1}),
		"many chunks": req(make([]protocol.OutputChunk, protocol.MaxOutputChunks+1)...),
	} {
		if _, err := f.hub.Output(ctx, f.machine.ID, bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := f.hub.Output(ctx, uuid.New(), req(ok)); err == nil {
		t.Error("output from an unknown machine was accepted")
	}
}

// The in-process agent ships a workload's output as it's written: whole lines at once, a partial
// line once it has waited, and the rest just before the workload is removed.
func TestOutputEndToEnd(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cat := testutil.Catalog(t)
	if err := f.st.InTx(ctx, func(q store.Queries) error {
		return q.SetQuota(ctx, domain.Quota{TenantID: f.owner.ID, GPUClass: "apple-silicon-32gb", MaxGPUs: 1})
	}); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	hub := agents.New(f.st, cat, agents.Options{PollTimeout: 20 * time.Millisecond, AllowSim: true}, log, nil)
	ctrl := controller.New(f.st, hub, cat, controller.Options{ProvisionTimeout: time.Minute}, log, nil)
	svc := service.New(f.st, cat, nil)
	drv := sim.New(domain.AcceleratorApple, domain.AcceleratorCPU)
	inventory := func(context.Context) (protocol.Inventory, error) { return *mac(), nil }
	go func() { _ = agent.New(hub.Conn(f.machine.ID), []agent.Driver{drv}, inventory, "test", log).Run(ctx) }()

	w, err := svc.CreateWorkload(ctx, testutil.Internal, service.CreateWorkloadInput{
		Name: "nb", Kind: domain.KindNotebook, ImageID: "pytorch-notebook", GPUClass: "apple-silicon-32gb", GPUCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// until reconciles until cond holds for the stored output, or fails after a while.
	until := func(what string, cond func(map[domain.OutputStream]string) bool) {
		t.Helper()
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			if err := ctrl.ReconcileOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if cond(storedOutput(t, f.st, f.owner, w.ID)) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%s: stored %q", what, storedOutput(t, f.st, f.owner, w.ID))
	}

	until("its first line", func(o map[domain.OutputStream]string) bool {
		return strings.Contains(o[domain.StreamStdout], "sim: pretending to run this notebook on apple (attempt 1)")
	})
	drv.Write(w.ID, domain.StreamStderr, "epoch 1 of 2\nepoch 2")
	until("whole lines", func(o map[domain.OutputStream]string) bool { return o[domain.StreamStderr] == "epoch 1 of 2\n" })
	until("a partial line, once it has waited", func(o map[domain.OutputStream]string) bool {
		return o[domain.StreamStderr] == "epoch 1 of 2\nepoch 2"
	})

	drv.Write(w.ID, domain.StreamStderr, " of 2: done, http://127.0.0.1:8888/lab?token="+fakeToken)
	if _, err := svc.DeleteWorkload(ctx, testutil.Internal, w.ID); err != nil {
		t.Fatal(err)
	}
	until("the rest, before it's removed", func(o map[domain.OutputStream]string) bool {
		return strings.HasSuffix(o[domain.StreamStdout], "sim: stopped\n") && strings.Contains(o[domain.StreamStderr], "done")
	})
	if got := storedOutput(t, f.st, f.owner, w.ID)[domain.StreamStderr]; strings.Contains(got, fakeToken) || !strings.Contains(got, "token=REDACTED") {
		t.Errorf("the token wasn't removed: %q", got)
	}
}
