//go:build darwin

package macosnative

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/krishnanand20/einsora/controlplane/internal/agent/drivers/process"
	"github.com/krishnanand20/einsora/controlplane/internal/agent/drivers/pythonruntime"
	"github.com/krishnanand20/einsora/controlplane/internal/agent/protocol"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
)

func (d *Driver) sandboxFingerprint(kind domain.WorkloadKind, runtime string) string {
	if kind != domain.KindEndpoint {
		return d.sandboxID
	}
	// Dependency-only security updates must also retire old serving processes. Older state files
	// have no runtime: their endpoints all ran MLX.
	if runtime == "" {
		runtime = "mlx-lm"
	}
	lock, _ := envs.ReadFile("envs/" + runtime + ".txt") // this fixed file is embedded with the runtime
	data := append([]byte(d.sandboxID), runtimeExtras(runtime)...)
	sum := sha256.Sum256(append(data, lock...))
	return hex.EncodeToString(sum[:])
}

// runtimeExtras is what an endpoint runtime's environment holds besides its lock: the embedded
// Python worker and, for llama.cpp, the pinned release.
func runtimeExtras(runtime string) []byte {
	switch runtime {
	case "mlx-lm":
		return pythonruntime.Bytes()
	case llamaRuntime:
		return append(append([]byte(nil), pythonruntime.Bytes()...), llamaCpp.SHA256...)
	}
	return nil
}

// nativeEndpoint reports whether w is an endpoint this driver serves: MLX with safetensors on the
// Apple GPU, or llama.cpp with GGUF on the Apple GPU or CPU, from a model source, with a memory
// allocation (its model's size limit) and CPUs (llama.cpp's threads).
func nativeEndpoint(w protocol.Desired) bool {
	if w.MemoryMiB <= 0 || (!strings.HasPrefix(w.ModelURI, "hf://") && !strings.HasPrefix(w.ModelURI, "s3://")) {
		return false
	}
	switch w.Runtime {
	case protocol.Runtime{Ref: "mlx-lm", Server: "mlx", ModelFormat: "safetensors"}:
		return w.Accelerator == domain.AcceleratorApple
	case protocol.Runtime{Ref: llamaRuntime, Server: "llama.cpp", ModelFormat: "gguf"}:
		return w.VCPUs > 0 && (w.Accelerator == domain.AcceleratorApple || w.Accelerator == domain.AcceleratorCPU)
	}
	return false
}

// healthPath is where a workload's server answers once it's ready.
func healthPath(st state) string {
	if st.Kind == domain.KindEndpoint && st.Runtime == llamaRuntime {
		return "/health"
	}
	return "/api"
}

// endpoint is a model endpoint's worker, under the sandbox: it fetches the model into the
// workload's data directory, validates it, and starts the server. MLX is served by the worker
// itself; for llama.cpp, the worker becomes the pinned llama-server, with the model's validated
// file, on the GPU (Metal) or only the CPU, as allocated.
func (d *Driver) endpoint(w protocol.Desired, env string, st state) process.Spec {
	env = resolved(env)
	run := d.runDir(st)
	args := []string{
		"--source", w.ModelURI, "--model-dir", filepath.Join(d.dir(w.ID), "data", "model"),
		"--socket", d.socket(st), "--status", filepath.Join(run, "status.json"),
		"--max-bytes", strconv.FormatInt(w.MemoryMiB*1024*1024, 10),
	}
	worker := []string{filepath.Join(env, "bin", "python"), "-m", "einsora_ml.native"}
	if w.Runtime.Ref == llamaRuntime {
		devices := "0"
		if w.Accelerator == domain.AcceleratorApple {
			devices = "1"
		}
		worker[2] = "einsora_ml.llama_server"
		args = append(args, "--server", filepath.Join(env, "llama", "llama-server"),
			"--accelerator", string(w.Accelerator), "--devices", devices, "--threads", strconv.Itoa(w.VCPUs))
	}
	spec := d.command(w.ID, env, st, append(worker, args...)...)
	spec.Env = append(spec.Env, "PYTHONPATH="+filepath.Join(env, "einsora-runtime.zip"),
		"EINSORA_NATIVE_TOKEN="+st.Token, "HF_HOME="+filepath.Join(run, "hf"), "HF_HUB_DISABLE_TELEMETRY=1",
		"AWS_EC2_METADATA_DISABLED=true")
	for _, key := range []string{"HF_TOKEN", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"AWS_REGION", "AWS_DEFAULT_REGION", "AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_S3", "HTTP_PROXY", "HTTPS_PROXY",
		"NO_PROXY", "ALL_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if value, ok := os.LookupEnv(key); ok {
			spec.Env = append(spec.Env, key+"="+value)
		}
	}
	return spec
}

// Only fixed codes from the worker enter a status or timeline. Its status file lives in a writable
// directory, so never follow a symlink or read an unbounded file from outside the sandbox.
func (d *Driver) endpointCode(st state, exited bool) protocol.Code {
	fallback := protocol.CodeFetchingModel
	if exited {
		fallback = protocol.CodeExited
	}
	f, err := os.OpenFile(filepath.Join(d.runDir(st), "status.json"), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fallback
	}
	defer func() { _ = f.Close() }()
	var status struct {
		Stage string        `json:"stage"`
		Code  protocol.Code `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(f, 4096)).Decode(&status); err != nil {
		return fallback
	}
	if exited {
		switch status.Code {
		case protocol.CodeModelRejected, protocol.CodeModelNotFound, protocol.CodeImage, protocol.CodeDriverProblem:
			return status.Code
		}
		return fallback
	}
	switch status.Stage {
	case "validating":
		return protocol.CodeValidatingModel
	case "loading":
		return protocol.CodeLoadingModel
	}
	return fallback
}
