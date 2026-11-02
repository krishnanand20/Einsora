// Package docker runs the machine owner's notebooks and model endpoints (vLLM with safetensors on
// NVIDIA, llama.cpp with GGUF on NVIDIA or CPU) through a local Docker Engine. Its shared-kernel
// isolation is never used for guests (ADR-0013, the trust matrix).
package docker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/krishnanand20/einsora/controlplane/internal/agent/protocol"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
)

const (
	instanceLabel   = "einsora.dev/agent-instance"
	workloadLabel   = "einsora.dev/workload-id"
	attemptLabel    = "einsora.dev/attempt"
	specLabel       = "einsora.dev/spec-sha256"
	runLabel        = "einsora.dev/run-id"
	containerSocket = "/run/einsora/server.sock"
	// llamaServer is llama.cpp's server in the endpoint image (images/containers/llama-endpoint).
	llamaServer = "/app/llama-server"
)

// Device is one physical GPU. IDs are stable NVIDIA UUIDs, never ordinal positions that can change
// after a reboot. Placement supplies the allowed model names and memory for the actual class.
type Device struct {
	ID        string
	Model     string
	MemoryMiB int64
}

type mount struct {
	Type        string
	Source      string
	Target      string
	ReadOnly    bool
	BindOptions *struct {
		Propagation  string
		NonRecursive bool
	} `json:",omitempty"`
}

type deviceRequest struct {
	Driver       string
	DeviceIDs    []string
	Capabilities [][]string
}

type hostConfig struct {
	Privileged      bool                       `json:",omitempty"`
	PidMode         string                     `json:",omitempty"`
	IpcMode         string                     `json:",omitempty"`
	PortBindings    map[string]json.RawMessage `json:",omitempty"`
	PublishAllPorts bool                       `json:",omitempty"`
	Runtime         string
	NetworkMode     string
	ReadonlyRootfs  bool
	CapDrop         []string
	SecurityOpt     []string
	PidsLimit       int64
	NanoCpus        int64
	Memory          int64
	MemorySwap      int64
	ShmSize         int64
	Init            bool
	AutoRemove      bool
	RestartPolicy   map[string]string
	Mounts          []mount
	Tmpfs           map[string]string
	DeviceRequests  []deviceRequest `json:",omitempty"`
}

// containerConfig is JSON data sent to Docker. No shell, published port, host namespace, host
// device, or daemon socket is used. The only writable host mounts belong to this workload.
type containerConfig struct {
	Image      string
	User       string
	WorkingDir string
	Entrypoint []string
	Cmd        []string
	Env        []string
	Labels     map[string]string
	HostConfig hostConfig
}

var imageName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@:-]*$`)

func pinnedImage(ref string) bool {
	if !imageName.MatchString(ref) || strings.Contains(ref, "..") || strings.Contains(ref, "//") {
		return false
	}
	if name, digest, ok := strings.Cut(ref, "@sha256:"); ok {
		_, err := hex.DecodeString(digest)
		return name != "" && len(digest) == 64 && err == nil
	}
	colon := strings.LastIndexByte(ref, ':')
	return colon > strings.LastIndexByte(ref, '/') && colon > 0 && ref[colon+1:] != "" && ref[colon+1:] != "latest" && !strings.Contains(ref, "@")
}

func validateDesired(w protocol.Desired, nvidia bool) error {
	switch {
	case w.StrongIsolation:
		return fmt.Errorf("docker's shared kernel is for the machine owner's own workloads only")
	case w.Kind != domain.KindNotebook && w.Kind != domain.KindEndpoint:
		return fmt.Errorf("docker runs notebooks and endpoints")
	case w.Attempt < 1 || w.ID == [16]byte{} || w.VCPUs < 1 || w.VCPUs > 4096 || w.MemoryMiB < 1 || w.MemoryMiB > 64<<20:
		return fmt.Errorf("invalid workload resources or attempt")
	case !pinnedImage(w.Runtime.Ref):
		return fmt.Errorf("docker needs a container image pinned by version or sha256 digest")
	case w.Accelerator == domain.AcceleratorCPU && w.Devices != 0:
		return fmt.Errorf("a CPU workload must request no GPUs")
	case w.Accelerator == domain.AcceleratorNVIDIA && (!nvidia || w.Devices < 1 || w.Devices > 64 || len(w.DeviceModels) == 0):
		return fmt.Errorf("NVIDIA workloads need the container toolkit and a physical GPU class")
	case w.Accelerator != domain.AcceleratorCPU && w.Accelerator != domain.AcceleratorNVIDIA:
		return fmt.Errorf("docker supports CPU and NVIDIA allocations only")
	case w.Kind == domain.KindEndpoint && !servesEndpoint(w):
		return fmt.Errorf("docker endpoints need vLLM with safetensors on NVIDIA, or llama.cpp with GGUF on NVIDIA or CPU")
	}
	return nil
}

// servesEndpoint reports whether w is an endpoint this driver serves. The accelerator and its
// devices are checked already.
func servesEndpoint(w protocol.Desired) bool {
	switch {
	case w.Runtime.Server == "vllm" && w.Runtime.ModelFormat == "safetensors":
		return w.Accelerator == domain.AcceleratorNVIDIA
	case w.Runtime.Server == "llama.cpp" && w.Runtime.ModelFormat == "gguf":
		return true
	}
	return false
}

func labels(instance string, w protocol.Desired) map[string]string {
	return map[string]string{instanceLabel: instance, workloadLabel: w.ID.String(), attemptLabel: strconv.FormatInt(w.Attempt, 10)}
}

// buildContainer is pure so host protection, exact device IDs, and resource limits are tested as
// data. The runtime is addressed by its resolved image ID, preventing a mutable tag changing mid-run.
func buildContainer(instance, imageID, dir, run, bundle, network, user, token string, w protocol.Desired, deviceIDs, credentials []string) containerConfig {
	mem := w.MemoryMiB * 1024 * 1024
	home := "/home/einsora"
	config := containerConfig{
		Image: imageID, User: user, WorkingDir: "/work", Labels: labels(instance, w),
		Env: []string{"HOME=" + home, "TMPDIR=/tmp", "XDG_CACHE_HOME=" + home + "/.cache",
			"NVIDIA_VISIBLE_DEVICES=void", "PYTHONDONTWRITEBYTECODE=1"},
		HostConfig: hostConfig{Runtime: "runc", NetworkMode: network, ReadonlyRootfs: true,
			CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"}, PidsLimit: 512,
			NanoCpus: int64(w.VCPUs) * 1_000_000_000, Memory: mem, MemorySwap: mem,
			ShmSize: min(mem/8, 1<<30), Init: true, RestartPolicy: map[string]string{"Name": "no"},
			Tmpfs: map[string]string{"/tmp": fmt.Sprintf("rw,nosuid,nodev,mode=1777,size=%d", min(mem/4, 1<<30))},
			Mounts: []mount{
				{Type: "bind", Source: filepath.Join(dir, "data"), Target: "/work"},
				{Type: "bind", Source: filepath.Join(dir, "home"), Target: home},
				{Type: "bind", Source: run, Target: "/run/einsora"},
			}},
	}
	config.Labels[runLabel] = filepath.Base(run)
	if w.Accelerator == domain.AcceleratorNVIDIA {
		config.HostConfig.Runtime = "nvidia"
		config.HostConfig.DeviceRequests = []deviceRequest{{Driver: "nvidia", DeviceIDs: slices.Clone(deviceIDs), Capabilities: [][]string{{"gpu"}}}}
		config.Env[3] = "NVIDIA_VISIBLE_DEVICES=" + strings.Join(deviceIDs, ",")
		config.Env = append(config.Env, "NVIDIA_DRIVER_CAPABILITIES=compute,utility")
	}
	if w.Kind == domain.KindNotebook {
		config.Entrypoint = []string{"jupyter", "lab"}
		config.Cmd = []string{"--no-browser", "--ServerApp.sock=" + containerSocket, "--ServerApp.sock_mode=0600",
			"--ServerApp.root_dir=/work", "--ServerApp.allow_remote_access=False", "--KernelManager.transport=ipc"}
		config.Env = append(config.Env, "JUPYTER_TOKEN="+token, "JUPYTER_RUNTIME_DIR=/run/einsora",
			"JUPYTER_CONFIG_DIR="+home+"/.jupyter", "IPYTHONDIR="+home+"/.ipython")
	} else {
		config.HostConfig.Mounts = append(config.HostConfig.Mounts, mount{Type: "bind", Source: bundle, Target: "/opt/einsora/runtime.zip", ReadOnly: true})
		config.Entrypoint = []string{"python3", "-m", "einsora_ml.vllm_server"}
		config.Cmd = []string{"--source", w.ModelURI, "--model-dir", "/work/model", "--socket", containerSocket,
			"--status", "/run/einsora/status.json", "--max-bytes", strconv.FormatInt(mem, 10), "--devices", strconv.Itoa(len(deviceIDs))}
		config.Env = append(config.Env, "PYTHONPATH=/opt/einsora/runtime.zip", "EINSORA_NATIVE_TOKEN="+token,
			"HF_HOME="+home+"/.cache/huggingface", "AWS_EC2_METADATA_DISABLED=true",
			"VLLM_CACHE_ROOT="+home+"/.cache/vllm", "CUDA_CACHE_PATH="+home+"/.cache/cuda", "VLLM_NO_USAGE_STATS=1", "DO_NOT_TRACK=1")
		if w.Runtime.Server == "llama.cpp" {
			// The worker validates the GGUF model, then becomes llama-server: on exactly the GPUs
			// in this container, or the CPU only, with a thread for each vCPU it may use.
			config.Entrypoint[2] = "einsora_ml.llama_server"
			config.Cmd = append(config.Cmd, "--server", llamaServer, "--accelerator", string(w.Accelerator), "--threads", strconv.Itoa(w.VCPUs))
			config.Env = slices.DeleteFunc(config.Env, func(e string) bool { return strings.HasPrefix(e, "VLLM_") })
		}
		config.Env = append(config.Env, credentials...)
	}
	raw, _ := json.Marshal(config)
	sum := sha256.Sum256(raw)
	config.Labels[specLabel] = hex.EncodeToString(sum[:])
	return config
}

func selectDevices(w protocol.Desired, devices []Device, used map[string]bool) ([]string, bool) {
	if w.Accelerator == domain.AcceleratorCPU {
		return nil, true
	}
	var ids []string
	for _, device := range devices {
		// Match the catalog's exact model names. VRAM is nominal: real L40S cards report
		// about 46068 MiB, below 48*1024, and still belong to the catalog's 48GB class.
		if !used[device.ID] && slices.ContainsFunc(w.DeviceModels, func(model string) bool { return strings.EqualFold(model, device.Model) }) {
			ids = append(ids, device.ID)
			if len(ids) == w.Devices {
				return ids, true
			}
		}
	}
	return nil, false
}
