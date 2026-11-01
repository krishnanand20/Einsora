# images

What users can launch.

| Path | What |
|---|---|
| `catalog.yaml` | The launch catalog for Solo and Lab (and `make dev`): regions, classes, and images. The control plane loads it, and its tests validate it. The Datacenter profile uses [`deploy/catalog.yaml`](../deploy/catalog.yaml), which offers only what Kubernetes can run. |
| `containers/pytorch-notebook/` | JupyterLab with PyTorch and CUDA (catalog id `pytorch-notebook`), for Linux |
| `vm/ubuntu-24.04-cuda/` | Datacenter VM base image, as a CDI DataSource (catalog id `ubuntu-24.04-cuda`) |

Endpoint images aren't built here. In a datacenter, the `openai-endpoint` catalog entry's vLLM runtime
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
- A runtime's `ref` is what the backend runs: a CDI DataSource, container image, or KServe ServingRuntime on Kubernetes, or a name the node agent's driver understands (`macos-native`: `jupyterlab-pytorch-mps`, an environment whose lock is embedded in the binary). The API shows each runtime's accelerators and server, never its ref.

## Adding an image

1. Build it: add a directory under `containers/` or `vm/`.
2. Pin the base image by digest for releases. Scan it, for example with `trivy image`.
3. Add it to `catalog.yaml` with the right `kind`, and a runtime (`accelerators`, `ref`, and `server` for endpoints) for each platform it supports.
4. Run `make test-cp`. The control plane tests load the catalog and fail on mistakes.

Rules for images in user workloads:

- Run as non-root (uid 1000).
- No baked-in credentials.
- No SSH server in container images.
- Expose the documented port only.
- Free to use and redistribute: no paid or licence-key software (rule 12).
