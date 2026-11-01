// Package manifests builds the Kubernetes objects for tenants and workloads. Every function
// is pure — data in, data out — so the whole cluster contract is unit-testable.
package manifests

import (
	"fmt"
	"strconv"

	"github.com/krishnanand20/tensorafi/controlplane/internal/catalog"
	"github.com/krishnanand20/tensorafi/controlplane/internal/cluster"
	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
)

// Context is everything a builder needs beyond the workload itself.
type Context struct {
	TenantSlug            string
	Tier                  domain.IsolationTier
	GPU                   catalog.GPUClass
	Image                 catalog.Image
	Runtime               catalog.Runtime // the image's runtime for GPU's accelerator; Ref is what runs
	StorageClass          string
	ImagesNamespace       string
	UntrustedRuntimeClass string
}

// Workload returns the cluster objects for w. running=false yields the parked form (VM
// halted, notebook scaled to zero) that keeps disks but frees GPUs. Endpoints have no
// parked form; delete them instead.
func Workload(w domain.Workload, c Context, running bool) ([]cluster.Manifest, error) {
	switch w.Kind {
	case domain.KindVM:
		if c.GPU.VMDeviceName == "" {
			return nil, fmt.Errorf("gpu class %s has no vm_device_name", c.GPU.ID)
		}
		return []cluster.Manifest{virtualMachine(w, c, running)}, nil
	case domain.KindNotebook:
		return notebook(w, c, running), nil
	case domain.KindEndpoint:
		if !running {
			return nil, fmt.Errorf("endpoints have no parked form; delete them instead")
		}
		return []cluster.Manifest{inferenceService(w, c)}, nil
	}
	return nil, fmt.Errorf("unknown workload kind %q", w.Kind)
}

func labels(w domain.Workload, c Context) map[string]any {
	return map[string]any{
		cluster.LabelManagedBy:    cluster.ManagedBy,
		cluster.LabelTenant:       c.TenantSlug,
		cluster.LabelWorkloadID:   w.ID.String(),
		cluster.LabelWorkloadKind: string(w.Kind),
		cluster.LabelGPUClass:     w.GPUClass,
	}
}

func annotations(w domain.Workload, running bool) map[string]any {
	return map[string]any{
		cluster.AnnotationGPUCount:       strconv.Itoa(w.GPUCount),
		cluster.AnnotationDesiredRunning: strconv.FormatBool(running),
	}
}

func metadata(name, namespace string, labels, annotations map[string]any) map[string]any {
	m := map[string]any{"name": name, "labels": labels}
	if namespace != "" {
		m["namespace"] = namespace
	}
	if len(annotations) > 0 {
		m["annotations"] = annotations
	}
	return m
}

func stringMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func anyList(ss ...string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// gpuResources sizes CPU and memory by GPU count. Requests equal limits (Guaranteed QoS)
// so a tenant can't be squeezed by noisy neighbours on the same node.
func gpuResources(c Context, gpus int) map[string]any {
	cpu := strconv.Itoa(c.GPU.VCPUsPerGPU * gpus)
	mem := fmt.Sprintf("%dGi", c.GPU.MemoryGiBPerGPU*gpus)
	return map[string]any{
		"limits":   map[string]any{c.GPU.ContainerResource: strconv.Itoa(gpus), "cpu": cpu, "memory": mem},
		"requests": map[string]any{c.GPU.ContainerResource: strconv.Itoa(gpus), "cpu": cpu, "memory": mem},
	}
}

// hardenPod applies the pod-level controls every tenant pod gets (tenancy-and-isolation.md).
func hardenPod(c Context, spec map[string]any) map[string]any {
	spec["automountServiceAccountToken"] = false
	spec["enableServiceLinks"] = false
	if len(c.GPU.NodeSelector) > 0 {
		spec["nodeSelector"] = stringMap(c.GPU.NodeSelector)
	}
	if c.Tier != domain.TierTrusted {
		spec["runtimeClassName"] = c.UntrustedRuntimeClass
	}
	return spec
}

func containerSecurityContext() map[string]any {
	return map[string]any{
		"allowPrivilegeEscalation": false,
		"capabilities":             map[string]any{"drop": anyList("ALL")},
		"seccompProfile":           map[string]any{"type": "RuntimeDefault"},
	}
}
