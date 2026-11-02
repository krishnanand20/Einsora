# Architecture Decision Records

Short records of decisions that are expensive to reverse. To write one, follow
[docs/playbooks/write-an-adr.md](../playbooks/write-an-adr.md). Claude Code users can run `/new-adr`.

ADRs are history, so they're never rewritten. When a later decision changes an earlier one, the earlier
ADR gets a banner at the top saying what still applies. **Read the newest ADRs first:** 0011 to 0020
define today's product. ADRs keep the names of their time: 0001 to 0019 use the working name "Tensora"
(`TENSORA_*`, `tensora.io/*`, the `tensora` binary), and 0020 uses "TensoraFi". The product is
**Einsora**; [ADR-0020](0020-name-and-licence.md) and [ADR-0021](0021-rename-to-einsora.md) map the old
identifiers to Einsora's.

| # | Decision | Status |
|---|---|---|
| [0011](0011-free-for-everyone.md) | **Free for everyone:** no pricing, no billing, open source | Accepted |
| [0012](0012-deployment-profiles.md) | **Deployment profiles:** Solo, Lab, Datacenter | Accepted (SQLite store and local accounts built; the rest planned) |
| [0013](0013-node-agent-and-apple-silicon.md) | **Node agent and drivers; Apple Silicon Macs as first-class hosts** | Accepted (machines, accelerators, Apple and CPU classes, per-platform runtimes, the in-process agent, and the `macos-native` and `docker` drivers in code; VM drivers planned) |
| [0014](0014-sharing-idle-compute.md) | **Sharing idle compute:** private, invite-only, community pools | Accepted (sharing level stored per machine; enforcement planned) |
| [0015](0015-logs-and-transparency.md) | **Logs and transparency:** complete, exportable, nothing hidden except secrets | Accepted (timeline, export, CLI built; output and activity logs planned) |
| [0016](0016-agent-protocol-and-machine-placement.md) | **Agent protocol and machine placement:** one long-polled `Sync`, desired state from the store, a runtime-neutral backend, placement in the controller | Proposed (in-process agent, agent backend, and placement built; network transport Phase 2) |
| [0017](0017-local-workload-access-and-native-drivers.md) | **Workload access and native drivers:** open workloads through an access proxy at their own `*.localhost` host with one-time tickets; native drivers are supervised, sandboxed processes | Proposed (proxy, process supervisor, and `macos-native` notebooks, MLX endpoints, and llama.cpp endpoints built) |
| [0018](0018-linux-docker-driver.md) | **Linux Docker driver:** local Engine API, owner-only containers, physical NVIDIA UUIDs, private socket access, durable recovery | Proposed (implemented; live Linux/CUDA validation pending) |
| [0019](0019-driver-specific-runtime-selection.md) | **Driver-specific runtimes:** choose by accelerator and driver; native CPU notebooks reserve Metal access explicitly | Proposed (implemented; real CPU/Metal notebook tests) |
| [0020](0020-name-and-licence.md) | **Name and licence:** the product is TensoraFi, renamed everywhere in one change; MIT licence | Accepted (name superseded by 0021; MIT still applies) |
| [0021](0021-rename-to-einsora.md) | **Rename to Einsora:** a US mark TENSORA is registered in the same class; every identifier renamed in one change | Accepted |
| [0022](0022-llama-cpp-endpoints.md) | **llama.cpp endpoints:** one pinned release (checked by SHA-256 on Macs, by digest in Docker), a separate GGUF endpoint image, one quantization per source, no silent CPU offload | Proposed (implemented; real Metal and Linux runs pending) |
| [0001](0001-kubernetes-kubevirt-gpu-operator.md) | Kubernetes, KubeVirt, and the GPU Operator | Accepted; Datacenter profile only (0012) |
| [0002](0002-k3s-single-node-first.md) | k3s with embedded etcd | Accepted; Datacenter profile only (0012) |
| [0003](0003-language-split.md) | Go for the core platform, Python for the ML layer, TypeScript for the dashboard | Accepted |
| [0004](0004-untrusted-code-in-vms-or-kata.md) | Untrusted code runs only behind strong isolation (VMs, Kata) | Accepted; generalized by the trust matrix (0014) |
| [0005](0005-storage-longhorn-then-ceph.md) | Longhorn, then Rook-Ceph | Accepted; Datacenter profile only (0012) |
| [0006](0006-bill-on-allocation.md) | Account usage on allocation, not utilization | **Superseded in part by 0011** (no billing; accounting kept) |
| [0007](0007-gateway-api-on-cilium.md) | Gateway API on Cilium | Accepted; Datacenter profile only (0012) |
| [0008](0008-postgres-intent-kubernetes-runtime.md) | The store owns intent, the runtime owns runtime state, a controller reconciles | Accepted; generalized by 0012 and 0013 |
| [0009](0009-regions-global-control-plane-regional-cells.md) | Regions: one control plane, independent regional cells | Accepted; reframed for self-hosting by 0011 and 0012 (regions in the data model, catalog, API, and controller) |
| [0010](0010-fallbacks-cpu-placement-and-inference-failover.md) | Fallbacks: CPU placement and inference failover | Accepted; "billed" reads as "accounted" (0011) (placed class recorded and accounted; fallback placement planned) |

Statuses run Proposed → Accepted → (Superseded by NNNN | Deprecated). Never delete an ADR; supersede it.
