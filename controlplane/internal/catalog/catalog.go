// Package catalog loads images/catalog.yaml: the regions, classes, and images users can launch.
package catalog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/tensora/tensora/controlplane/internal/domain"
)

const defaultRootDiskGiB = 100

// Region is a location this installation offers (ADR-0009). The catalog's first region is the
// default for workloads that don't name one.
type Region struct {
	ID          domain.Region `json:"id"`
	DisplayName string        `json:"display_name"`
}

// GPUClass is something a workload runs on: a kind of GPU, an Apple Silicon GPU, or a CPU-only
// size. The name is historical; CPU classes are classes too (ADR-0010).
type GPUClass struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	// Accelerator is the kind of device a workload of this class gets (ADR-0013).
	Accelerator domain.AcceleratorKind `json:"accelerator"`
	// VRAMGB is memory per device: VRAM, or unified memory for Apple. Zero for CPU classes.
	VRAMGB int `json:"vram_gb"`
	// Regions offering this class. Empty means every region in the catalog.
	Regions []domain.Region `json:"regions,omitempty"`
	// ContainerResource is the extended resource Kubernetes pods request (nvidia.com/gpu).
	// Required for NVIDIA and AMD classes; Apple and CPU classes run on node agents.
	ContainerResource string `json:"container_resource,omitempty"`
	// VMDeviceName is the KubeVirt host-device resource for passthrough. Empty = no VMs.
	VMDeviceName string `json:"vm_device_name,omitempty"`
	// NodeSelector picks container-mode nodes carrying this GPU (GPU Feature Discovery labels).
	NodeSelector map[string]string `json:"node_selector,omitempty"`
	// Models are the device names node agents report for this class (NVIDIA and AMD), such as
	// "NVIDIA L40S". A device model belongs to at most one class. Without it, agents never offer
	// the class. Apple classes match by unified memory instead (DevicesOn).
	Models []string `json:"models,omitempty"`
	// VCPUsPerGPU and MemoryGiBPerGPU size each unit: one GPU, or one CPU-class unit.
	VCPUsPerGPU     int `json:"vcpus_per_gpu"`
	MemoryGiBPerGPU int `json:"memory_gib_per_gpu"`
	// InternalOnly hides the class from untrusted tenants: consumer GPUs (see gpu-sharing.md), and
	// Apple classes, whose native macOS execution is for machines' owners only (rule 4).
	InternalOnly bool `json:"internal_only,omitempty"`
}

// VisibleTo reports whether tenants in tier may see and use this class.
func (g GPUClass) VisibleTo(tier domain.IsolationTier) bool {
	return !g.InternalOnly || tier == domain.TierTrusted
}

// OfferedIn reports whether the class is offered in region r.
func (g GPUClass) OfferedIn(r domain.Region) bool {
	return len(g.Regions) == 0 || slices.Contains(g.Regions, r)
}

// Runtime is how an image runs on some kinds of accelerator (ADR-0013): for an endpoint, vLLM on
// NVIDIA, MLX on Apple Silicon, or llama.cpp on anything.
type Runtime struct {
	Accelerators []domain.AcceleratorKind `json:"accelerators"`
	// Server names an endpoint's model server (vllm, mlx, llama.cpp), so users can see what
	// serves them (rule 10). Empty for other kinds.
	Server string `json:"server,omitempty"`
	// ModelFormat is the weight format an endpoint's server loads: safetensors or gguf. A format is
	// accepted only once tensora-ml validate checks it (rule 6).
	ModelFormat string `json:"model_format,omitempty"`
	// Ref is what the backend runs. Kubernetes: a CDI DataSource (vm), a container image
	// (notebook), or a KServe ClusterServingRuntime (endpoint). Node agents: interpreted by their
	// driver (roadmap step 7).
	Ref string `json:"ref"`
	// DriverRefs maps supported agent drivers to their runtime refs. When nonempty, an agent
	// driver must be listed; Ref remains the Kubernetes/default runtime. Never exposed by the API.
	DriverRefs map[string]string `json:"driver_refs,omitempty"`
	// ContainerRef is the pinned container image when an endpoint's Ref names a Kubernetes runtime.
	// It is operator configuration and is never exposed by the API.
	ContainerRef string `json:"container_ref,omitempty"`
}

type Image struct {
	ID          string              `json:"id"`
	DisplayName string              `json:"display_name"`
	Kind        domain.WorkloadKind `json:"kind"`
	Description string              `json:"description,omitempty"`
	RootDiskGiB int                 `json:"root_disk_gib,omitempty"`
	// Runtimes are tried in order; the first that supports the class's accelerator is used.
	Runtimes []Runtime `json:"runtimes"`
}

// RuntimeFor returns the runtime the image uses on accelerator a.
func (img Image) RuntimeFor(a domain.AcceleratorKind) (Runtime, bool) {
	return img.RuntimeForDriver(a, "")
}

// RuntimeForDriver selects by accelerator and driver, preserving runtime preference order.
// Empty driver (Kubernetes/default) and sim use Ref. Legacy catalogs without DriverRefs retain
// their original agent behavior, including ContainerRef for Docker endpoints.
func (img Image) RuntimeForDriver(a domain.AcceleratorKind, driver string) (Runtime, bool) {
	if driver != "" && driver != domain.DriverSim {
		spec, ok := domain.LookupDriver(driver)
		if !ok || !slices.Contains(spec.Kinds, img.Kind) || !slices.Contains(spec.Accelerators, a) {
			return Runtime{}, false
		}
	}
	for _, r := range img.Runtimes {
		if !slices.Contains(r.Accelerators, a) {
			continue
		}
		if driver != "" && driver != domain.DriverSim {
			if ref, ok := r.DriverRefs[driver]; ok {
				r.Ref = ref
			} else if len(r.DriverRefs) > 0 {
				continue
			} else if driver == domain.DriverDocker && r.ContainerRef != "" {
				r.Ref = r.ContainerRef
			}
		}
		return r, true
	}
	return Runtime{}, false
}

type Catalog struct {
	Regions    []Region   `json:"regions"`
	GPUClasses []GPUClass `json:"gpu_classes"`
	Images     []Image    `json:"images"`
}

// Load reads and validates a catalog file. Unknown fields are errors.
func Load(path string) (*Catalog, error) {
	// The path comes from operator config (TENSORA_CATALOG_PATH), never from a request.
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("read catalog: %w", err)
	}
	var c Catalog
	if err := yaml.UnmarshalStrict(raw, &c); err != nil {
		return nil, fmt.Errorf("parse catalog %s: %w", path, err)
	}
	for i := range c.Images {
		if c.Images[i].Kind == domain.KindVM && c.Images[i].RootDiskGiB == 0 {
			c.Images[i].RootDiskGiB = defaultRootDiskGiB
		}
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid catalog %s: %w", path, err)
	}
	return &c, nil
}

func (c *Catalog) Validate() error {
	var errs []error
	regions := map[domain.Region]bool{}
	for _, r := range c.Regions {
		if !r.ID.Valid() || regions[r.ID] {
			errs = append(errs, fmt.Errorf("region %q: invalid or duplicate id", r.ID))
		}
		regions[r.ID] = true
	}
	if len(c.Regions) == 0 {
		errs = append(errs, errors.New("regions: list at least one (a Solo install has one, local)"))
	}
	seen, models := map[string]bool{}, map[string]string{}
	for _, g := range c.GPUClasses {
		errs = append(errs, validateClass(g, seen, regions))
		seen[g.ID] = true
		for _, m := range g.Models {
			key := strings.ToLower(m)
			if other, dup := models[key]; dup {
				errs = append(errs, fmt.Errorf("class %q: model %q is already class %q's; a device matches one class", g.ID, m, other))
			}
			models[key] = g.ID
		}
	}
	seen = map[string]bool{}
	for _, img := range c.Images {
		errs = append(errs, validateImage(img, seen))
		seen[img.ID] = true
	}
	return errors.Join(errs...)
}

func validateClass(g GPUClass, seen map[string]bool, regions map[domain.Region]bool) error {
	kubernetes := g.ContainerResource != "" || g.VMDeviceName != "" || len(g.NodeSelector) > 0
	switch {
	case g.ID == "" || seen[g.ID]:
		return fmt.Errorf("class %q: missing or duplicate id", g.ID)
	case !g.Accelerator.Valid():
		return fmt.Errorf("class %q: accelerator must be nvidia, apple, amd, or cpu, got %q", g.ID, g.Accelerator)
	case (g.Accelerator == domain.AcceleratorNVIDIA || g.Accelerator == domain.AcceleratorAMD) && g.ContainerResource == "":
		return fmt.Errorf("class %q: container_resource is required for %s GPUs", g.ID, g.Accelerator)
	case g.Accelerator == domain.AcceleratorApple && kubernetes:
		return fmt.Errorf("class %q: Apple classes run only on node agents, so they take no Kubernetes fields", g.ID)
	case len(g.Models) > 0 && g.Accelerator != domain.AcceleratorNVIDIA && g.Accelerator != domain.AcceleratorAMD:
		return fmt.Errorf("class %q: models are for NVIDIA and AMD classes", g.ID)
	case g.Accelerator != domain.AcceleratorCPU && g.VRAMGB <= 0:
		return fmt.Errorf("class %q: vram_gb must be positive for a GPU", g.ID)
	case g.VCPUsPerGPU <= 0 || g.MemoryGiBPerGPU <= 0:
		return fmt.Errorf("class %q: vcpus_per_gpu and memory_gib_per_gpu must be positive", g.ID)
	}
	for _, r := range g.Regions {
		if !regions[r] {
			return fmt.Errorf("class %q: region %q isn't in the catalog's regions", g.ID, r)
		}
	}
	return nil
}

func validateImage(img Image, seen map[string]bool) error {
	switch {
	case img.ID == "" || seen[img.ID]:
		return fmt.Errorf("image %q: missing or duplicate id", img.ID)
	case !img.Kind.Valid():
		return fmt.Errorf("image %q: invalid kind %q", img.ID, img.Kind)
	case len(img.Runtimes) == 0:
		return fmt.Errorf("image %q: list at least one runtime", img.ID)
	}
	for i, r := range img.Runtimes {
		switch {
		case r.Ref == "":
			return fmt.Errorf("image %q runtime %d: ref is required", img.ID, i)
		case len(r.Accelerators) == 0:
			return fmt.Errorf("image %q runtime %d: list the accelerators it runs on", img.ID, i)
		case img.Kind == domain.KindEndpoint && r.Server == "":
			return fmt.Errorf("image %q runtime %d: endpoints name their model server", img.ID, i)
		case img.Kind == domain.KindEndpoint && r.ModelFormat != "safetensors" && r.ModelFormat != "gguf":
			return fmt.Errorf("image %q runtime %d: model_format must be safetensors or gguf", img.ID, i)
		}
		for _, a := range r.Accelerators {
			if !a.Valid() {
				return fmt.Errorf("image %q runtime %d: unknown accelerator %q", img.ID, i, a)
			}
		}
		for driver, ref := range r.DriverRefs {
			spec, ok := domain.LookupDriver(driver)
			if !ok || driver == domain.DriverSim || strings.TrimSpace(ref) == "" || !slices.Contains(spec.Kinds, img.Kind) {
				return fmt.Errorf("image %q runtime %d: invalid driver ref for %q", img.ID, i, driver)
			}
			if !slices.ContainsFunc(r.Accelerators, func(a domain.AcceleratorKind) bool { return slices.Contains(spec.Accelerators, a) }) {
				return fmt.Errorf("image %q runtime %d: driver %q supports none of its accelerators", img.ID, i, driver)
			}
			if driver == domain.DriverDocker && r.ContainerRef != "" && r.ContainerRef != ref {
				return fmt.Errorf("image %q runtime %d: conflicting Docker refs", img.ID, i)
			}
		}
	}
	return nil
}

// validatedFormats are the model formats tensora-ml validate checks (rule 6). GGUF joins when the
// validator checks GGUF headers (roadmap step 10); until then, runtimes that load it can't be used.
var validatedFormats = map[string]bool{"safetensors": true}

// LaunchRuntime selects the actual driver runtime and enforces the model-format gate again after
// placement. A later driver's candidate cannot bypass validation of the default candidate.
func (img Image) LaunchRuntime(a domain.AcceleratorKind, driver string) (Runtime, bool) {
	r, ok := img.RuntimeForDriver(a, driver)
	return r, ok && (img.Kind != domain.KindEndpoint || validatedFormats[r.ModelFormat])
}

// CheckLaunch decides whether a tenant in tier may run a workload of kind with img on class in
// region, and returns the runtime it would use. It's checked at create, at start, and again at
// admission, so a catalog or tier that changed in between can't be bypassed: kind is the
// workload's own, which must still match the image's. Errors are safe to show the workload's owner.
func CheckLaunch(tier domain.IsolationTier, class GPUClass, img Image, kind domain.WorkloadKind, region domain.Region) (Runtime, error) {
	invalid := func(format string, args ...any) (Runtime, error) {
		return Runtime{}, domain.Errorf(domain.CodeInvalid, format, args...)
	}
	switch {
	case img.Kind != kind:
		return invalid("image %q is no longer a %s image", img.ID, kind)
	case !class.VisibleTo(tier):
		return invalid("gpu_class %q isn't available to this tenant", class.ID)
	case !class.OfferedIn(region):
		return invalid("gpu_class %q isn't offered in region %q", class.ID, region)
	case img.Kind == domain.KindVM && class.VMDeviceName == "" && class.Accelerator != domain.AcceleratorApple:
		return invalid("gpu_class %q cannot be attached to VMs", class.ID)
	}
	runtime, ok := img.RuntimeFor(class.Accelerator)
	if !ok {
		return invalid("image %q can't run on gpu_class %q (%s)", img.ID, class.ID, class.Accelerator)
	}
	// Rule 4 and the trust matrix (tenancy-and-isolation.md): on Apple Silicon, notebooks and
	// endpoints run natively on macOS, where the only isolation is the macOS sandbox. That's for
	// machines' owners only; invited and community users get macOS VMs there.
	if class.Accelerator == domain.AcceleratorApple && img.Kind != domain.KindVM && tier != domain.TierTrusted {
		return invalid("on Apple Silicon, %ss run natively on macOS, which only the machine's owner may do; use a VM", img.Kind)
	}
	if img.Kind == domain.KindEndpoint && !validatedFormats[runtime.ModelFormat] {
		return invalid("on %s, endpoints are served by %s, which loads %s models; those aren't accepted until tensora-ml validate checks them (rule 6)",
			class.Accelerator, runtime.Server, runtime.ModelFormat)
	}
	return runtime, nil
}

// DevicesOn reports how many devices of each GPU class a machine's accelerators provide, among the
// classes offered in region (ADR-0016). An NVIDIA or AMD device matches the class listing its model;
// a Mac's GPU matches the largest Apple class whose vram_gb fits in its unified memory. CPU classes
// need no devices, so they aren't listed.
func (c *Catalog) DevicesOn(accelerators []domain.Accelerator, region domain.Region) map[string]int {
	out := map[string]int{}
	for _, a := range accelerators {
		switch a.Kind {
		case domain.AcceleratorNVIDIA, domain.AcceleratorAMD:
			for _, g := range c.GPUClasses {
				if g.Accelerator == a.Kind && g.OfferedIn(region) && slices.ContainsFunc(g.Models, func(m string) bool {
					return strings.EqualFold(m, a.Model)
				}) {
					out[g.ID] += a.Count
				}
			}
		case domain.AcceleratorApple:
			best := GPUClass{}
			for _, g := range c.GPUClasses {
				if g.Accelerator == domain.AcceleratorApple && g.OfferedIn(region) &&
					int64(g.VRAMGB)*1024 <= a.MemoryMiB && g.VRAMGB > best.VRAMGB {
					best = g
				}
			}
			if best.ID != "" {
				out[best.ID] += a.Count
			}
		}
	}
	return out
}

// DefaultRegion is where workloads go when they don't name a region: the first one listed.
func (c *Catalog) DefaultRegion() domain.Region { return c.Regions[0].ID }

func (c *Catalog) Region(id domain.Region) (Region, bool) {
	for _, r := range c.Regions {
		if r.ID == id {
			return r, true
		}
	}
	return Region{}, false
}

func (c *Catalog) GPUClass(id string) (GPUClass, bool) {
	for _, g := range c.GPUClasses {
		if g.ID == id {
			return g, true
		}
	}
	return GPUClass{}, false
}

func (c *Catalog) Image(id string) (Image, bool) {
	for _, img := range c.Images {
		if img.ID == id {
			return img, true
		}
	}
	return Image{}, false
}
