# GPUs: access, allocation, and sharing

## Accelerators Einsora supports

| Accelerator | Platforms | How workloads use it | Partitioning | Status |
|---|---|---|---|---|
| **NVIDIA** (datacenter and consumer) | Linux | CUDA in containers; VFIO passthrough to VMs | MIG on A100, H100 and newer; vGPU (licensed); time-slicing | Built (Datacenter, and the agent's `docker` driver; live Linux validation pending) |
| **Apple Silicon** (M-series GPU, unified memory) | macOS | Metal from native processes (MLX, llama.cpp, PyTorch MPS); paravirtualized Metal in macOS VMs | None | Built (native notebooks and endpoints); macOS VMs planned |
| **CPU** | All | Everything without a GPU; small-model inference with llama.cpp | — | Built (notebooks, and llama.cpp endpoints on Macs and in Docker) |
| **AMD** (ROCm) | Linux | Containers with ROCm | — | M5 |

## NVIDIA: the options

| Method | Isolation | Cards | Einsora policy |
|---|---|---|---|
| **Whole-GPU passthrough** (VFIO) | Strong: the VM owns the device | Almost any; consumer cards have caveats | Default for VMs |
| **Whole GPU to a container** | Good with Kata, weak with runc | Any | Default for notebooks and endpoints |
| **MIG** | Hardware-partitioned memory and SMs; a shared driver | A100, H100, H200, B200 and similar | M5, for small endpoints, behind Kata |
| **vGPU** | Strong (a mediated device per VM) | Datacenter cards; licensed by NVIDIA | Not planned |
| **Time-slicing** | **None** | Any | **Never between different users**; allowed for a single owner's own workloads |

### GPU Operator node modes (Datacenter)

The GPU Operator decides **per node** how all of its GPUs are used, through a label:

```
nvidia.com/gpu.workload.config=container        # NVIDIA driver on the host → nvidia.com/gpu for pods
nvidia.com/gpu.workload.config=vm-passthrough   # vfio-pci bound → nvidia.com/<DEVICE> for KubeVirt
```

- A node can't serve VMs and containers at the same time. With one node, test the two modes one after the other. Relabeling rebinds every GPU, so drain GPU workloads first.
- From two nodes onward, dedicate nodes to a mode.
- Capacity per GPU class counts passthrough nodes by `vm_device_name`, and container nodes by `container_resource` plus `node_selector`.

On a Lab's Linux machines, the agent's `docker` and `libvirt` drivers switch modes the same way:
a GPU is either given to containers or bound to vfio-pci for a VM, never both at once.

### Consumer NVIDIA cards (RTX 4090, 5090)

- Cheap per FLOP, with no MIG or vGPU. Passthrough generally works, but some boards have reset quirks, so test stop/start cycles.
- **At home (Solo and Lab):** a normal use of a GeForce card.
- **In a datacenter:** NVIDIA's GeForce driver licence has historically excluded datacenter deployment. Datacenter operators should read the current licence. The catalog's `internal_only` flag restricts a class to trusted tenants.

## Apple Silicon

- **Unified memory:** the GPU uses system RAM. macOS caps how much the GPU may wire by default (an administrator can raise it with a `sysctl`). The agent reports usable GPU memory and **keeps headroom for the owner**.
- **Frameworks:**
  - MLX (Apple's array framework, using safetensors weights)
  - llama.cpp with its Metal backend (GGUF weights)
  - PyTorch's MPS backend
  - Both the MLX and llama.cpp servers provide OpenAI-compatible APIs. vLLM is the NVIDIA runtime and isn't used on Macs.
- **VMs:** macOS guests get paravirtualized Metal through Apple's Virtualization framework. The macOS licence allows at most 2 macOS VMs per Mac. Linux guests get no GPU compute.
- **No partitioning:** Metal has nothing like MIG. On shared Macs, one untrusted user gets the whole GPU or none of it, and an owner's own workloads may share freely.
- **Capacity:** Apple classes are sized by unified memory (`apple-silicon-16gb` to `apple-silicon-128gb`), because memory decides which models fit; one unit is one Mac's whole GPU. They're offered to machines' owners only (rule 4).

## When GPUs are busy

Users may opt in to **placement fallback**: another GPU class, or CPU, after a wait they choose
([ADR-0010](../adr/0010-fallbacks-cpu-placement-and-inference-failover.md)). On a Mac, the natural
order is Metal GPU, then CPU. A fallback is **never** a GPU shared with a stranger.
