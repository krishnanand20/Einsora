package agents

import (
	"slices"
	"testing"

	"github.com/krishnanand20/einsora/controlplane/internal/agent/protocol"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/testutil"
)

func TestDescribeDockerUsesContainerImageAndExactPlacedModels(t *testing.T) {
	h := &Hub{catalog: testutil.Catalog(t)}
	w := domain.Workload{ImageID: "openai-endpoint", GPUClass: "apple-silicon-32gb", PlacedClass: "l40s-48gb", GPUCount: 1, Driver: domain.DriverDocker}
	var d protocol.Desired
	if err := h.describe(&d, w); err != nil {
		t.Fatal(err)
	}
	if d.Runtime.Ref != "ghcr.io/your-org/einsora-vllm-endpoint:0.1.0" || d.Runtime.Server != "vllm" || d.Runtime.ModelFormat != "safetensors" || d.Accelerator != domain.AcceleratorNVIDIA || !slices.Contains(d.DeviceModels, "NVIDIA L40S") {
		t.Fatalf("incorrect Docker desired runtime: %+v", d)
	}
	w.Driver = domain.DriverSim
	if err := h.describe(&d, w); err != nil {
		t.Fatal(err)
	}
	if d.Runtime.Ref != "einsora-vllm" {
		t.Fatal("Docker image replaced the Kubernetes runtime identifier")
	}
}

func TestDescribeCPURuntimeForNativeAndDocker(t *testing.T) {
	h := &Hub{catalog: testutil.Catalog(t)}
	for driver, ref := range map[string]string{domain.DriverMacOSNative: "jupyterlab-pytorch-mps", domain.DriverDocker: "ghcr.io/your-org/einsora-pytorch-notebook:0.1.0"} {
		w := domain.Workload{ImageID: "pytorch-notebook", PlacedClass: "cpu-2c-8g", GPUCount: 1, Driver: driver}
		var d protocol.Desired
		if err := h.describe(&d, w); err != nil {
			t.Fatal(err)
		}
		if d.Runtime.Ref != ref || d.Devices != 0 || d.Accelerator != domain.AcceleratorCPU || d.VCPUs != 2 || d.MemoryMiB != 8*1024 {
			t.Fatalf("incorrect CPU desired state: %+v", d)
		}
	}
}
