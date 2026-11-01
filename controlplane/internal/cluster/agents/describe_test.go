package agents

import (
	"slices"
	"testing"

	"github.com/tensora/tensora/controlplane/internal/agent/protocol"
	"github.com/tensora/tensora/controlplane/internal/domain"
	"github.com/tensora/tensora/controlplane/internal/testutil"
)

func TestDescribeDockerUsesContainerImageAndExactPlacedModels(t *testing.T) {
	h := &Hub{catalog: testutil.Catalog(t)}
	w := domain.Workload{ImageID: "openai-endpoint", GPUClass: "apple-silicon-32gb", PlacedClass: "l40s-48gb", GPUCount: 1, Driver: domain.DriverDocker}
	var d protocol.Desired
	if err := h.describe(&d, w); err != nil {
		t.Fatal(err)
	}
	if d.Runtime.Ref != "ghcr.io/your-org/tensora-vllm-endpoint:0.1.0" || d.Runtime.Server != "vllm" || d.Runtime.ModelFormat != "safetensors" || d.Accelerator != domain.AcceleratorNVIDIA || !slices.Contains(d.DeviceModels, "NVIDIA L40S") {
		t.Fatalf("incorrect Docker desired runtime: %+v", d)
	}
	w.Driver = domain.DriverSim
	if err := h.describe(&d, w); err != nil {
		t.Fatal(err)
	}
	if d.Runtime.Ref != "tensora-vllm" {
		t.Fatal("Docker image replaced the Kubernetes runtime identifier")
	}
}
