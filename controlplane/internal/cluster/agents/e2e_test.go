package agents_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/krishnanand20/einsora/controlplane/internal/agent"
	"github.com/krishnanand20/einsora/controlplane/internal/agent/drivers/sim"
	"github.com/krishnanand20/einsora/controlplane/internal/agent/protocol"
	"github.com/krishnanand20/einsora/controlplane/internal/cluster"
	"github.com/krishnanand20/einsora/controlplane/internal/cluster/agents"
	"github.com/krishnanand20/einsora/controlplane/internal/controller"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/service"
	"github.com/krishnanand20/einsora/controlplane/internal/store"
	"github.com/krishnanand20/einsora/controlplane/internal/testutil"
)

// The whole path in one process, as in Solo: a notebook is placed on this machine's GPU, the
// in-process agent runs it, and stop, start, and delete reach the agent too.
func TestInProcessAgentEndToEnd(t *testing.T) {
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
	inventory := func(context.Context) (protocol.Inventory, error) { return *mac(), nil }
	go func() {
		_ = agent.New(hub.Conn(f.machine.ID), []agent.Driver{sim.New(domain.AcceleratorApple, domain.AcceleratorCPU)},
			inventory, "test", log).Run(ctx)
	}()

	until := func(w domain.Workload, want domain.State) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if err := ctrl.ReconcileOnce(ctx); err != nil {
				t.Fatal(err)
			}
			got, err := svc.GetWorkloadHistory(ctx, testutil.Internal, w.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("workload never reached %s", want)
	}

	w, err := svc.CreateWorkload(ctx, testutil.Internal, service.CreateWorkloadInput{
		Name: "nb", Kind: domain.KindNotebook, ImageID: "pytorch-notebook", GPUClass: "apple-silicon-32gb", GPUCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	until(w, domain.StateRunning)

	placed, _ := svc.GetWorkload(ctx, testutil.Internal, w.ID)
	if placed.MachineID != f.machine.ID || placed.Driver != domain.DriverSim || placed.PlacedClass != "apple-silicon-32gb" {
		t.Errorf("placement = machine %s driver %q class %q", placed.MachineID, placed.Driver, placed.PlacedClass)
	}
	events, _ := svc.WorkloadEvents(ctx, testutil.Internal, w.ID)
	admitted := false
	for _, e := range events {
		admitted = admitted || e.Reason == "admitted: 1 × apple-silicon-32gb on studio with sim"
	}
	if !admitted {
		t.Errorf("the admission reason must say the machine and driver: %+v", events)
	}
	// The controller's first passes ran before the agent's first sync: it waited, rather than
	// queueing the workload for a machine that was about to connect.
	for _, e := range events {
		if e.To == domain.StateQueued {
			t.Errorf("queued while this machine was connecting: %q", e.Reason)
		}
	}

	if _, err := svc.StopWorkload(ctx, testutil.Internal, w.ID); err != nil {
		t.Fatal(err)
	}
	until(w, domain.StateStopped)
	if _, err := svc.StartWorkload(ctx, testutil.Internal, w.ID); err != nil {
		t.Fatal(err)
	}
	until(w, domain.StateRunning)
	if _, err := svc.DeleteWorkload(ctx, testutil.Internal, w.ID); err != nil {
		t.Fatal(err)
	}
	until(w, domain.StateDeleted)
}

// A workload that crashes is failed with the crash's reason, isn't restarted under the same
// attempt, and starting it again is a new attempt that the old failure can't answer.
func TestCrashThenRestart(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cat := testutil.Catalog(t)
	log := slog.New(slog.DiscardHandler)
	hub := agents.New(f.st, cat, agents.Options{PollTimeout: 20 * time.Millisecond, AllowSim: true}, log, nil)
	ctrl := controller.New(f.st, hub, cat, controller.Options{ProvisionTimeout: time.Minute}, log, nil)
	svc := service.New(f.st, cat, nil)
	drv := sim.New(domain.AcceleratorApple, domain.AcceleratorCPU)
	inventory := func(context.Context) (protocol.Inventory, error) { return *mac(), nil }
	go func() { _ = agent.New(hub.Conn(f.machine.ID), []agent.Driver{drv}, inventory, "test", log).Run(ctx) }()
	until := func(w domain.Workload, want domain.State) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if err := ctrl.ReconcileOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if got, _ := svc.GetWorkloadHistory(ctx, testutil.Internal, w.ID); got.State == want {
				return
			}
		}
		t.Fatalf("workload never reached %s", want)
	}

	w, err := svc.CreateWorkload(ctx, testutil.Internal, service.CreateWorkloadInput{
		Name: "nb", Kind: domain.KindNotebook, ImageID: "pytorch-notebook", GPUClass: "cpu-2c-8g", GPUCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	until(w, domain.StateRunning)
	drv.Crash(w.ID)
	until(w, domain.StateFailed)
	time.Sleep(100 * time.Millisecond) // a few syncs: the agent must not start the old attempt again
	sts, _ := drv.Statuses(ctx)
	if len(sts) != 1 || sts[0].Attempt != 1 || sts[0].State == protocol.StateReady {
		t.Errorf("after the failure: %+v; want attempt 1 not running again", sts)
	}

	if _, err := svc.StartWorkload(ctx, testutil.Internal, w.ID); err != nil {
		t.Fatal(err)
	}
	until(w, domain.StateRunning)
	events, _ := svc.WorkloadEvents(ctx, testutil.Internal, w.ID)
	var failures []string
	for _, e := range events {
		if e.To == domain.StateFailed {
			failures = append(failures, e.Reason)
		}
	}
	if len(failures) != 1 || failures[0] != "workload failed: it exited unexpectedly" {
		t.Errorf("failures = %q; want only the crash, not the restart failed by its old status", failures)
	}
	if sts, _ := drv.Statuses(ctx); len(sts) != 1 || sts[0].Attempt != 2 || sts[0].State != protocol.StateReady {
		t.Errorf("after the restart: %+v; want attempt 2 running", sts)
	}
}

// The control plane hears at once when an agent has done what it was asked, not when the agent's
// long poll ends: with a 20-second poll, a workload is running within a pass or two.
func TestAgentsReportPromptly(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cat := testutil.Catalog(t)
	log := slog.New(slog.DiscardHandler)
	hub := agents.New(f.st, cat, agents.Options{PollTimeout: 20 * time.Second, AllowSim: true}, log, nil)
	ctrl := controller.New(f.st, hub, cat, controller.Options{ProvisionTimeout: time.Minute}, log, nil)
	svc := service.New(f.st, cat, nil)
	inventory := func(context.Context) (protocol.Inventory, error) { return *mac(), nil }
	go func() {
		_ = agent.New(hub.Conn(f.machine.ID), []agent.Driver{sim.New(domain.AcceleratorCPU)}, inventory, "test", log).Run(ctx)
	}()
	w, err := svc.CreateWorkload(ctx, testutil.Internal, service.CreateWorkloadInput{
		Name: "nb", Kind: domain.KindNotebook, ImageID: "pytorch-notebook", GPUClass: "cpu-2c-8g", GPUCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if err := ctrl.ReconcileOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if got, _ := svc.GetWorkload(ctx, testutil.Internal, w.ID); got.State == domain.StateRunning {
			return
		}
	}
	t.Fatal("not running within 3 seconds: the agent's report waited for its long poll")
}

// noImage is a driver whose workloads can't start: their image can't be fetched.
type noImage struct{ agent.Driver }

func (noImage) Run(context.Context, protocol.Desired) error {
	return agent.WithCode(protocol.CodeImage, errors.New("pull failed"))
}

// A workload that can't start fails promptly with the reason, even while the machine's agent is
// woken again and again (nudges for other workloads), rather than sitting in provisioning until
// the timeout with nothing said.
func TestStartFailuresReachTheTimeline(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cat := testutil.Catalog(t)
	log := slog.New(slog.DiscardHandler)
	hub := agents.New(f.st, cat, agents.Options{PollTimeout: 20 * time.Second, AllowSim: true}, log, nil)
	ctrl := controller.New(f.st, hub, cat, controller.Options{ProvisionTimeout: time.Hour}, log, nil)
	svc := service.New(f.st, cat, nil)
	inventory := func(context.Context) (protocol.Inventory, error) { return *mac(), nil }
	go func() {
		_ = agent.New(hub.Conn(f.machine.ID), []agent.Driver{noImage{sim.New(domain.AcceleratorCPU)}}, inventory, "test", log).Run(ctx)
	}()
	w, err := svc.CreateWorkload(ctx, testutil.Internal, service.CreateWorkloadInput{
		Name: "nb", Kind: domain.KindNotebook, ImageID: "pytorch-notebook", GPUClass: "cpu-2c-8g", GPUCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if err := ctrl.ReconcileOnce(ctx); err != nil {
			t.Fatal(err)
		}
		_ = hub.Park(ctx, cluster.Spec{Workload: domain.Workload{MachineID: f.machine.ID}}) // another workload's nudge
		if got, _ := svc.GetWorkloadHistory(ctx, testutil.Internal, w.ID); got.State == domain.StateFailed {
			events, _ := svc.WorkloadEvents(ctx, testutil.Internal, w.ID)
			if last := events[len(events)-1].Reason; last != "workload failed: its machine couldn't get the image or model" {
				t.Errorf("reason = %q", last)
			}
			return
		}
	}
	t.Fatal("a workload that couldn't start never failed")
}
