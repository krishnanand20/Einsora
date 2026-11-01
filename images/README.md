# images

What users can launch.

| Path | What |
|---|---|
| `catalog.yaml` | The launch catalog for Solo and Lab (and `make dev`): regions, classes, and images. The control plane loads it, and its tests validate it. The Datacenter profile uses [`deploy/catalog.yaml`](../deploy/catalog.yaml), which offers only what Kubernetes can run. |
| `containers/pytorch-notebook/` | JupyterLab with PyTorch and CUDA (catalog id `pytorch-notebook`), for Linux |
| `containers/vllm-endpoint/` | NVIDIA vLLM endpoint with the driver-supplied safetensors validation worker |
| `vm/ubuntu-24.04-cuda/` | Datacenter VM base image, as a CDI DataSource (catalog id `ubuntu-24.04-cuda`) |

For Linux Solo, build the vLLM image below. In a datacenter, the `openai-endpoint` catalog entry's vLLM runtime
points to the `tensora-vllm` ClusterServingRuntime in `deploy/charts/cluster-config`, which pins an
upstream vLLM image.

## How the catalog is shaped (ADR-0009, 0010, 0013)

- **`regions:`** lists the locations this installation offers. The first is the default; Solo has one, `local`.
- **Classes** have an `accelerator` (`nvidia`, `apple`, `amd`, `cpu`) and, optionally, the `regions` offering them (none listed means every region). NVIDIA and AMD classes carry the Kubernetes resource names; Apple and CPU classes run on node agents.
- **Apple Silicon classes** are sized by unified memory (`apple-silicon-16gb` to `apple-silicon-128gb`), because memory decides which models fit; one unit is the whole GPU of one Mac. They're for machines' owners only (rule 4). **CPU classes** are sized by vCPUs and memory (`cpu-4c-16g`, `cpu-16c-64g`).
- **Images list runtimes in order of preference**, and a workload uses the first that supports its class's accelerator:
  - endpoints (`openai-endpoint`): vLLM on NVIDIA, MLX on Apple, llama.cpp on anything, including CPU
  - notebooks (`pytorch-notebook`): the CUDA container on NVIDIA and CPU, and a uv-managed environment with PyTorch (MPS) on Macs. That environment's lock file, with hashes, is `controlplane/internal/agent/drivers/macosnative/envs/jupyterlab-pytorch-mps.txt`, embedded in the binary; its header says how to regenerate it.
  - VMs (`ubuntu-24.04-cuda`): NVIDIA passthrough today; macOS guests (Virtualization framework) arrive with the `macos-vz` driver
- An endpoint runtime names its `server` and the `model_format` it loads. Only formats `tensora-ml validate` checks are accepted (rule 6): safetensors today, GGUF from roadmap step 10, so llama.cpp endpoints wait until then.
- A runtime's `ref` is what the backend runs: a CDI DataSource, container image, or KServe ServingRuntime on Kubernetes, or a name the node agent's driver understands (`macos-native`: `jupyterlab-pytorch-mps`, an environment whose lock is embedded in the binary). An optional `container_ref` supplies the pinned Docker image when `ref` names a KServe runtime. The API shows accelerators and server, never either ref.

## Adding an image

1. Build it: add a directory under `containers/` or `vm/`.
2. Pin the base image by digest for releases. Scan it, for example with `trivy image`.
3. Add it to `catalog.yaml` with the right `kind`, and a runtime (`accelerators`, `ref`, and `server` for endpoints) for each platform it supports.
4. Run `make test-cp`. The control plane tests load the catalog and fail on mistakes.

Rules for images in user workloads:

- Run as non-root (uid 1000).
- No baked-in credentials.
- No SSH server in container images.
- Expose the documented port only for Datacenter workloads; Solo uses private Unix sockets and publishes no ports.
- Free to use and redistribute: no paid or licence-key software (rule 12).

## Build Linux Solo images

From the repository root, on an already configured Linux Docker host:

```bash
docker build -f images/containers/pytorch-notebook/Dockerfile -t ghcr.io/your-org/tensora-pytorch-notebook:0.1.0 .
docker build -f images/containers/vllm-endpoint/Dockerfile -t ghcr.io/your-org/tensora-vllm-endpoint:0.1.0 .
```

These are local tags matching `images/catalog.yaml`, not published images. Change the catalog refs
if you tag them differently; keep explicit versions or sha256 digests. The vLLM base is pinned to
v0.11.0 for Unix socket support, independently of Datacenter's image. Its extra pinned `boto3`
dependency supports the existing S3 ingestion path. The Go binary supplies its embedded Python
policy archive read-only; regenerate it with `python3 ml/scripts/bundle_native.py` after ML source
changes. Container commands and auth are set by the driver, overriding image defaults.

CPU notebooks use the same notebook image with runc and no GPU device request. NVIDIA workloads
require the NVIDIA container runtime and physical GPU UUID discovery. CPU model endpoints remain
blocked until GGUF validation and llama.cpp are implemented. Live Linux/CUDA validation is pending.
