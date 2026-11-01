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
	"syscall"

	"github.com/krishnanand20/tensorafi/controlplane/internal/agent/drivers/process"
	"github.com/krishnanand20/tensorafi/controlplane/internal/agent/drivers/pythonruntime"
	"github.com/krishnanand20/tensorafi/controlplane/internal/agent/protocol"
	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
)

func (d *Driver) sandboxFingerprint(kind domain.WorkloadKind) string {
	if kind != domain.KindEndpoint {
		return d.sandboxID
	}
	// Dependency-only security updates must also retire old serving processes.
	lock, _ := envs.ReadFile("envs/mlx-lm.txt") // this fixed file is embedded with the runtime
	data := append([]byte(d.sandboxID), pythonruntime.Bytes()...)
	sum := sha256.Sum256(append(data, lock...))
	return hex.EncodeToString(sum[:])
}

func (d *Driver) endpoint(w protocol.Desired, env string, st state) process.Spec {
	env = resolved(env)
	run := d.runDir(st)
	spec := d.command(w.ID, env, st, filepath.Join(env, "bin", "python"), "-m", "tensorafi_ml.native",
		"--source", w.ModelURI, "--model-dir", filepath.Join(d.dir(w.ID), "data", "model"),
		"--socket", d.socket(st), "--status", filepath.Join(run, "status.json"),
		"--max-bytes", strconv.FormatInt(w.MemoryMiB*1024*1024, 10))
	spec.Env = append(spec.Env, "PYTHONPATH="+filepath.Join(env, "tensorafi-runtime.zip"),
		"TENSORAFI_NATIVE_TOKEN="+st.Token, "HF_HOME="+filepath.Join(run, "hf"), "HF_HUB_DISABLE_TELEMETRY=1",
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
		case protocol.CodeModelRejected, protocol.CodeImage, protocol.CodeDriverProblem:
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
