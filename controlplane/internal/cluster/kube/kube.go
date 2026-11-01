// Package kube is the Kubernetes cluster.Backend (the Datacenter profile): client-go with
// server-side apply. It turns specs into objects with the pure builders in cluster/manifests.
package kube

import (
	"context"
	"fmt"
	"regexp"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/krishnanand20/einsora/controlplane/internal/catalog"
	"github.com/krishnanand20/einsora/controlplane/internal/cluster"
	"github.com/krishnanand20/einsora/controlplane/internal/cluster/manifests"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
)

type kindInfo struct {
	resource   string
	namespaced bool
}

// allowed is every kind the controller may create. Apply refuses anything else, so a bug in
// a builder can't create, say, a ClusterRoleBinding. Keep in sync with deploy/charts/einsora RBAC.
var allowed = map[schema.GroupVersionKind]kindInfo{
	{Version: "v1", Kind: "Namespace"}:                                         {"namespaces", false},
	{Version: "v1", Kind: "Service"}:                                           {"services", true},
	{Version: "v1", Kind: "ResourceQuota"}:                                     {"resourcequotas", true},
	{Group: "apps", Version: "v1", Kind: "StatefulSet"}:                        {"statefulsets", true},
	{Group: "networking.k8s.io", Version: "v1", Kind: "NetworkPolicy"}:         {"networkpolicies", true},
	{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"}:           {"ciliumnetworkpolicies", true},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding"}:   {"rolebindings", true},
	{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine"}:              {"virtualmachines", true},
	{Group: "serving.kserve.io", Version: "v1beta1", Kind: "InferenceService"}: {"inferenceservices", true},
}

var (
	vmGVR   = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines"}
	stsGVR  = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
	isvcGVR = schema.GroupVersionResource{Group: "serving.kserve.io", Version: "v1beta1", Resource: "inferenceservices"}
	svcGVR  = schema.GroupVersionResource{Version: "v1", Resource: "services"}
	pvcGVR  = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}

	// cleanup is what Delete removes, primary objects first.
	cleanup = []schema.GroupVersionResource{vmGVR, isvcGVR, stsGVR, svcGVR, pvcGVR}
)

// Options are cluster settings the manifests need (EINSORA_STORAGE_CLASS, EINSORA_IMAGES_NAMESPACE,
// EINSORA_UNTRUSTED_RUNTIME_CLASS).
type Options struct {
	StorageClass          string
	ImagesNamespace       string
	UntrustedRuntimeClass string
}

type Backend struct {
	dyn     dynamic.Interface
	core    kubernetes.Interface
	catalog *catalog.Catalog
	opts    Options
}

// New uses in-cluster config, falling back to KUBECONFIG / ~/.kube/config.
func New(cat *catalog.Catalog, opts Options) (*Backend, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(), nil).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("kubernetes config: %w", err)
		}
	}
	// client-go's default (5 requests a second) would make a pass over many workloads take minutes.
	if cfg.QPS == 0 {
		cfg.QPS, cfg.Burst = 50, 100
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	core, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Backend{dyn: dyn, core: core, catalog: cat, opts: opts}, nil
}

// Run applies the tenant's namespace-level objects and the workload's objects.
func (b *Backend) Run(ctx context.Context, s cluster.Spec) error {
	objs, err := manifests.Workload(s.Workload, b.context(s), true)
	if err != nil {
		return err
	}
	tenant := manifests.Tenant(s.TenantSlug, QuotaResources(b.catalog, s.Quotas), b.opts.ImagesNamespace)
	return b.apply(ctx, append(tenant, objs...))
}

// Park applies the workload's parked form: VMs halted, notebooks scaled to zero, disks kept.
func (b *Backend) Park(ctx context.Context, s cluster.Spec) error {
	objs, err := manifests.Workload(s.Workload, b.context(s), false)
	if err != nil {
		return err
	}
	return b.apply(ctx, objs)
}

func (b *Backend) context(s cluster.Spec) manifests.Context {
	return manifests.Context{
		TenantSlug:            s.TenantSlug,
		Tier:                  s.Tier,
		GPU:                   s.Class,
		Image:                 s.Image,
		Runtime:               s.Runtime,
		StorageClass:          b.opts.StorageClass,
		ImagesNamespace:       b.opts.ImagesNamespace,
		UntrustedRuntimeClass: b.opts.UntrustedRuntimeClass,
	}
}

// QuotaResources turns per-class quotas into ResourceQuota limits on the extended resources those
// classes consume. Classes sharing nvidia.com/gpu are summed (it's a backstop only).
func QuotaResources(cat *catalog.Catalog, quotas []domain.Quota) map[string]int {
	out := map[string]int{}
	for _, q := range quotas {
		gc, ok := cat.GPUClass(q.GPUClass)
		if !ok || gc.ContainerResource == "" { // Apple and CPU classes don't run on Kubernetes
			continue
		}
		out[gc.ContainerResource] += q.MaxGPUs
		if gc.VMDeviceName != "" {
			out[gc.VMDeviceName] += q.MaxGPUs
		}
	}
	return out
}

func (b *Backend) apply(ctx context.Context, objs []cluster.Manifest) error {
	for _, m := range objs {
		u := &unstructured.Unstructured{Object: m}
		gvk := u.GroupVersionKind()
		info, ok := allowed[gvk]
		if !ok {
			return fmt.Errorf("refusing to apply %s: kind not in allowlist", gvk)
		}
		res := b.dyn.Resource(gvk.GroupVersion().WithResource(info.resource))
		var ri dynamic.ResourceInterface = res
		if info.namespaced {
			ri = res.Namespace(u.GetNamespace())
		}
		_, err := ri.Apply(ctx, u.GetName(), u, metav1.ApplyOptions{FieldManager: cluster.FieldManager, Force: true})
		if err != nil {
			return fmt.Errorf("apply %s %s/%s: %w", gvk.Kind, u.GetNamespace(), u.GetName(), err)
		}
	}
	return nil
}

func (b *Backend) Observe(ctx context.Context, ref cluster.Ref) (cluster.Observed, string, error) {
	ns, id, kind := cluster.TenantNamespace(ref.TenantSlug), ref.ID, ref.Kind
	var gvr schema.GroupVersionResource
	var judge func(*unstructured.Unstructured) (cluster.Observed, string)
	switch kind {
	case domain.KindVM:
		gvr, judge = vmGVR, observeVM
	case domain.KindNotebook:
		gvr, judge = stsGVR, observeStatefulSet
	case domain.KindEndpoint:
		gvr, judge = isvcGVR, observeInferenceService
	default:
		return "", "", fmt.Errorf("unknown workload kind %q", kind)
	}
	u, err := b.dyn.Resource(gvr).Namespace(ns).Get(ctx, cluster.ObjectName(id), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return cluster.ObservedAbsent, "", nil
	}
	if err != nil {
		return "", "", err
	}
	if u.GetDeletionTimestamp() != nil {
		return cluster.ObservedProgressing, "deleting", nil
	}
	o, reason := judge(u)
	return o, reason, nil
}

func (b *Backend) Delete(ctx context.Context, ref cluster.Ref) error {
	ns := cluster.TenantNamespace(ref.TenantSlug)
	sel := metav1.ListOptions{LabelSelector: cluster.LabelWorkloadID + "=" + ref.ID.String()}
	background := metav1.DeletePropagationBackground
	for _, gvr := range cleanup {
		ri := b.dyn.Resource(gvr).Namespace(ns)
		list, err := ri.List(ctx, sel)
		if apierrors.IsNotFound(err) {
			continue // CRD not installed
		}
		if err != nil {
			return fmt.Errorf("list %s: %w", gvr.Resource, err)
		}
		for _, item := range list.Items {
			err := ri.Delete(ctx, item.GetName(), metav1.DeleteOptions{PropagationPolicy: &background})
			if err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete %s %s/%s: %w", gvr.Resource, ns, item.GetName(), err)
			}
		}
	}
	return nil
}

// Capacity is schedulable GPUs per class across Ready, uncordoned nodes; Kubernetes picks the node.
func (b *Backend) Capacity(ctx context.Context) (cluster.Capacity, error) {
	nodes, err := b.core.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return cluster.Capacity{}, fmt.Errorf("list nodes: %w", err)
	}
	out := map[string]int{}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Spec.Unschedulable || !nodeReady(n) {
			continue
		}
		for _, gc := range b.catalog.GPUClasses {
			out[gc.ID] += nodeCapacity(n, gc)
		}
	}
	return cluster.Capacity{Classes: out}, nil
}

// ---- pure helpers (unit-tested)

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// nodeCapacity counts a node's GPUs of one class. Passthrough GPUs are identified by their
// device resource; container GPUs share nvidia.com/gpu, so the node selector tells classes apart.
func nodeCapacity(n *corev1.Node, gc catalog.GPUClass) int {
	total := 0
	if gc.VMDeviceName != "" {
		total += allocatable(n, gc.VMDeviceName)
	}
	if len(gc.NodeSelector) > 0 && labelsMatch(n.Labels, gc.NodeSelector) {
		total += allocatable(n, gc.ContainerResource)
	}
	return total
}

func allocatable(n *corev1.Node, resource string) int {
	q, ok := n.Status.Allocatable[corev1.ResourceName(resource)]
	if !ok {
		return 0
	}
	return int(q.Value())
}

func labelsMatch(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// Observe's reason is shown to the workload's owner (ADR-0015), so it comes from a fixed vocabulary.
// Raw runtime messages can name internal objects and nodes, describe other tenants' use of the
// cluster, or echo credentials from a model URI. Operators read those with kubectl.

// statusWord is a runtime status or condition reason that is a single enum-like word.
var statusWord = regexp.MustCompile(`^[A-Za-z]{1,64}$`)

var vmStatusReasons = map[string]string{
	"Provisioning":            "preparing the VM's disk",
	"WaitingForVolumeBinding": "waiting for the VM's disk",
	"Starting":                "starting the VM",
	"ErrorUnschedulable":      "no machine can fit the VM right now",
	"ErrImagePull":            "the VM image couldn't be downloaded",
	"ImagePullBackOff":        "the VM image couldn't be downloaded",
	"CrashLoopBackOff":        "the VM keeps crashing while starting",
	"DataVolumeError":         "the VM's disk couldn't be prepared",
	"ErrorPvcNotFound":        "the VM's disk is missing",
	"Stopping":                "stopping the VM",
	"Terminating":             "shutting the VM down",
}

func observeVM(u *unstructured.Unstructured) (cluster.Observed, string) {
	status, _, _ := unstructured.NestedString(u.Object, "status", "printableStatus")
	switch status {
	case "Running":
		return cluster.ObservedReady, "running"
	case "Stopped":
		return cluster.ObservedStopped, "stopped"
	}
	// Errors such as ErrorUnschedulable may clear on their own; the controller's
	// provisioning timeout decides when to give up.
	if reason, ok := vmStatusReasons[status]; ok {
		return cluster.ObservedProgressing, reason
	}
	if statusWord.MatchString(status) {
		return cluster.ObservedProgressing, "VM status: " + status
	}
	return cluster.ObservedProgressing, "waiting for the VM's status"
}

func observeStatefulSet(u *unstructured.Unstructured) (cluster.Observed, string) {
	want, found, _ := unstructured.NestedInt64(u.Object, "spec", "replicas")
	if !found {
		want = 1
	}
	ready, _, _ := unstructured.NestedInt64(u.Object, "status", "readyReplicas")
	switch {
	case want == 0 && ready == 0:
		return cluster.ObservedStopped, "scaled to zero"
	case want > 0 && ready >= want:
		return cluster.ObservedReady, ""
	}
	return cluster.ObservedProgressing, fmt.Sprintf("%d/%d replicas ready", ready, want)
}

// observeInferenceService reports the Ready condition's reason, a single word such as
// RevisionMissing, and never its message, which is free text from KServe and Knative.
func observeInferenceService(u *unstructured.Unstructured) (cluster.Observed, string) {
	conditions, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conditions {
		cond, _ := c.(map[string]any)
		if cond["type"] != "Ready" {
			continue
		}
		if cond["status"] == "True" {
			return cluster.ObservedReady, ""
		}
		if reason, _ := cond["reason"].(string); statusWord.MatchString(reason) {
			return cluster.ObservedProgressing, "model server not ready (" + reason + ")"
		}
		return cluster.ObservedProgressing, "model server not ready"
	}
	return cluster.ObservedProgressing, "waiting for the model server's status"
}
