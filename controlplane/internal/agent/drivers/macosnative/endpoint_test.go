//go:build darwin

package macosnative

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/agent/drivers/pythonruntime"
	"github.com/krishnanand20/einsora/controlplane/internal/agent/protocol"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
)

func fakeEndpoint(args []string) {
	values := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if strings.HasPrefix(args[i], "--") {
			values[args[i]] = args[i+1]
		}
	}
	status := func(stage, code string) {
		raw, _ := json.Marshal(map[string]string{"stage": stage, "code": code})
		_ = os.WriteFile(values["--status"], raw, 0o600)
	}
	// What it was started with, for the tests: its run directory is beside its status file.
	raw, _ := json.Marshal(args)
	_ = os.WriteFile(filepath.Join(filepath.Dir(values["--status"]), "args.json"), raw, 0o600)
	status("fetching", "")
	if values["--source"] == "hf://org/preparing" {
		time.Sleep(2 * time.Minute)
		return
	}
	if values["--source"] == "hf://org/rejected" {
		time.Sleep(100 * time.Millisecond)
		status("failed", "model-rejected")
		return
	}
	l, err := net.Listen("unix", values["--socket"])
	if err != nil {
		os.Exit(3)
	}
	_ = http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ready":true}`)
	}))
}

func endpointWorkload() protocol.Desired {
	w := notebook(1)
	w.Kind, w.ModelURI, w.MemoryMiB = domain.KindEndpoint, "hf://org/model", 1024
	w.Runtime = protocol.Runtime{Ref: "mlx-lm", Server: "mlx", ModelFormat: "safetensors"}
	return w
}

func endpointEnvironment(t *testing.T, d *Driver, python string) string {
	t.Helper()
	return runtimeEnvironment(t, d, "mlx-lm", python)
}

// runtimeEnvironment is a built environment for an endpoint runtime, whose Python is python.
func runtimeEnvironment(t *testing.T, d *Driver, ref, python string) string {
	t.Helper()
	lock, err := envs.ReadFile("envs/" + ref + ".txt")
	if err != nil {
		t.Fatal(err)
	}
	_, env := d.envDir(ref, lock)
	if err := os.MkdirAll(filepath.Join(env, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"bin/python": []byte(python), ".einsora-ready": {}, "einsora-runtime.zip": pythonruntime.Bytes(),
	} {
		mode := os.FileMode(0o600)
		if name == "bin/python" {
			mode = 0o700
		}
		if err := os.WriteFile(filepath.Join(env, name), data, mode); err != nil {
			t.Fatal(err)
		}
	}
	return env
}

func TestEndpointLifecycleAndRestart(t *testing.T) {
	d, stateDir := newDriver(t)
	self, _ := os.Executable()
	endpointEnvironment(t, d, fmt.Sprintf("#!/bin/sh\nexec %q fake-endpoint \"$@\"\n", self))
	ctx := context.Background()
	w := endpointWorkload()
	if err := d.Run(ctx, w); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Remove(ctx, w.ID) })
	initial := status(t, d, w.ID, ready)
	again, err := New(Options{Root: filepath.Join(stateDir, "native"), State: stateDir, Runs: d.opts.Runs,
		UV: "/usr/bin/false", Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if found := status(t, again, w.ID, ready); found.Access.Token != initial.Access.Token {
		t.Error("a control-plane restart replaced the model server")
	}
	if err := again.Stop(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	status(t, again, w.ID, func(s protocol.Status) bool { return s.State == protocol.StateStopped })
	st, _, _ := again.readState(w.ID)
	run := again.runDir(st)
	if err := again.Remove(ctx, w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(run); !os.IsNotExist(err) {
		t.Errorf("endpoint run directory survived removal: %v", err)
	}
}

func TestEndpointPreparationCanStopAndPolicyFailuresAreFixed(t *testing.T) {
	for _, source := range []string{"hf://org/preparing", "hf://org/rejected"} {
		t.Run(source, func(t *testing.T) {
			d, _ := newDriver(t)
			self, _ := os.Executable()
			endpointEnvironment(t, d, fmt.Sprintf("#!/bin/sh\nexec %q fake-endpoint \"$@\"\n", self))
			w := endpointWorkload()
			w.ModelURI = source
			ctx := context.Background()
			if err := d.Run(ctx, w); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = d.Remove(ctx, w.ID) })
			if strings.HasSuffix(source, "rejected") {
				failed := status(t, d, w.ID, func(s protocol.Status) bool { return s.State == protocol.StateFailed })
				if failed.Code != protocol.CodeModelRejected || failed.Access != nil {
					t.Errorf("model failure = %+v", failed)
				}
			} else {
				status(t, d, w.ID, func(s protocol.Status) bool { return s.Code == protocol.CodeFetchingModel })
				if err := d.Stop(ctx, w.ID); err != nil {
					t.Fatal(err)
				}
				status(t, d, w.ID, func(s protocol.Status) bool { return s.State == protocol.StateStopped })
			}
		})
	}
}

func TestEndpointPreparationResumesAfterRestart(t *testing.T) {
	d, _ := newDriver(t)
	self, _ := os.Executable()
	endpointEnvironment(t, d, fmt.Sprintf("#!/bin/sh\nexec %q fake-endpoint \"$@\"\n", self))
	w := endpointWorkload()
	for _, sub := range []string{"data", "home", "logs"} {
		if err := os.MkdirAll(filepath.Join(d.dir(w.ID), sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	run, err := d.newRunName()
	if err != nil {
		t.Fatal(err)
	}
	token, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	st := state{Attempt: w.Attempt, Phase: phasePreparing, Run: run, Token: token, Runtime: w.Runtime.Ref,
		Kind: w.Kind, Sandbox: d.sandboxFingerprint(w.Kind, w.Runtime.Ref), ModelURI: w.ModelURI, MemoryMiB: w.MemoryMiB}
	if err := d.writeState(w.ID, st); err != nil {
		t.Fatal(err)
	}
	again, err := New(d.opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = again.Remove(context.Background(), w.ID) })
	if ready := status(t, again, w.ID, ready); ready.Attempt != w.Attempt || ready.Access.Token != token {
		t.Error("resuming preparation changed the attempt")
	}
}

func llamaWorkload(accelerator domain.AcceleratorKind) protocol.Desired {
	w := endpointWorkload()
	w.ModelURI = "hf://org/model-GGUF:Q4_K_M"
	w.Runtime = protocol.Runtime{Ref: llamaRuntime, Server: "llama.cpp", ModelFormat: "gguf"}
	w.Accelerator, w.Devices, w.VCPUs = accelerator, 0, 4
	if accelerator == domain.AcceleratorApple {
		w.Devices = 1
	}
	return w
}

// startedWith is the arguments a workload's (fake) worker was started with.
func startedWith(t *testing.T, d *Driver, id uuid.UUID) map[string]string {
	t.Helper()
	st, _, err := d.readState(id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(d.runDir(st), "args.json"))
	if err != nil {
		t.Fatal(err)
	}
	var args []string
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			values[args[i]] = args[i+1]
		}
	}
	return values
}

func TestLlamaEndpointRunsOnItsAllocation(t *testing.T) {
	for _, tt := range []struct {
		accelerator domain.AcceleratorKind
		devices     string
	}{{domain.AcceleratorApple, "1"}, {domain.AcceleratorCPU, "0"}} {
		t.Run(string(tt.accelerator), func(t *testing.T) {
			d, _ := newDriver(t)
			self, _ := os.Executable()
			env := runtimeEnvironment(t, d, llamaRuntime, fmt.Sprintf("#!/bin/sh\nexec %q fake-endpoint \"$@\"\n", self))
			w := llamaWorkload(tt.accelerator)
			ctx := context.Background()
			if err := d.Run(ctx, w); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = d.Remove(ctx, w.ID) })
			status(t, d, w.ID, ready) // answered at /health
			want := map[string]string{
				"-m": "einsora_ml.llama_server", "--source": w.ModelURI, "--server": filepath.Join(resolved(env), "llama", "llama-server"),
				"--accelerator": string(tt.accelerator), "--devices": tt.devices, "--threads": "4",
				"--max-bytes": strconv.FormatInt(w.MemoryMiB*1024*1024, 10),
			}
			got := startedWith(t, d, w.ID)
			for flag, value := range want {
				if got[flag] != value {
					t.Errorf("%s = %q, want %q", flag, got[flag], value)
				}
			}
		})
	}
}

// A llama.cpp endpoint whose environment was still being built when the agent restarted starts on
// the allocation it was given: the state file keeps it.
func TestLlamaEndpointPreparationResumesWithItsAllocation(t *testing.T) {
	d, _ := newDriver(t)
	self, _ := os.Executable()
	runtimeEnvironment(t, d, llamaRuntime, fmt.Sprintf("#!/bin/sh\nexec %q fake-endpoint \"$@\"\n", self))
	w := llamaWorkload(domain.AcceleratorCPU)
	for _, sub := range []string{"data", "home", "logs"} {
		if err := os.MkdirAll(filepath.Join(d.dir(w.ID), sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	run, err := d.newRunName()
	if err != nil {
		t.Fatal(err)
	}
	st := state{Attempt: w.Attempt, Phase: phasePreparing, Run: run, Token: strings.Repeat("a", 64), Runtime: w.Runtime.Ref,
		Kind: w.Kind, Sandbox: d.sandboxFingerprint(w.Kind, w.Runtime.Ref), ModelURI: w.ModelURI, MemoryMiB: w.MemoryMiB,
		Accelerator: w.Accelerator, VCPUs: w.VCPUs}
	if err := d.writeState(w.ID, st); err != nil {
		t.Fatal(err)
	}
	again, err := New(d.opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = again.Remove(context.Background(), w.ID) })
	status(t, again, w.ID, ready)
	if got := startedWith(t, again, w.ID); got["--accelerator"] != "cpu" || got["--threads"] != "4" || got["-m"] != "einsora_ml.llama_server" {
		t.Errorf("resumed with %v", got)
	}
}

func TestNativeEndpointsServeWhatTheyCheck(t *testing.T) {
	llama := llamaWorkload(domain.AcceleratorCPU)
	for name, tt := range map[string]struct {
		change func(*protocol.Desired)
		ok     bool
	}{
		"MLX on the GPU":         {func(w *protocol.Desired) { *w = endpointWorkload() }, true},
		"MLX on the CPU":         {func(w *protocol.Desired) { *w = endpointWorkload(); w.Accelerator = domain.AcceleratorCPU }, false},
		"llama.cpp on the CPU":   {func(*protocol.Desired) {}, true},
		"llama.cpp on the GPU":   {func(w *protocol.Desired) { *w = llamaWorkload(domain.AcceleratorApple) }, true},
		"llama.cpp, safetensors": {func(w *protocol.Desired) { w.Runtime.ModelFormat = "safetensors" }, false},
		"MLX, GGUF":              {func(w *protocol.Desired) { *w = endpointWorkload(); w.Runtime.ModelFormat = "gguf" }, false},
		"llama.cpp, no CPUs":     {func(w *protocol.Desired) { w.VCPUs = 0 }, false},
		"llama.cpp, on NVIDIA":   {func(w *protocol.Desired) { w.Accelerator = domain.AcceleratorNVIDIA }, false},
		"no memory":              {func(w *protocol.Desired) { w.MemoryMiB = 0 }, false},
		"another source":         {func(w *protocol.Desired) { w.ModelURI = "https://example.com/model.gguf" }, false},
		"another server":         {func(w *protocol.Desired) { w.Runtime.Server = "vllm" }, false},
	} {
		w := llama
		tt.change(&w)
		if nativeEndpoint(w) != tt.ok {
			t.Errorf("%s: nativeEndpoint = %v, want %v", name, !tt.ok, tt.ok)
		}
	}
}
