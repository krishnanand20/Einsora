//go:build darwin

package macosnative

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
	"github.com/krishnanand20/tensorafi/controlplane/internal/redact"
)

// TestRealNotebook uses an environment previously built from the driver's embedded lock. It
// exercises real Jupyter kernels under the sandbox, without downloading packages during tests.
func TestRealNotebook(t *testing.T) {
	for _, accelerator := range []domain.AcceleratorKind{domain.AcceleratorApple, domain.AcceleratorCPU} {
		t.Run(string(accelerator), func(t *testing.T) { testRealNotebook(t, accelerator) })
	}
}

func testRealNotebook(t *testing.T, accelerator domain.AcceleratorKind) {
	realEnv := os.Getenv("TENSORAFI_TEST_NATIVE_ENV")
	if realEnv == "" {
		t.Skip("set TENSORAFI_TEST_NATIVE_ENV to a driver-built Jupyter environment to test Metal and IPC")
	}
	if !filepath.IsAbs(realEnv) {
		t.Fatal("TENSORAFI_TEST_NATIVE_ENV must be an absolute path")
	}
	d, _ := newDriver(t)
	lock, _ := envs.ReadFile("envs/" + runtimeRef + ".txt")
	_, env := d.envDir(runtimeRef, lock)
	if err := os.RemoveAll(env); err != nil { // the fake environment created by newDriver
		t.Fatal(err)
	}
	if err := os.Symlink(realEnv, env); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	w := notebook(1)
	w.Accelerator = accelerator
	if accelerator == domain.AcceleratorCPU {
		w.Devices = 0
	}
	if err := d.Run(ctx, w); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Remove(context.Background(), w.ID) })
	t.Cleanup(func() {
		if t.Failed() {
			if output, err := os.ReadFile(filepath.Join(d.dir(w.ID), "logs", "stderr.log")); err == nil {
				t.Log(redact.Secrets(string(output)))
			}
		}
	})
	s := status(t, d, w.ID, ready)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", s.Access.Socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/api/kernels", strings.NewReader(`{"name":"python3"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "token "+s.Access.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("creating a real kernel: HTTP %d", resp.StatusCode)
	}
	var kernel struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&kernel); err != nil {
		t.Fatal(err)
	}
	st, _, err := d.readState(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	connection := filepath.Join(d.runDir(st), "kernel-"+kernel.ID+".json")
	// The client talks to the kernel over IPC. GPU work runs inside the sandboxed kernel.
	const check = `
import sys
from jupyter_client import BlockingKernelClient
client = BlockingKernelClient(connection_file=sys.argv[1])
client.load_connection_file()
assert client.transport == "ipc", client.transport
client.start_channels()
try:
    client.wait_for_ready(timeout=30)
    code = """
import torch
import mlx.core as mx
assert torch.backends.mps.is_available(), "PyTorch MPS unavailable"
assert torch.ones(4, device="mps").sum().item() == 4
assert mx.metal.is_available(), "MLX Metal unavailable"
assert mx.sum(mx.ones(4)).item() == 4
print("native notebook: IPC, PyTorch MPS, MLX Metal")
"""
    if sys.argv[2] == "cpu":
        code = 'import torch; t = torch.ones(4, device="cpu"); assert t.device.type == "cpu"; assert t.sum().item() == 4; print("native notebook: IPC, PyTorch CPU")'
    message = client.execute(code)
    output = ""
    while True:
        reply = client.get_iopub_msg(timeout=30)
        if reply.get("parent_header", {}).get("msg_id") != message:
            continue
        content = reply["content"]
        if reply["msg_type"] == "error":
            raise RuntimeError(content["ename"] + ": " + content["evalue"])
        if reply["msg_type"] == "stream":
            output += content["text"]
        if reply["msg_type"] == "status" and content["execution_state"] == "idle":
            break
    expected = "native notebook: IPC, PyTorch CPU" if sys.argv[2] == "cpu" else "native notebook: IPC, PyTorch MPS, MLX Metal"
    assert expected in output, output
    print(output.strip())
finally:
    client.stop_channels()
`
	cmd := exec.CommandContext(ctx, filepath.Join(realEnv, "bin", "python"), "-c", check, connection, string(accelerator))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("real notebook execution: %v: %s", err, out)
	} else {
		t.Log(strings.TrimSpace(string(out)))
	}
}
