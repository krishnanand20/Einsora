package docker

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/krishnanand20/einsora/controlplane/internal/domain"
)

func TestContainerSpecHasOnlyPrivateMountsAndEnforcesLimits(t *testing.T) {
	w := desired()
	c := buildContainer("instance", "sha256:"+strings.Repeat("a", 64), "/state/workload", "/run/own", "/state/runtime.zip", "own-network", "1000:1000", "private-worker-token", w, nil, nil)
	if c.User != "1000:1000" || !c.HostConfig.ReadonlyRootfs || !c.HostConfig.Init || c.HostConfig.AutoRemove ||
		!slices.Contains(c.HostConfig.CapDrop, "ALL") || !slices.Contains(c.HostConfig.SecurityOpt, "no-new-privileges:true") {
		t.Fatal("container lacks its non-root, read-only, capability, or process controls")
	}
	if c.HostConfig.Memory != 1024*1024*1024 || c.HostConfig.MemorySwap != c.HostConfig.Memory || c.HostConfig.NanoCpus != 2_000_000_000 || c.HostConfig.PidsLimit != 512 {
		t.Fatal("resource limits do not match the allocation")
	}
	if c.HostConfig.Runtime != "runc" || c.HostConfig.NetworkMode != "own-network" || len(c.HostConfig.DeviceRequests) != 0 {
		t.Fatal("CPU container received GPU access or another network")
	}
	if len(c.HostConfig.Mounts) != 3 {
		t.Fatal("unexpected host mount")
	}
	for _, mount := range c.HostConfig.Mounts {
		if !strings.HasPrefix(mount.Source, "/state/workload/") && mount.Source != "/run/own" {
			t.Errorf("mount escapes workload: %s", mount.Source)
		}
	}
	if !slices.Contains(c.Env, "NVIDIA_VISIBLE_DEVICES=void") || !slices.Contains(c.Cmd, "--ServerApp.sock="+containerSocket) {
		t.Error("CPU isolation or private notebook socket missing")
	}
	raw, _ := json.Marshal(c)
	for _, forbidden := range []string{"PortBindings", "PublishAllPorts", "Privileged", "PidMode", "IpcMode", "docker.sock"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("unexpected host access field %s", forbidden)
		}
	}
}

func TestPhysicalDevicesAreMatchedAndNeverTimeSliced(t *testing.T) {
	w := desired()
	w.Accelerator, w.Devices, w.DeviceModels = domain.AcceleratorNVIDIA, 1, []string{"NVIDIA L40S"}
	devices := []Device{{ID: "wrong-model", Model: "NVIDIA A100", MemoryMiB: 80 * 1024},
		{ID: "busy", Model: "NVIDIA L40S", MemoryMiB: 46068}, {ID: "free", Model: "NVIDIA L40S", MemoryMiB: 46068}}
	ids, ok := selectDevices(w, devices, map[string]bool{"busy": true})
	if !ok || !slices.Equal(ids, []string{"free"}) {
		t.Errorf("incorrect physical GPU choice: %v %v", ids, ok)
	}
	c := buildContainer("instance", "image", "/state/workload", "/run/own", "bundle", "network", "1000:1000", "token", w, ids, nil)
	if len(c.HostConfig.DeviceRequests) != 1 || !slices.Equal(c.HostConfig.DeviceRequests[0].DeviceIDs, ids) || c.HostConfig.Runtime != "nvidia" {
		t.Fatal("GPU request did not preserve exact UUIDs")
	}
	w.Devices = 2
	if _, ok := selectDevices(w, devices, map[string]bool{"busy": true}); ok {
		t.Error("used an occupied, different GPU class")
	}
}

func TestInvalidImagesAllocationsAndGuestExecutionAreRefused(t *testing.T) {
	for _, ref := range []string{"image", "image:latest", "https://registry/image:1", "/host/path:1", "image@sha256:bad"} {
		w := desired()
		w.Runtime.Ref = ref
		if err := validateDesired(w, true); err == nil {
			t.Errorf("accepted unpinned/invalid image %q", ref)
		}
	}
	for _, ref := range []string{"registry.test:5000/repo/image:1.0", "image@sha256:" + strings.Repeat("a", 64)} {
		w := desired()
		w.Runtime.Ref = ref
		if err := validateDesired(w, false); err != nil {
			t.Errorf("valid image rejected: %v", err)
		}
	}
	w := desired()
	w.StrongIsolation = true
	if err := validateDesired(w, true); err == nil {
		t.Error("guest received weak isolation")
	}
	w = desired()
	w.Accelerator, w.Devices, w.DeviceModels = domain.AcceleratorNVIDIA, 1, []string{"NVIDIA L40S"}
	if err := validateDesired(w, false); err == nil {
		t.Error("GPU request silently became CPU when the toolkit was absent")
	}
	w = desired()
	w.Kind = domain.KindEndpoint
	w.Runtime.Server, w.Runtime.ModelFormat = "llama.cpp", "gguf"
	if err := validateDesired(w, true); err == nil {
		t.Error("unvalidated model format was accepted")
	}
}

func TestEndpointSpecKeepsPolicyReadOnlyAndCredentialsOutOfArguments(t *testing.T) {
	w := desired()
	w.Kind, w.Accelerator, w.Devices = domain.KindEndpoint, domain.AcceleratorNVIDIA, 1
	w.Runtime.Server, w.Runtime.ModelFormat, w.ModelURI = "vllm", "safetensors", "hf://org/model"
	c := buildContainer("instance", "image", "/state/workload", "/run/own", "/state/runtime.zip", "network", "1000:1000", "internal-key", w, []string{"gpu"}, []string{"HF_TOKEN=fetch-key"})
	if c.Entrypoint[len(c.Entrypoint)-1] != "einsora_ml.vllm_server" {
		t.Fatal("endpoint bypassed model-policy worker")
	}
	if strings.Contains(strings.Join(c.Cmd, " "), "internal-key") || strings.Contains(strings.Join(c.Cmd, " "), "fetch-key") {
		t.Fatal("credentials leaked to process arguments")
	}
	policy := c.HostConfig.Mounts[len(c.HostConfig.Mounts)-1]
	if policy.Target != "/opt/einsora/runtime.zip" || !policy.ReadOnly {
		t.Fatal("endpoint can overwrite the shared model policy")
	}
	if !slices.Contains(c.Env, "HF_TOKEN=fetch-key") {
		t.Fatal("explicit fetch credentials were lost")
	}
}
