# 0013. Node agent and runtime drivers, with Apple Silicon Macs as first-class hosts

- **Status:** Accepted (in code: the `machines` table, catalog accelerators, Apple Silicon and CPU classes, a runtime per platform, the agent backend, the in-process agent with inventory, and placement with the trust matrix; the protocol is [ADR-0016](0016-agent-protocol-and-machine-placement.md). The `macos-native` and `docker` drivers are built, serving MLX, vLLM, and llama.cpp ([ADR-0022](0022-llama-cpp-endpoints.md)); VM drivers and agents joining over the network are Phase 2)
- **Date:** 2026-10-23
- **Amends:** ADR-0001, and AGENTS.md rules 2 and 6

## Context

Users want to install Tensora on their own computers, including Apple Silicon Macs, and use the
GPU in them. Kubernetes can't run natively on macOS. The Mac's GPU (Metal, with unified memory) is
only usable from native macOS processes or from macOS virtual machines, not from a Linux VM.
Personal Linux machines shouldn't need Kubernetes either.

## Decision

### The agent

`tensora agent` runs on every machine (Linux amd64 or arm64, and macOS on Apple Silicon).

- It **connects outbound** to the control plane over mTLS, so it works behind home routers and NAT with no inbound ports opened. Machines join with an enrolment token.
- It reports the machine's **inventory**: CPU, RAM, disk, accelerators (NVIDIA through NVML, Apple GPU cores and unified memory, AMD later), OS, and architecture.
- It runs workloads through **drivers**.
- On the control plane, a new **`agent` implementation of `cluster.Backend`** dispatches to agents, so rule 2 still holds. In Solo, the agent runs in-process.

### Drivers

| Driver | OS | Runs | Accelerator | Isolation |
|---|---|---|---|---|
| `docker` | Linux | Notebooks, endpoints (vLLM, llama.cpp), CPU jobs | NVIDIA via the container toolkit, or CPU | Weak with runc; strong with Kata or gVisor |
| `libvirt` | Linux | VMs (KVM) | Whole-GPU VFIO passthrough | Strong |
| `macos-native` | macOS | Notebooks (Jupyter with PyTorch MPS and MLX), endpoints (MLX and llama.cpp servers) | **Metal** (Apple GPU) | Weak (macOS sandbox) |
| `macos-vz` | macOS | VMs through Apple's Virtualization framework. macOS guests get paravirtualized Metal; Linux guests are CPU-only. | Metal in macOS guests | Strong |
| `macos-container` | macOS | Linux containers, each in its own lightweight VM (Apple's containerization) | CPU only | Strong |
| `kubernetes` | Linux clusters | Everything in the Datacenter profile (KubeVirt, KServe) | NVIDIA | Per ADR-0004 |

The `kubernetes` row is a control-plane backend, not an agent driver. It's listed here for comparison.

### Apple Silicon specifics

- **Endpoints on a Mac** use MLX (safetensors weights) or llama.cpp (GGUF weights). Both serve OpenAI-compatible APIs. vLLM stays the NVIDIA runtime.
- **Unified memory:** the GPU and CPU share RAM, and macOS caps how much the GPU may wire by default. The agent reports usable GPU memory and keeps headroom for the owner.
- **macOS VMs:** the macOS licence allows up to two macOS VMs per Mac, so the driver enforces that limit.
- **Metal has no hardware partitioning** (no MIG). An untrusted user gets the whole Mac GPU or none of it.
- **Intel Macs** aren't targeted. **Windows** support (through WSL2) is planned for later.

### Model formats (amends rule 6)

GGUF is allowed in addition to safetensors once `tensora-ml validate` checks GGUF headers. Both are
code-free formats, and GGUF chat templates are rendered by a sandboxed template engine. Pickle is still never allowed.

## Consequences

- New rule 11: code must build and run on linux/amd64, linux/arm64, and darwin/arm64. OS-specific code lives behind build tags in driver packages only.
- CI gains a macOS arm64 job.
- Which workloads may run where depends on isolation. The trust matrix in tenancy-and-isolation.md decides it.
- `tensora-ml burnin` needs an MPS (Metal) path, and the validator needs GGUF support.
- Catalog classes describe the accelerator kind (`nvidia`, `apple`, `amd`, `cpu`) and its memory, which is unified memory for Apple.

## Alternatives considered

- **Virtual Kubelet on the Mac:** keeps the Kubernetes model, but needs a full Kubernetes control plane just to use one laptop.
- **A Linux VM with k3s on the Mac:** has no access to the Mac's GPU.
- **A separate Mac-only product:** two products to maintain, and two APIs.
