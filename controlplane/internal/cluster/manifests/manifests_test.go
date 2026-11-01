package manifests

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/cluster"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/testutil"
)

func ctxFor(t *testing.T, tier domain.IsolationTier, imageID string) Context {
	t.Helper()
	cat := testutil.Catalog(t)
	gpu, _ := cat.GPUClass("l40s-48gb")
	img, ok := cat.Image(imageID)
	if !ok {
		t.Fatalf("image %s missing from catalog", imageID)
	}
	runtime, ok := img.RuntimeFor(gpu.Accelerator)
	if !ok {
		t.Fatalf("image %s has no runtime for %s", imageID, gpu.Accelerator)
	}
	return Context{
		TenantSlug: "acme", Tier: tier, GPU: gpu, Image: img, Runtime: runtime,
		StorageClass: "einsora-block", ImagesNamespace: "einsora-images", UntrustedRuntimeClass: "kata-test",
	}
}

func wl(kind domain.WorkloadKind, gpus int) domain.Workload {
	return domain.Workload{
		ID: uuid.New(), Kind: kind, GPUClass: "l40s-48gb", GPUCount: gpus,
		Params: map[string]string{
			domain.ParamSSHPublicKey: testutil.SSHKey,
			domain.ParamModelURI:     "hf://Qwen/Qwen2.5-0.5B-Instruct",
		},
	}
}

var imageFor = map[domain.WorkloadKind]string{
	domain.KindVM: "ubuntu-24.04-cuda", domain.KindNotebook: "pytorch-notebook", domain.KindEndpoint: "openai-endpoint",
}

// dig walks nested maps/slices: dig(m, "spec", "template", "containers", 0, "image").
func dig(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("at %q: not a map: %T", k, v)
			}
			v = m[k]
		case int:
			s, ok := v.([]any)
			if !ok || k >= len(s) {
				t.Fatalf("at [%d]: not a long enough slice: %T", k, v)
			}
			v = s[k]
		}
	}
	return v
}

func TestEveryBuilderEmitsUnstructuredSafeTypes(t *testing.T) {
	for kind, img := range imageFor {
		for _, tier := range []domain.IsolationTier{domain.TierTrusted, domain.TierUntrusted} {
			for _, running := range []bool{true, false} {
				objs, err := Workload(wl(kind, 2), ctxFor(t, tier, img), running)
				if kind == domain.KindEndpoint && !running {
					if err == nil {
						t.Error("endpoints must not have a parked form")
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				for _, o := range objs {
					if err := cluster.CheckJSONTypes(o); err != nil {
						t.Errorf("%s/%v: %v", kind, o["kind"], err)
					}
				}
			}
		}
	}
	for _, o := range Tenant("acme", map[string]int{"nvidia.com/gpu": 3}, "einsora-images") {
		if err := cluster.CheckJSONTypes(o); err != nil {
			t.Errorf("tenant %v: %v", o["kind"], err)
		}
	}
}

func TestVMPassesThroughRequestedGPUs(t *testing.T) {
	c := ctxFor(t, domain.TierUntrusted, "ubuntu-24.04-cuda")
	for _, running := range []bool{true, false} {
		objs, err := Workload(wl(domain.KindVM, 2), c, running)
		if err != nil {
			t.Fatal(err)
		}
		vm := objs[0]
		if got := dig(t, vm, "metadata", "namespace"); got != "t-acme" {
			t.Errorf("namespace = %v", got)
		}
		gpus := dig(t, vm, "spec", "template", "spec", "domain", "devices", "gpus").([]any)
		if len(gpus) != 2 || dig(t, gpus[1], "deviceName") != c.GPU.VMDeviceName {
			t.Errorf("gpus = %v", gpus)
		}
		want := map[bool]string{true: "Always", false: "Halted"}[running]
		if got := dig(t, vm, "spec", "runStrategy"); got != want {
			t.Errorf("running=%v: runStrategy = %v", running, got)
		}
	}
}

func TestCloudConfigCannotBeInjected(t *testing.T) {
	hostile := "ssh-ed25519 AAAA\"}\n#cloud-config\nruncmd: [rm -rf /]"
	out := cloudConfig(hostile)
	body, ok := strings.CutPrefix(out, "#cloud-config\n")
	if !ok || strings.Contains(body, "\n") {
		t.Fatalf("user-data must be one JSON line after the header: %q", out)
	}
	var parsed struct {
		Keys []string `json:"ssh_authorized_keys"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Keys) != 1 || parsed.Keys[0] != hostile {
		t.Fatalf("key was altered: %q", parsed.Keys)
	}
}

func TestPodsAreHardenedAndUntrustedOnesSandboxed(t *testing.T) {
	podSpecs := func(objs []cluster.Manifest) []map[string]any {
		var out []map[string]any
		for _, o := range objs {
			switch o["kind"] {
			case "StatefulSet":
				out = append(out, dig(t, o, "spec", "template", "spec").(map[string]any))
			case "InferenceService":
				out = append(out, dig(t, o, "spec", "predictor").(map[string]any))
			}
		}
		return out
	}
	for _, kind := range []domain.WorkloadKind{domain.KindNotebook, domain.KindEndpoint} {
		for _, tier := range []domain.IsolationTier{domain.TierTrusted, domain.TierUntrusted} {
			objs, err := Workload(wl(kind, 1), ctxFor(t, tier, imageFor[kind]), true)
			if err != nil {
				t.Fatal(err)
			}
			specs := podSpecs(objs)
			if len(specs) != 1 {
				t.Fatalf("%s: want one pod spec, got %d", kind, len(specs))
			}
			spec := specs[0]
			if spec["automountServiceAccountToken"] != false {
				t.Errorf("%s/%s: service account token must not be mounted", kind, tier)
			}
			rc, hasRC := spec["runtimeClassName"]
			if tier == domain.TierUntrusted && rc != "kata-test" {
				t.Errorf("%s/untrusted: runtimeClassName = %v, want sandbox runtime", kind, rc)
			}
			if tier == domain.TierTrusted && hasRC {
				t.Errorf("%s/trusted: unexpected runtimeClassName %v", kind, rc)
			}
		}
	}
}

func TestEndpointUsesSafeRuntime(t *testing.T) {
	objs, err := Workload(wl(domain.KindEndpoint, 2), ctxFor(t, domain.TierUntrusted, "openai-endpoint"), true)
	if err != nil {
		t.Fatal(err)
	}
	isvc := objs[0]
	if got := dig(t, isvc, "spec", "predictor", "model", "runtime"); got != "einsora-vllm" {
		t.Errorf("runtime = %v", got)
	}
	if got := dig(t, isvc, "spec", "predictor", "model", "args", 0); got != "--tensor-parallel-size=2" {
		t.Errorf("args = %v", got)
	}
	raw, _ := json.Marshal(isvc)
	if strings.Contains(strings.ToLower(string(raw)), "trust") {
		t.Error("endpoint manifest must never mention trust_remote_code")
	}
}

func TestTenantNetworkIsolation(t *testing.T) {
	objs := Tenant("acme", map[string]int{"nvidia.com/gpu": 3}, "einsora-images")
	var policy, quota cluster.Manifest
	for _, o := range objs {
		switch o["kind"] {
		case "NetworkPolicy":
			policy = o
		case "ResourceQuota":
			quota = o
		}
	}
	if got := dig(t, quota, "spec", "hard", "requests.nvidia.com/gpu"); got != "3" {
		t.Errorf("quota = %v", got)
	}
	types := dig(t, policy, "spec", "policyTypes").([]any)
	if !slices.Contains(types, any("Egress")) || !slices.Contains(types, any("Ingress")) {
		t.Errorf("policy must cover ingress and egress: %v", types)
	}
	var except []any
	for _, rule := range dig(t, policy, "spec", "egress").([]any) {
		to := dig(t, rule, "to", 0).(map[string]any)
		if block, ok := to["ipBlock"].(map[string]any); ok {
			except = block["except"].([]any)
		}
	}
	for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16"} {
		if !slices.Contains(except, any(cidr)) {
			t.Errorf("tenant egress must exclude %s", cidr)
		}
	}
}
