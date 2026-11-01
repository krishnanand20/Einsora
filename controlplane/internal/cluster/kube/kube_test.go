package kube

import (
	"testing"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/krishnanand20/tensorafi/controlplane/internal/catalog"
	"github.com/krishnanand20/tensorafi/controlplane/internal/cluster"
	"github.com/krishnanand20/tensorafi/controlplane/internal/cluster/manifests"
	"github.com/krishnanand20/tensorafi/controlplane/internal/domain"
	"github.com/krishnanand20/tensorafi/controlplane/internal/testutil"
)

// Every kind a builder can emit must be in the allowlist, and must survive client-go's
// unstructured deep copy (which panics on non-JSON Go types).
func TestBuildersOnlyEmitAllowedKinds(t *testing.T) {
	cat := testutil.Catalog(t)
	gpu, _ := cat.GPUClass("l40s-48gb")
	objs := manifests.Tenant("acme", map[string]int{"nvidia.com/gpu": 1}, "tensorafi-images")
	for kind, imageID := range map[domain.WorkloadKind]string{
		domain.KindVM: "ubuntu-24.04-cuda", domain.KindNotebook: "pytorch-notebook", domain.KindEndpoint: "openai-endpoint",
	} {
		img, _ := cat.Image(imageID)
		runtime, _ := img.RuntimeFor(gpu.Accelerator)
		w := domain.Workload{ID: uuid.New(), Kind: kind, GPUClass: gpu.ID, GPUCount: 1, Params: map[string]string{
			domain.ParamSSHPublicKey: testutil.SSHKey, domain.ParamModelURI: "s3://models/x",
		}}
		built, err := manifests.Workload(w, manifests.Context{TenantSlug: "acme", Tier: domain.TierUntrusted, GPU: gpu, Image: img, Runtime: runtime}, true)
		if err != nil {
			t.Fatal(err)
		}
		objs = append(objs, built...)
	}
	for _, o := range objs {
		u := &unstructured.Unstructured{Object: runtime.DeepCopyJSON(o)}
		if _, ok := allowed[u.GroupVersionKind()]; !ok {
			t.Errorf("builder emits %s, which Apply would refuse", u.GroupVersionKind())
		}
	}
}

func TestNodeCapacity(t *testing.T) {
	cat := testutil.Catalog(t)
	l40s, _ := cat.GPUClass("l40s-48gb")
	a100, _ := cat.GPUClass("a100-80gb")
	node := func(labels map[string]string, alloc corev1.ResourceList) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Status: corev1.NodeStatus{Allocatable: alloc}}
	}
	containerL40S := node(l40s.NodeSelector, corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("4")})
	passthroughL40S := node(nil, corev1.ResourceList{corev1.ResourceName(l40s.VMDeviceName): resource.MustParse("8")})

	tests := []struct {
		name string
		node *corev1.Node
		gc   string
		want int
	}{
		{"container node, matching class", containerL40S, "l40s", 4},
		{"container node, other class sharing nvidia.com/gpu", containerL40S, "a100", 0},
		{"passthrough node", passthroughL40S, "l40s", 8},
		{"passthrough node, other class", passthroughL40S, "a100", 0},
	}
	classes := map[string]catalog.GPUClass{"l40s": l40s, "a100": a100}
	for _, tt := range tests {
		if got := nodeCapacity(tt.node, classes[tt.gc]); got != tt.want {
			t.Errorf("%s: got %d, want %d", tt.name, got, tt.want)
		}
	}
}

func obj(fields map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: fields}
}

func TestObserve(t *testing.T) {
	tests := []struct {
		name  string
		judge func(*unstructured.Unstructured) (cluster.Observed, string)
		obj   map[string]any
		want  cluster.Observed
	}{
		{"vm running", observeVM, map[string]any{"status": map[string]any{"printableStatus": "Running"}}, cluster.ObservedReady},
		{"vm stopped", observeVM, map[string]any{"status": map[string]any{"printableStatus": "Stopped"}}, cluster.ObservedStopped},
		{"vm unschedulable", observeVM, map[string]any{"status": map[string]any{"printableStatus": "ErrorUnschedulable"}}, cluster.ObservedProgressing},
		{"sts ready", observeStatefulSet, map[string]any{"spec": map[string]any{"replicas": int64(1)}, "status": map[string]any{"readyReplicas": int64(1)}}, cluster.ObservedReady},
		{"sts scaled down", observeStatefulSet, map[string]any{"spec": map[string]any{"replicas": int64(0)}}, cluster.ObservedStopped},
		{"sts starting", observeStatefulSet, map[string]any{"spec": map[string]any{"replicas": int64(1)}}, cluster.ObservedProgressing},
		{"isvc ready", observeInferenceService, map[string]any{"status": map[string]any{"conditions": []any{
			map[string]any{"type": "PredictorReady", "status": "True"},
			map[string]any{"type": "Ready", "status": "True"},
		}}}, cluster.ObservedReady},
		{"isvc loading", observeInferenceService, map[string]any{"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": "False", "message": "downloading"},
		}}}, cluster.ObservedProgressing},
	}
	for _, tt := range tests {
		if got, _ := tt.judge(obj(tt.obj)); got != tt.want {
			t.Errorf("%s: got %s, want %s", tt.name, got, tt.want)
		}
	}
}

// Reasons reach users, so runtime free text must never pass through (ADR-0015).
func TestObserveNeverPassesRuntimeTextThrough(t *testing.T) {
	leaky := `Revision "w-1-predictor-00001" failed: 0/12 nodes are available: 9 Insufficient nvidia.com/gpu; ` +
		`pull from 10.0.0.5:5000 failed; storageUri s3://AKIA:secret@bucket/m`
	tests := []struct {
		name  string
		judge func(*unstructured.Unstructured) (cluster.Observed, string)
		obj   map[string]any
		want  string
	}{
		{"isvc message dropped, reason kept", observeInferenceService, map[string]any{"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": "False", "reason": "RevisionFailed", "message": leaky},
		}}}, "model server not ready (RevisionFailed)"},
		{"isvc free-text reason dropped", observeInferenceService, map[string]any{"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": "False", "reason": leaky, "message": leaky},
		}}}, "model server not ready"},
		{"vm known status", observeVM, map[string]any{"status": map[string]any{"printableStatus": "ErrorUnschedulable"}}, "no machine can fit the VM right now"},
		{"vm unknown word", observeVM, map[string]any{"status": map[string]any{"printableStatus": "Migrating"}}, "VM status: Migrating"},
		{"vm free text", observeVM, map[string]any{"status": map[string]any{"printableStatus": leaky}}, "waiting for the VM's status"},
	}
	for _, tt := range tests {
		if _, got := tt.judge(obj(tt.obj)); got != tt.want {
			t.Errorf("%s: reason = %q, want %q", tt.name, got, tt.want)
		}
	}
}
