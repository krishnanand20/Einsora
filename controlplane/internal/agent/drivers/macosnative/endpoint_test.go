//go:build darwin

package macosnative

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tensora/tensora/controlplane/internal/agent/protocol"
	"github.com/tensora/tensora/controlplane/internal/domain"
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
	lock, err := envs.ReadFile("envs/mlx-lm.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, env := d.envDir("mlx-lm", lock)
	if err := os.MkdirAll(filepath.Join(env, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"bin/python": []byte(python), ".tensora-ready": {}, "tensora-runtime.zip": nativeRuntime,
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

func TestNativeBundleMatchesMLSources(t *testing.T) {
	archive, err := zip.NewReader(bytes.NewReader(nativeRuntime), int64(len(nativeRuntime)))
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join("..", "..", "..", "..", "..", "ml", "src")
	files, err := filepath.Glob(filepath.Join(source, "tensora_ml", "*.py"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(archive.File) {
		t.Fatal("runtime bundle is stale: run python3 ml/scripts/bundle_native.py")
	}
	for _, file := range archive.File {
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		original, err := os.ReadFile(filepath.Join(source, filepath.FromSlash(file.Name)))
		if err != nil || !bytes.Equal(data, original) {
			t.Errorf("%s differs from its ML source; regenerate runtime.zip", file.Name)
		}
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
		Kind: w.Kind, Sandbox: d.sandboxFingerprint(w.Kind), ModelURI: w.ModelURI, MemoryMiB: w.MemoryMiB}
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
