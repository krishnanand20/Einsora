# Hardware guide

Einsora runs on hardware you already have. This page helps you choose machines, from a single Mac to a datacenter.

## Personal machines (Solo and Lab)

| Machine | Good for | Notes |
|---|---|---|
| **Apple Silicon Mac, 16–32 GB** (MacBook Air or Pro, Mac mini) | Notebooks; small model endpoints (up to ~8B parameters, quantized) | Metal GPU; very low power; laptops share only on AC power |
| **Apple Silicon Mac, 64–192 GB+** (Max or Ultra chips, Mac Studio) | Larger models (30–70B, quantized) on one machine | Unified memory is the big advantage: the GPU can use most of the RAM |
| **Desktop with an NVIDIA RTX card** (12–32 GB VRAM) | CUDA notebooks, vLLM endpoints, VMs with GPU passthrough | Passthrough needs a second GPU or integrated graphics for the host display |
| **Any 64-bit machine without a GPU** (old laptops and desktops) | CPU instances, small llama.cpp models, fallback capacity | Every machine adds capacity |

**Rough model sizing** (weights only; add room for context):

| Precision | Memory per billion parameters | 8B model | 70B model |
|---|---|---|---|
| 4-bit | ~0.5–0.6 GB | ~5 GB | ~40 GB |
| 8-bit | ~1 GB | ~8 GB | ~70 GB |
| 16-bit | ~2 GB | ~16 GB | ~140 GB |

## Lab or small company

- A small **always-on machine for the control plane** (a Mac mini or a small Linux box), with SQLite, or Postgres if you have many users.
- Wired gigabit Ethernet between machines in one place. Machines elsewhere connect over the internet, because agents dial out.
- A small UPS for the control plane machine, so a power blip doesn't corrupt its store.

## Datacenter

### Node tiers

| Tier | GPUs | Use |
|---|---|---|
| **Starter node** | 4× L40S 48GB PCIe in a 2U/4U server | Inference endpoints, notebooks, fine-tuning, GPU VMs |
| **Large-model node** | 4–8× A100 80GB or H100 PCIe | Bigger models, more memory per GPU, MIG |
| **Training node** | HGX 8× H100/H200 SXM with NVLink | Multi-GPU training. Needs a training fabric and about 10 kW per node. |

L40S is a pragmatic start: 48 GB per card, strong inference performance, PCIe, and supported by passthrough
and the GPU Operator. It has no MIG. If small fractional GPUs matter, choose A100 or H100.

### GPU node: what to specify

| Component | Spec | Why |
|---|---|---|
| Chassis | 2U or 4U GPU server validated for 4× double-width passive GPUs | Passive datacenter cards need server airflow |
| CPU | 1–2× AMD EPYC (9004/9005) or Xeon Scalable, with ≥ 64 PCIe 5.0 lanes free for GPUs and NICs | x16 lanes per GPU; IOMMU group separation |
| RAM | ≥ 2× total VRAM: **384 GB minimum** for 4× 48 GB; 512 GB–1 TB preferred | Model loading, VM memory, page cache |
| OS disks | 2× 480–960 GB NVMe or SATA SSD, RAID 1 | Survives a boot disk failure |
| Data disks | 2–4× 3.84–7.68 TB NVMe (U.2 or U.3, PLP) | Longhorn or Ceph, model cache, VM disks |
| Network | 2× 25 GbE (SFP28) minimum; 100 GbE for Ceph | Model pulls and storage replication |
| Management | Dedicated BMC/IPMI port (Redfish) | Remote power, console, sensors |
| Power supplies | Redundant 2+2 or 1+1, Titanium or Platinum, sized for the GPU TDP sum × 1.3 | GPU power spikes; a PSU can fail |
| BIOS | VT-d/AMD-Vi, SR-IOV, Above-4G decoding, ACS (if offered) | Needed for passthrough (runbook 01) |

### CPU nodes

| Role | Spec | Why |
|---|---|---|
| **Control-plane servers** (×3 per region) | 1U, 8–16 cores, 64 GB RAM, 2× NVMe (RAID 1), 2× 10/25 GbE | k3s servers, Postgres, API, controller. A GPU node failure must never take down the platform. |
| **CPU worker nodes** | 1U/2U, 64–128 cores, 256–512 GB RAM, NVMe | CPU instances, CPU fallback (ADR-0010), data prep |

### Shared infrastructure

| Item | Spec |
|---|---|
| Top-of-rack switch | 25/100 GbE, 2 ports per node plus uplinks, with VLANs and LACP |
| Management switch | 1 GbE for BMCs, on a separate VLAN with no internet |
| Router and firewall | Static public IPs, dual ISP, WireGuard endpoint |
| UPS and power | See [power-and-cooling.md](power-and-cooling.md) |
| Rack | Metered PDUs, A and B feeds |

### Questions for vendors

1. Is the chassis validated (thermally and with BIOS) for your chosen GPUs?
2. Does each GPU land in its own IOMMU group? Ask for `lspci -tv` from a reference unit.
3. What's the GPU lead time, and the warranty and repair turnaround in your country?
4. Is a BMC with Redfish and a remote console included?
5. What's the measured power draw at full GPU load?

### Purchase tracking

| Date | Region | Vendor | Config | Lead time | Notes |
|---|---|---|---|---|---|
| | | | | | |
