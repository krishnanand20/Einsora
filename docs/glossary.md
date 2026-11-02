# Glossary

| Term | Meaning |
|---|---|
| **Access proxy** | Opens a running workload's web UI in the browser at its own site, `w-<id>.localhost:<port>`, so its code can't act with the dashboard's session. It adds the workload's own token itself, so the token is never in a URL (ADR-0017). |
| **Access ticket** | A one-time link from `POST /v1/workloads/{id}/access`, valid for 60 seconds, that the access proxy exchanges for a cookie for that workload's site only. Only its hash is stored. |
| **Activity log** | (Planned) per tenant: every API action by every user, with its request ID. An audit log. Tenant admins can read and export it (ADR-0015). |
| **Admission** | The controller's decision to place a pending or queued workload onto free capacity |
| **Agent (node agent)** | Runs on each machine (Linux or macOS), reports its inventory, and runs workloads through drivers (ADR-0013). In Solo it runs inside the control plane's process; other machines will run `einsora agent` and dial out (Phase 2). It syncs with the control plane's agent backend (ADR-0016). |
| **Allocation (usage accounting)** | Time a resource is reserved for a workload in a metered state, whatever its utilization. Used for quotas and fairness, never money (ADR-0011). |
| **API token** | A credential for the CLI and scripts (`tsk_…`), made on the dashboard's API tokens page. Shown once, stored only as a hash, revocable, and optionally expiring. |
| **Apple Silicon** | Apple's M-series chips. Their GPU is used through **Metal**, and CPU and GPU share **unified memory**. |
| **CDI** | Containerized Data Importer (Datacenter): imports and clones VM disk images into PVCs (DataVolume, DataSource) |
| **Cell** | One region's runtime (agent machines or a Kubernetes cluster), its storage, and its regional controller (ADR-0009) |
| **Cilium** | eBPF-based CNI providing networking, NetworkPolicy, kube-proxy replacement, and Gateway API (Datacenter) |
| **Community pool** | A Einsora instance where hosts lend idle machines to any of its users (ADR-0014) |
| **DataSource** | A CDI object naming a "golden" disk that VM disks are cloned from (Datacenter) |
| **DCGM** | NVIDIA Data Center GPU Manager. Its exporter publishes GPU metrics. |
| **Desired state** | What a machine should be running, parking, or removing, worked out from the store on every sync, so a restart never loses it (ADR-0016) |
| **Driver** | The part of the agent that runs one kind of workload on one OS, for example `macos-native`, `macos-vz`, `docker`, or `libvirt`; `sim` runs nothing, for development. Each driver's isolation (strong or weak) is set by the control plane, never by the agent. |
| **Export (workload export)** | One JSON file (`einsora.workload-export/v1`) with everything recorded about a workload: inputs, timeline, usage, and redactions. `einsora export <id>`, or **Download full log** in the dashboard (ADR-0015). |
| **Fallback** | An opt-in substitute (another GPU class, CPU, machine, replica, model, or region) used when the requested one is unavailable (ADR-0010) |
| **Gateway API** | The Kubernetes ingress API that succeeds Ingress (Datacenter) |
| **GGUF** | A code-free model file format used by llama.cpp: one file holds the weights, tokenizer, and chat template. A repository usually holds several quantizations of one model; an endpoint names one, as in `hf://org/model-GGUF:Q4_K_M`. `einsora-ml validate --format gguf` checks it (ADR-0013), and the `gguf-endpoint` image serves it (ADR-0022). |
| **GPU class** | A catalog entry for what a workload runs on (`l40s-48gb`, `apple-silicon-32gb`, `cpu-4c-16g`), with its accelerator kind and the regions offering it. CPU classes are classes too. |
| **GPU Operator** | NVIDIA operator that installs the driver, toolkit, device plugins, vfio-manager, MIG manager, and DCGM (Datacenter) |
| **Host** | The owner of a machine that runs workloads. Can see guest data on their own machine. |
| **Inference gateway** | A proxy in front of endpoints: API keys, token counts, rate limits, and request failover (ADR-0010) |
| **Intent** | What a user asked for, recorded in the store. The controller makes the runtime match it. |
| **Inventory** | What a machine's agent reports: OS, CPUs, memory, GPUs, the drivers it has, and what Einsora may use (the rest stays the owner's). It never decides the machine's owner, region, or sharing. |
| **IOMMU** | CPU feature (Intel VT-d, AMD-Vi) that isolates device DMA; required for passthrough |
| **Isolation (strong or weak)** | How well a driver separates a workload from its host: strong (a VM, or a container in its own VM) or weak (a shared kernel, or the macOS sandbox). Weak isolation is only for the machine owner's own workloads (the trust matrix). |
| **Isolation tier** | In code today: `trusted` (owners) or `untrusted` (invited and community users) |
| **Kata Containers** | Runs each container inside a lightweight VM, giving containers a VM-grade boundary |
| **KServe** | Kubernetes model-serving layer. `InferenceService` is its main resource. (Datacenter) |
| **KubeVirt** | Runs VMs as Kubernetes objects (`VirtualMachine`) (Datacenter) |
| **Managed inference** | A host runs a catalog model server with validated weights, and users only send prompts. The safe way for untrusted users to use a Mac. |
| **Metal** | Apple's GPU API, used by MLX, llama.cpp, and PyTorch's MPS backend |
| **MetalLB** | Gives bare-metal clusters LoadBalancer IPs (Datacenter) |
| **Metered** | The workload states whose time is counted by usage accounting (`running`) |
| **MIG** | Multi-Instance GPU: hardware partitioning of A100, H100, and later cards |
| **MLX** | Apple's array framework for machine learning on Apple Silicon. Uses safetensors weights. |
| **Operator** | Whoever runs a Einsora control plane: an individual, a team, a company, or a pool operator |
| **Park** | Release a workload's GPUs while keeping its disks (VM halted, notebook scaled to 0) |
| **Passthrough** | Giving a VM exclusive, direct access to a whole PCI device through VFIO |
| **Placement** | Where a workload actually runs: region, machine, and `placed_class` |
| **Preempted** | (Planned) a workload stopped because a shared machine's owner reclaimed it (ADR-0014) |
| **Profile** | One of three ways to deploy the same product: **Solo**, **Lab**, or **Datacenter** (ADR-0012) |
| **PSA** | Pod Security Admission: per-namespace `privileged`, `baseline`, or `restricted` enforcement (Datacenter) |
| **Quantization** | A model stored at lower precision to save memory, such as GGUF's `Q4_K_M` (about 4.8 bits per weight) or `Q8_0`. A GGUF source names one by its tag. |
| **Reconcile** | One controller pass that makes the runtime match intent: admit, observe, park, remove |
| **Redaction** | Replacing a secret (a password or token, for example inside a model URI) with `REDACTED` in an export. Every redaction is listed in the export, so nothing is removed silently. |
| **Region** | One location (a home, an office, a datacenter) within a deployment. Every workload lives in exactly one, and Solo has one called `local` (ADR-0009). |
| **Request ID** | An ID the server generates for every request, returned in `X-Request-ID` and in error bodies (`request_id`). The same ID is on every log line for that request, with the tenant and subject, so an operator can find exactly what happened and check whose request it was. |
| **Runtime** | How a catalog image runs on a kind of accelerator: for an endpoint, its server and the model format it loads, such as vLLM or MLX with safetensors, or llama.cpp with GGUF on Apple Silicon, NVIDIA, or CPU |
| **safetensors** | A tensor file format that can't execute code when loaded, unlike pickle-based `.bin` and `.pt` files |
| **Sharing level** | Per machine: **private** (default), **invite-only**, or **community** (ADR-0014) |
| **Store** | Where business state lives: SQLite (Solo and small Labs) or Postgres |
| **Sync** | The one call between an agent and the control plane: the agent reports what it has, and gets the machine's complete desired state back as soon as it changes (a long poll; ADR-0016) |
| **Tenant** | A user or organization account. In a datacenter its objects live in namespace `t-<slug>`. |
| **Time-slicing** | Several processes sharing one GPU with no memory isolation. Never used between different users. |
| **Timeline** | A workload's complete list of state changes and notes, each with a time and a human-readable reason (`workload_events`). A **note** records a decision that leaves the state as it was, such as a retry. `einsora events <id>` (ADR-0015). |
| **Trust level** | Owner, invited, or community. Decides which drivers may run a user's workloads (the trust matrix). |
| **Unified memory** | On Apple Silicon, RAM shared by the CPU and GPU. macOS caps the GPU's share by default. |
| **Usage accounting** | Recording who used how much compute, for quotas, fair share, and reports. Never billing. |
| **VFIO** | Linux framework that hands devices to VMs safely, using the IOMMU |
| **vLLM** | High-throughput LLM inference server for NVIDIA GPUs, with an OpenAI-compatible API |
| **Workload** | Something a user launches: `vm`, `notebook`, or `endpoint` |
| **XID error** | NVIDIA driver error code. 79 means the GPU fell off the bus; 48 means a double-bit ECC error. |
