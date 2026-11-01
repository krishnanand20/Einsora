package catalog_test

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/tensora/tensora/controlplane/internal/catalog"
	"github.com/tensora/tensora/controlplane/internal/domain"
	"github.com/tensora/tensora/controlplane/internal/testutil"
)

// The real catalog: an endpoint is served by vLLM on NVIDIA, MLX on Apple Silicon, and llama.cpp
// on CPU (ADR-0013).
func TestEndpointRuntimePerAccelerator(t *testing.T) {
	cat := testutil.Catalog(t)
	img, ok := cat.Image("openai-endpoint")
	if !ok {
		t.Fatal("openai-endpoint missing from the catalog")
	}
	for accel, want := range map[domain.AcceleratorKind]string{
		domain.AcceleratorNVIDIA: "vllm", domain.AcceleratorApple: "mlx", domain.AcceleratorCPU: "llama.cpp", domain.AcceleratorAMD: "llama.cpp",
	} {
		if r, ok := img.RuntimeFor(accel); !ok || r.Server != want {
			t.Errorf("%s: runtime = %+v, want %s", accel, r, want)
		}
	}
	if cat.DefaultRegion() != domain.RegionLocal {
		t.Errorf("default region = %q, want local", cat.DefaultRegion())
	}
}

func TestValidate(t *testing.T) {
	valid := func() catalog.Catalog {
		return catalog.Catalog{
			Regions: []catalog.Region{{ID: "local"}, {ID: "eu-fra-1"}},
			GPUClasses: []catalog.GPUClass{
				{ID: "l40s", Accelerator: domain.AcceleratorNVIDIA, VRAMGB: 48, ContainerResource: "nvidia.com/gpu", VCPUsPerGPU: 8, MemoryGiBPerGPU: 64, Regions: []domain.Region{"eu-fra-1"}},
				{ID: "mac", Accelerator: domain.AcceleratorApple, VRAMGB: 32, VCPUsPerGPU: 8, MemoryGiBPerGPU: 32},
				{ID: "cpu", Accelerator: domain.AcceleratorCPU, VCPUsPerGPU: 4, MemoryGiBPerGPU: 16},
			},
			Images: []catalog.Image{{ID: "ep", Kind: domain.KindEndpoint, Runtimes: []catalog.Runtime{
				{Accelerators: []domain.AcceleratorKind{domain.AcceleratorNVIDIA}, Server: "vllm", ModelFormat: "safetensors", Ref: "tensora-vllm"},
			}}},
		}
	}
	c := valid()
	if err := c.Validate(); err != nil {
		t.Fatalf("valid catalog: %v", err)
	}
	tests := map[string]struct {
		mutate func(*catalog.Catalog)
		want   string
	}{
		"no regions":              {func(c *catalog.Catalog) { c.Regions = nil }, "at least one"},
		"bad region id":           {func(c *catalog.Catalog) { c.Regions[1].ID = "EU Frankfurt" }, "invalid or duplicate"},
		"class in unknown region": {func(c *catalog.Catalog) { c.GPUClasses[0].Regions = []domain.Region{"us-east-1"} }, "isn't in the catalog"},
		"no accelerator":          {func(c *catalog.Catalog) { c.GPUClasses[0].Accelerator = "" }, "accelerator must be"},
		"nvidia without resource": {func(c *catalog.Catalog) { c.GPUClasses[0].ContainerResource = "" }, "container_resource is required"},
		"apple on kubernetes":     {func(c *catalog.Catalog) { c.GPUClasses[1].ContainerResource = "nvidia.com/gpu" }, "only on node agents"},
		"gpu without memory":      {func(c *catalog.Catalog) { c.GPUClasses[1].VRAMGB = 0 }, "vram_gb must be positive"},
		"image without runtimes":  {func(c *catalog.Catalog) { c.Images[0].Runtimes = nil }, "at least one runtime"},
		"endpoint without server": {func(c *catalog.Catalog) { c.Images[0].Runtimes[0].Server = "" }, "name their model server"},
		"unknown accelerator":     {func(c *catalog.Catalog) { c.Images[0].Runtimes[0].Accelerators = []domain.AcceleratorKind{"tpu"} }, "unknown accelerator"},
		"endpoint without format": {func(c *catalog.Catalog) { c.Images[0].Runtimes[0].ModelFormat = "" }, "model_format must be"},
	}
	for name, tt := range tests {
		c := valid()
		tt.mutate(&c)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want mention of %q", name, err, tt.want)
		}
	}
}

// repoFile finds path relative to the repository root, above the test's working directory.
func repoFile(t *testing.T, path string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, path)); err == nil {
			return filepath.Join(dir, path)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("%s not found above the working directory", path)
		}
		dir = parent
	}
}

// The Datacenter catalog offers only what its Kubernetes backend can run, and lists every
// environment's region, so no workload is created somewhere no controller serves.
func TestDatacenterCatalog(t *testing.T) {
	cat, err := catalog.Load(repoFile(t, "deploy/catalog.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range cat.GPUClasses {
		if g.ContainerResource == "" {
			t.Errorf("class %s has no Kubernetes resource, so the Datacenter can't run it", g.ID)
		}
	}
	envs, err := filepath.Glob(filepath.Join(filepath.Dir(repoFile(t, "deploy/catalog.yaml")), "environments", "*.yaml"))
	if err != nil || len(envs) == 0 {
		t.Fatalf("environment files: %v, %v", envs, err)
	}
	for _, env := range envs {
		raw, err := os.ReadFile(env) //nolint:gosec // a path the test globbed from the repository
		if err != nil {
			t.Fatal(err)
		}
		var values struct {
			Region domain.Region `json:"region"`
		}
		if err := yaml.Unmarshal(raw, &values); err != nil {
			t.Fatalf("%s: %v", env, err)
		}
		if _, ok := cat.Region(values.Region); !ok {
			t.Errorf("%s: region %q isn't in deploy/catalog.yaml", filepath.Base(env), values.Region)
		}
	}
}

// The launch rules (rule 4, rule 6, ADR-0009), against the real catalog.
func TestCheckLaunch(t *testing.T) {
	cat := testutil.Catalog(t)
	class := func(id string) catalog.GPUClass {
		g, ok := cat.GPUClass(id)
		if !ok {
			t.Fatalf("class %s missing", id)
		}
		return g
	}
	image := func(id string) catalog.Image {
		img, ok := cat.Image(id)
		if !ok {
			t.Fatalf("image %s missing", id)
		}
		return img
	}
	macVM := catalog.Image{ID: "macos", Kind: domain.KindVM, Runtimes: []catalog.Runtime{
		{Accelerators: []domain.AcceleratorKind{domain.AcceleratorApple}, Ref: "macos-15"},
	}}
	tests := []struct {
		name   string
		tier   domain.IsolationTier
		class  catalog.GPUClass
		image  catalog.Image
		region domain.Region
		want   string // "" = allowed
		server string
	}{
		{"owner's notebook on a Mac", domain.TierTrusted, class("apple-silicon-32gb"), image("pytorch-notebook"), "local", "", ""},
		{"owner's endpoint on a Mac uses MLX", domain.TierTrusted, class("apple-silicon-32gb"), image("openai-endpoint"), "local", "", "mlx"},
		{"invited user's notebook on a Mac", domain.TierUntrusted, func() catalog.GPUClass {
			g := class("apple-silicon-32gb")
			g.InternalOnly = false // even if an operator offers Apple classes to everyone
			return g
		}(), image("pytorch-notebook"), "local", "only the machine's owner", ""},
		{"invited user's endpoint on a Mac", domain.TierUntrusted, func() catalog.GPUClass {
			g := class("apple-silicon-32gb")
			g.InternalOnly = false
			return g
		}(), image("openai-endpoint"), "local", "only the machine's owner", ""},
		{"invited user's macOS VM", domain.TierUntrusted, func() catalog.GPUClass {
			g := class("apple-silicon-32gb")
			g.InternalOnly = false
			return g
		}(), macVM, "local", "", ""},
		{"hidden class", domain.TierUntrusted, class("apple-silicon-32gb"), image("openai-endpoint"), "local", "isn't available", ""},
		{"CPU endpoint needs GGUF", domain.TierTrusted, class("cpu-4c-16g"), image("openai-endpoint"), "local", "aren't accepted until", ""},
		{"NVIDIA endpoint uses vLLM", domain.TierUntrusted, class("l40s-48gb"), image("openai-endpoint"), "local", "", "vllm"},
		{"unknown region", domain.TierTrusted, func() catalog.GPUClass {
			g := class("l40s-48gb")
			g.Regions = []domain.Region{"local"}
			return g
		}(), image("openai-endpoint"), "eu-fra-1", "isn't offered in region", ""},
	}
	for _, tt := range tests {
		runtime, err := catalog.CheckLaunch(tt.tier, tt.class, tt.image, tt.image.Kind, tt.region)
		switch {
		case tt.want == "" && err != nil:
			t.Errorf("%s: %v", tt.name, err)
		case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
			t.Errorf("%s: err = %v, want %q", tt.name, err, tt.want)
		case runtime.Server != tt.server:
			t.Errorf("%s: server = %q, want %q", tt.name, runtime.Server, tt.server)
		}
	}
}

// The workload's own kind is what's checked: an image whose kind changed in the catalog can't turn
// an untrusted notebook on a Mac into an allowed "VM".
func TestCheckLaunchUsesTheWorkloadsKind(t *testing.T) {
	g, _ := testutil.Catalog(t).GPUClass("apple-silicon-32gb")
	g.InternalOnly = false
	nowAVM := catalog.Image{ID: "pytorch-notebook", Kind: domain.KindVM, Runtimes: []catalog.Runtime{
		{Accelerators: []domain.AcceleratorKind{domain.AcceleratorApple}, Ref: "x"},
	}}
	if _, err := catalog.CheckLaunch(domain.TierUntrusted, g, nowAVM, domain.KindNotebook, "local"); err == nil ||
		!strings.Contains(err.Error(), "no longer a notebook image") {
		t.Fatalf("err = %v", err)
	}
}

// Agents' devices match classes: NVIDIA by model name, a Mac by the largest tier its unified
// memory holds. Nothing matches what the catalog doesn't list, or a region the class isn't in.
func TestDevicesOn(t *testing.T) {
	c := testutil.Catalog(t)
	gib := func(n int64) int64 { return n * 1024 }
	for name, tt := range map[string]struct {
		accelerators []domain.Accelerator
		want         map[string]int
	}{
		"two L40S": {[]domain.Accelerator{{Kind: domain.AcceleratorNVIDIA, Model: "NVIDIA L40S", Count: 2, MemoryMiB: 46068}},
			map[string]int{"l40s-48gb": 2}},
		"model names ignore case": {[]domain.Accelerator{{Kind: domain.AcceleratorNVIDIA, Model: "nvidia geforce rtx 4090", Count: 1}},
			map[string]int{"rtx4090-24gb": 1}},
		"an unlisted NVIDIA card": {[]domain.Accelerator{{Kind: domain.AcceleratorNVIDIA, Model: "NVIDIA T4", Count: 1}},
			map[string]int{}},
		"a 36 GB Mac is the 32 GB tier": {[]domain.Accelerator{{Kind: domain.AcceleratorApple, Model: "Apple M3 Pro", Count: 1, MemoryMiB: gib(36)}},
			map[string]int{"apple-silicon-32gb": 1}},
		"a 16 GB Mac": {[]domain.Accelerator{{Kind: domain.AcceleratorApple, Model: "Apple M2", Count: 1, MemoryMiB: gib(16)}},
			map[string]int{"apple-silicon-16gb": 1}},
		"an 8 GB Mac has no GPU tier": {[]domain.Accelerator{{Kind: domain.AcceleratorApple, Model: "Apple M1", Count: 1, MemoryMiB: gib(8)}},
			map[string]int{}},
	} {
		if got := c.DevicesOn(tt.accelerators, "local"); !maps.Equal(got, tt.want) {
			t.Errorf("%s: DevicesOn = %v, want %v", name, got, tt.want)
		}
	}
	limited := *c
	limited.GPUClasses = slices.Clone(c.GPUClasses)
	for i := range limited.GPUClasses {
		limited.GPUClasses[i].Regions = []domain.Region{"eu-fra-1"}
	}
	if got := limited.DevicesOn([]domain.Accelerator{{Kind: domain.AcceleratorNVIDIA, Model: "NVIDIA L40S", Count: 1}}, "local"); len(got) != 0 {
		t.Errorf("in a region without the class: %v", got)
	}
}

func TestModelsBelongToOneClass(t *testing.T) {
	c := testutil.Catalog(t)
	dup := *c
	dup.GPUClasses = slices.Clone(c.GPUClasses)
	for i, g := range dup.GPUClasses {
		if g.ID == "a100-80gb" {
			dup.GPUClasses[i].Models = []string{"nvidia l40s"}
		}
	}
	if err := dup.Validate(); err == nil || !strings.Contains(err.Error(), "already class") {
		t.Errorf("a model in two classes: %v", err)
	}
	apple := *c
	apple.GPUClasses = slices.Clone(c.GPUClasses)
	for i, g := range apple.GPUClasses {
		if g.Accelerator == domain.AcceleratorApple {
			apple.GPUClasses[i].Models = []string{"Apple M3"}
		}
	}
	if err := apple.Validate(); err == nil {
		t.Error("models on an Apple class were accepted")
	}
}

func TestRuntimeSelectionIncludesDriver(t *testing.T) {
	cat := testutil.Catalog(t)
	for _, tt := range []struct {
		image       string
		accel       domain.AcceleratorKind
		driver, ref string
	}{
		{"pytorch-notebook", domain.AcceleratorCPU, "", "ghcr.io/your-org/tensora-pytorch-notebook:0.1.0"},
		{"pytorch-notebook", domain.AcceleratorCPU, domain.DriverDocker, "ghcr.io/your-org/tensora-pytorch-notebook:0.1.0"},
		{"pytorch-notebook", domain.AcceleratorCPU, domain.DriverMacOSNative, "jupyterlab-pytorch-mps"},
		{"pytorch-notebook", domain.AcceleratorNVIDIA, domain.DriverMacOSNative, ""},
		{"pytorch-notebook", domain.AcceleratorCPU, domain.DriverMacOSContainer, ""},
		{"openai-endpoint", domain.AcceleratorNVIDIA, domain.DriverDocker, "ghcr.io/your-org/tensora-vllm-endpoint:0.1.0"},
		{"openai-endpoint", domain.AcceleratorNVIDIA, "", "tensora-vllm"},
	} {
		t.Run(tt.image+"/"+string(tt.accel)+"/"+tt.driver, func(t *testing.T) {
			img, _ := cat.Image(tt.image)
			r, ok := img.RuntimeForDriver(tt.accel, tt.driver)
			if ok != (tt.ref != "") || ok && r.Ref != tt.ref {
				t.Fatalf("got %+v, %v; want %q", r, ok, tt.ref)
			}
		})
	}
}

func TestDriverCandidatesCannotBypassModelPolicy(t *testing.T) {
	img := catalog.Image{Kind: domain.KindEndpoint, Runtimes: []catalog.Runtime{
		{Accelerators: []domain.AcceleratorKind{domain.AcceleratorApple}, Ref: "mlx-lm", Server: "mlx", ModelFormat: "safetensors", DriverRefs: map[string]string{domain.DriverMacOSNative: "mlx-lm"}},
		{Accelerators: []domain.AcceleratorKind{domain.AcceleratorApple}, Ref: "llama-server", Server: "llama.cpp", ModelFormat: "gguf", DriverRefs: map[string]string{domain.DriverMacOSVZ: "llama-server"}},
	}}
	if _, ok := img.LaunchRuntime(domain.AcceleratorApple, domain.DriverMacOSNative); !ok {
		t.Fatal("validated MLX runtime unavailable")
	}
	// An agent candidate with an unvalidated format must fail even though the default is safe.
	img.Runtimes[0].DriverRefs = map[string]string{domain.DriverDocker: "unrelated"}
	img.Runtimes[1].DriverRefs = map[string]string{domain.DriverMacOSNative: "llama-server"}
	if r, ok := img.LaunchRuntime(domain.AcceleratorApple, domain.DriverMacOSNative); ok || r.ModelFormat != "gguf" {
		t.Fatal("driver selection bypassed GGUF validation")
	}
}

func TestLegacyDockerContainerRefStillResolves(t *testing.T) {
	img := catalog.Image{Kind: domain.KindEndpoint, Runtimes: []catalog.Runtime{{Accelerators: []domain.AcceleratorKind{domain.AcceleratorNVIDIA}, Ref: "tensora-vllm", ContainerRef: "registry.test/vllm:1", ModelFormat: "safetensors"}}}
	if r, ok := img.RuntimeForDriver(domain.AcceleratorNVIDIA, domain.DriverDocker); !ok || r.Ref != "registry.test/vllm:1" {
		t.Fatal("legacy Docker catalog stopped working")
	}
	if r, _ := img.RuntimeFor(domain.AcceleratorNVIDIA); r.Ref != "tensora-vllm" {
		t.Fatal("agent selection changed the default runtime")
	}
}

func TestDriverRefValidation(t *testing.T) {
	for _, refs := range []map[string]string{{"typo": "env"}, {domain.DriverMacOSNative: " "}, {domain.DriverLibvirt: "env"}} {
		cat := *testutil.Catalog(t)
		cat.Images = append([]catalog.Image(nil), cat.Images...)
		img := cat.Images[1]
		img.Runtimes = append([]catalog.Runtime(nil), img.Runtimes...)
		img.Runtimes[0].DriverRefs = refs
		cat.Images[1] = img
		if err := cat.Validate(); err == nil {
			t.Errorf("accepted invalid driver refs: %v", refs)
		}
	}
}
