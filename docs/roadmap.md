# Roadmap (development plan)

This is the engineering plan. **What** each milestone must deliver is defined in the
[product spec](product/product-spec.md#milestones). The order follows the product: one machine first
(a Mac or a Linux box), then many machines, then datacenters, then sharing.

| Phase | Delivers | Needs hardware |
|---|---|---|
| 0. Foundations | M0 ✓ and the redesign | — |
| 1. Solo | M1: install on one Mac or Linux machine and use its GPU | Your own Mac or PC |
| 2. Lab | M2: many mixed machines, many users | 2+ of your own machines |
| 3. Datacenter | M3: the Kubernetes profile | One Linux NVIDIA server (borrowed is fine), more later |
| 4. Sharing | M4: invite-only sharing and community pools | Any |
| 5. Ecosystem | M5: Windows, AMD, serverless API across pools | Any |

## Phase 0: Foundations

- [x] Repo structure, agent instructions (AGENTS.md and CLAUDE.md), CI, and docs
- [x] Core platform skeleton (Go): API, controller, usage accounting, lifecycle, quotas, fake and Kubernetes backends
- [x] It already builds and runs on macOS Apple Silicon: `make dev` runs there, and `make build-all` cross-compiles for linux/amd64, linux/arm64, and darwin/arm64
- [x] ML layer (Python): model validator, ingestion, benchmarking, burn-in (CUDA)
- [x] Dashboard skeleton (TypeScript)
- [x] Redesign: free for everyone (ADR-0011), profiles (ADR-0012), node agent and Apple Silicon (ADR-0013), sharing (ADR-0014), regions (ADR-0009), fallbacks (ADR-0010)
- [x] Logs and transparency (ADR-0015): a timeline for every workload with a specific reason for each change, complete JSON export (deleted workloads too), secret redaction that is always reported, request IDs on every response and log line, `EINSORA_LOG_FILE`, CLI client commands (`workloads`, `events`, `export`), and a dashboard timeline with **Download full log**
- [x] **Choose the final product name:** Einsora ([ADR-0021](adr/0021-rename-to-einsora.md); TensoraFi before it, [ADR-0020](adr/0020-name-and-licence.md))
- [x] **Rename in one change:** Go module path, `einsora.dev/` labels, `EINSORA_` env vars, `X-Einsora-*` headers, cookies, binary, Python package, charts, docs
- [x] **Rename the GitHub repository:** `github.com/krishnanand20/Einsora` (GitHub redirects the old URLs)
- [ ] **Clear the name:** a web search found nothing called Einsora (2026-11-01), but get a trademark attorney's view in your main markets (classes 9 and 42) before the first release, mentioning the "Sora" sound ([ADR-0021](adr/0021-rename-to-einsora.md)); then register `einsora.dev` and `einsora.com` (both available on 2026-11-01)
- [x] **Choose the open-source licence:** MIT, in `LICENSE` ([ADR-0020](adr/0020-name-and-licence.md))
- [x] Set the image registry: `ghcr.io/krishnanand20`, the repository's own GitHub registry

## Phase 1: Solo (→ M1) ← *we are here*

**Done when:** on a fresh Apple Silicon Mac and a fresh Linux box, one command installs Einsora,
`einsora up` opens the dashboard, and a notebook and a model endpoint run on the local GPU (Metal or
CUDA), or on CPU, from the dashboard, CLI, or API.

### Task breakdown: the Solo foundation (next code change)

Each step keeps `make lint test deploy-check build-all` green. Do the data-model steps before any real data exists.

| # | Area | Task |
|---|---|---|
| 1 | Store | ✅ **Done.** `sqlitestore` with `modernc.org/sqlite` (pure Go, CGO-free). It passes `storetest`, and migrations live in `migrations/postgres` and `migrations/sqlite`. Select it with `EINSORA_DATABASE_URL=sqlite:<path>`. |
| 2 | Data model | ✅ **Done**, as migration `00002` rather than an edit to `00001`, which had been applied by then; `00003` repairs its backfill (admitted means it ever moved to provisioning) and rebuilds `idempotency_keys` per user. New tables: `machines` (owner, region, OS, architecture, accelerators, sharing level, last seen) and `idempotency_keys` (used from Phase 2). `workloads` gains `region`, `machine_id`, `placed_class`, `fallback`, and `fallback_after_s`; `gpu_class` stays the requested class (it's the API's name for it). `usage_records` gains `region` and `machine_id`, and its `gpu_class` is now the class that actually ran, which covers what `compute_class` was meant to do. |
| 3 | Domain | ✅ **Done.** `Region`, accelerator kinds (`nvidia`, `apple`, `amd`, `cpu`), sharing levels, `Machine` with validation, and placement fields on `Workload`. Tested: a workload can't enter a metered state before it's placed (`Transition` refuses), and a machine needs exactly one valid region. The controller records placement in the same transaction as admission, capacity is counted by placed class, and usage is accounted on the placed class and region. |
| 4 | Catalog | ✅ **Done.** A `regions:` list (the first is the default; Solo has `local`). Classes have an `accelerator` and optional `regions`. Apple Silicon classes by unified-memory tier (`apple-silicon-32gb`; owners only, rule 4) and CPU classes (`cpu-4c-16g`, `cpu-16c-64g`). Images list runtimes in order: the endpoint image (`openai-endpoint`) uses vLLM on NVIDIA, MLX on Apple, and llama.cpp on anything. Create takes `region`; the controller reconciles one region (`EINSORA_REGION`). |
| 5 | Auth | ✅ **Done.** `EINSORA_AUTH_MODE=local`: accounts with argon2id passwords; dashboard sessions (HttpOnly, SameSite=Strict cookie, CSRF header, 7 days); API tokens (`tsk_…`, shown once, stored as hashes, expiring or not, revocable); guessing limited per email-and-address pair and per address; passwords and cookies only over HTTPS or this machine (`EINSORA_TRUSTED_PROXIES`, `EINSORA_INSECURE_HTTP`); tokens can't make tokens or change the password; timelines name the credential; and `einsora setup` (with `-reset-password`) for the owner account. The dashboard's own first-run page arrives with `einsora up` (step 8). |
| 6 | Agent | ✅ **Done** ([ADR-0016](adr/0016-agent-protocol-and-machine-placement.md)). `cluster.Backend` is runtime-neutral (`Run`, `Park`, `Observe`, `Delete` take a placed spec), and the controller places, then runs. The agent protocol is one long-polled `Sync`, whose desired state is computed from the store, so restarts are safe. The `agent` backend (`internal/cluster/agents`) and the in-process agent (`internal/agent`) report inventory (CPUs, memory, Apple Silicon GPUs through sysctl, NVIDIA through `nvidia-smi`) and heartbeats, and run what they're given through drivers (`sim` for development). Placement (`controller/placement.go`) chooses the machine and driver: region and sharing (`Machine.CanHost`), the trust matrix (`Machine.Permits`: weak isolation only for the owner's own workloads), and free devices, CPUs, and memory; VMs and notebooks return to their disk. Agents re-check the driver, isolation, and free GPUs, and delete data only for a workload listed for removal. A machine is found by the ID saved next to its database, and its owner and region come from its registration, never its agent. Until Phase 4's opt-in, workloads are placed only on their tenant's own machines. Migration `00005` adds machine CPUs and memory, and the workload's driver. The network transport, enrolment tokens, and agent certificates moved to Phase 2's first item. |
| 7 | Drivers | ✅ **Done** ([ADR-0017](adr/0017-local-workload-access-and-native-drivers.md), [ADR-0018](adr/0018-linux-docker-driver.md), [ADR-0019](adr/0019-driver-specific-runtime-selection.md)). Drivers implement `agent.Driver` ([ADR-0016](adr/0016-agent-protocol-and-machine-placement.md)): `Run`, `Stop`, and `Remove` return promptly and report long work (an image pull) as progressing; each status says which attempt it's about (`Run` replaces the status with the new attempt before it returns, and attempts are kept across agent restarts), and `Stop` leaves a workload stopped, not failed; a driver never creates a fresh disk for a VM or notebook that has run before, and reports that its disk is missing instead; driver errors go through `redact.Secrets` before they're logged. Driver code lives behind build tags. **7a** ✅ **Done:** the process supervisor (`agent/drivers/process`: process groups, log files, process IDs checked against their start time); `macos-native` notebooks (JupyterLab with PyTorch on Metal, in a uv environment built from a lock file with hashes, under `sandbox-exec`, found again after a restart); the access proxy (`internal/access`), which opens a running notebook at its own site, `w-<id>.localhost`, with a one-time ticket from `POST /v1/workloads/{id}/access` (migration `00007`), so a workload's pages can't act with the user's dashboard session and its token is never in a URL; and the dashboard's Open button. Native workloads listen on no TCP or UDP port: JupyterLab is on a Unix socket and kernels talk over IPC, so a workload can't be handed the browser's cookies (they ignore ports); validated with the real environment (PyTorch and MLX on Metal). **7b** ✅ **Done:** `macos-native` endpoints fetch Hugging Face or S3 models into private workload storage, validate with the bundled ML policy, and serve one fixed safetensors model through MLX on Metal over a Unix socket. The access proxy rechecks existing API tokens, their tenant, and running state on every request; the dashboard shows the API base URL. Chat, text completions, SSE, and token usage are supported, with request limits and no model switching or remote code. Preparing environments resume after restarts with child-inherited build locks. Tested with a tiny real model on Metal and a local S3 fixture. **7c** ✅ **Implemented** ([ADR-0018](adr/0018-linux-docker-driver.md)): Linux Docker notebooks on NVIDIA or explicitly selected CPU classes, and validated safetensors endpoints through pinned vLLM. Local rootful Engine only, private Unix socket access, non-root containers with cgroup limits, exact physical GPU UUID reservations, asynchronous image pulls, and durable stop/remove/restart recovery. Missing NVIDIA support advertises CPU only; GPU requests never silently become CPU. Engine lifecycle and failure tests run without Docker; an opt-in real Linux notebook test is included. **Live Linux/CUDA verification remains pending** (development host is macOS). **7d** ✅ **Implemented** ([ADR-0019](adr/0019-driver-specific-runtime-selection.md)): catalog `driver_refs` selects by accelerator and driver during placement, admission, and dispatch. `macos-native` CPU notebooks use the pinned Jupyter environment, preserve data, and remain owner-only. Native code can still use Metal, so both controller and agent reserve the Apple GPU exclusively even for a CPU class; the timeline explains this. Model-format checks apply again to the selected driver runtime. llama.cpp servers come with GGUF validation (step 10). |
| 8 | `einsora up` | ✅ **Done.** `einsora up` runs Solo in one process: the API with the dashboard built in (`internal/webui`: `go:embed` of `web/dist`, with a strict Content-Security-Policy, no framing, and the dashboard's routes surviving a refresh), the controller with this machine's agent and the access proxy, and usage accounting. It listens on `127.0.0.1` only, keeps its data in one owner-only directory per OS (`EINSORA_DATA_DIR`; macOS `~/Library/Application Support/Einsora`, Linux `~/.local/share/einsora`), writes `einsora.log` there as JSON while the terminal gets readable lines, and the first time asks for the owner account in the terminal (`einsora setup` without one: no sign-up page). The catalog is built into the binary (a copy of `images/catalog.yaml` that a test keeps current). `make build` builds it all; a port already in use is a clear error. Every `EINSORA_*` setting overrides a Solo default. |
| 9 | Web | Machines page; region and placed class on each workload; fallback opt-in on the launch form. No money anywhere. |
| 10 | ML | GGUF validation in `validate.py`; an MPS (Metal) path for `burnin`; bench docs for llama.cpp and MLX servers |
| 11 | Packaging | Homebrew tap, `install.sh`, `.deb` and `.rpm` packages, launchd and systemd units, signed and notarized macOS builds, a release workflow |
| 12 | Docs | The website (`site/`) is built: a landing page, the thinking behind Einsora, the licence, and every document in this repository, rendered at build time and published by Cloudflare Pages to einsora.com. Left: turn [runbook 00](runbooks/00-quickstart.md) from planned into verified; update the control-plane.md API tables; remove "planned" notes as each item lands |
| 13 | CLI | Write commands: `einsora launch`, `stop`, `start`, `delete`, and `catalog`, with `-json` output. The read commands (`workloads`, `events`, `export`) are built. |
| 14 | Logs | Workload output (ADR-0015): drivers capture stdout and stderr, and VM boot logs. The agent ships them to the control plane, which keeps them per workload for 14 days. Add `GET /v1/workloads/{id}/logs` (with follow), `einsora logs <id> --follow`, a Logs tab in the dashboard, and `output_logs` in the export. |

## Phase 2: Lab (→ M2)

**Done when:** a class or small team runs one instance across mixed Mac and Linux machines, with
users, quotas, VMs, fallbacks, and endpoint API keys.

- [ ] Agents join over the network (ADR-0016): `POST /agent/v1/sync` over HTTPS with mutual TLS, outbound from the agent (works behind NAT); a certificate authority on the control plane; enrolment tokens from the dashboard, which fix the machine's owner and region; per-machine certificates and revocation; machine health on the dashboard; failing a running workload whose machine stays offline past a grace period
- [ ] Users, invites, teams, and per-user quotas; usage reports (no money)
- [ ] Activity log per tenant (ADR-0015): every API action by every user, with its request ID; tenant admins can read and export it
- [ ] VMs: `libvirt` driver (Linux KVM, with VFIO GPU passthrough) and `macos-vz` driver (macOS guests with Metal, max 2 per Mac; Linux guests CPU-only)
- [ ] `macos-container` driver (a Linux container in its own lightweight VM) for strong-isolation CPU workloads on Macs
- [ ] Fallback placement across machines and to CPU (ADR-0010); `Idempotency-Key` on POST; `Retry-After` on 503
- [ ] Built-in inference gateway: endpoint API keys, token counts, failover to another replica or CPU
- [ ] Access to workloads from outside the LAN through the control plane's relay (notebook UI, endpoint API, VM SSH)
- [ ] Python SDK; Postgres option for larger Labs; `einsora backup` and `restore`, portable between SQLite and Postgres (moving up a profile)

## Phase 3: Datacenter (→ M3)

**Done when:** the Kubernetes profile runs in production on real servers, with VMs, endpoints, SSO,
regions, and HA, and its install docs are verified.

- [ ] One Linux NVIDIA server: bootstrap ([runbook 01](runbooks/01-node-bootstrap.md)), passthrough and burn-in ([runbook 02](runbooks/02-gpu-passthrough.md)), first endpoint ([runbook 03](runbooks/03-first-model-endpoint.md))
- [ ] Verify and record every pinned chart version (deploy/README.md)
- [ ] Kubernetes backend end to end, plus an orphan sweep (regional controllers, `EINSORA_REGION`, are built)
- [ ] **Test GPU memory remanence** between VMs (tenancy-and-isolation.md, open risks)
- [ ] **Validate Kata + GPU passthrough** (needed before untrusted notebooks and endpoints run in containers)
- [ ] Models resource, with `einsora-ml ingest` running as a Job
- [ ] More nodes; 3 k3s servers; Rook-Ceph; object store; Postgres HA (CloudNativePG); backups and restore drills
- [ ] SSO through Keycloak or the company's IdP; RBAC; the activity log kept in Postgres with configurable retention
- [ ] Workload output logs from Loki (Alloy ships pod logs and VM consoles), served by the API per tenant (ADR-0015)
- [ ] Regional inference gateway with autoscaling; Prometheus `/metrics` and alerts; host firewall (admin access only over WireGuard); upgrade runbook
- [ ] Controller leader election; usage-accounting correction for workloads that died while the controller was down

## Phase 4: Sharing (→ M4)

**Done when:** owners can safely lend idle machines to invited people, and anyone can run a
community pool with fair share and abuse tools.

- [ ] Sharing levels per machine (private, invite-only, community) and the trust matrix enforced at admission
- [ ] Idle detection (macOS IOKit, Linux logind), schedules, AC-power checks, and resource caps
- [ ] Preemption: a `preempted` state, instant reclaim, and re-queueing through fallbacks
- [ ] Managed inference for community users on hosts without strong isolation (Macs)
- [ ] Community workloads with no internet egress by default; models delivered through the pool's model store
- [ ] Verified accounts, fair-share queues, rate limits, reports and bans, and an AUP template
- [ ] Clear disclosure in the product that host owners can see guest data; contributed-hours reports for hosts
- [ ] Host reputation, and an optional directory of public pools

## Phase 5: Ecosystem (→ M5)

- [ ] Windows agent (through WSL2, with CUDA)
- [ ] AMD GPUs (ROCm)
- [ ] Serverless model API across a pool
- [ ] Terraform provider; templates gallery; LAN auto-discovery; upgrades without downtime
- [ ] Batch and fine-tuning jobs; MIG-backed classes; SCIM

## Open decisions

| Decision | Current assumption | Needed by |
|---|---|---|
| Final product name | **Einsora** ([ADR-0021](adr/0021-rename-to-einsora.md)); not yet cleared by a trademark attorney | Before the first release |
| ~~Open-source licence~~ | **Decided** ([ADR-0020](adr/0020-name-and-licence.md)): MIT | — |
| ~~Agent protocol~~ | **Decided** ([ADR-0016](adr/0016-agent-protocol-and-machine-placement.md)): one long-polled `Sync`, over HTTPS with mutual TLS on the network, outbound from the agent | — |
| Sandbox for native macOS workloads | A macOS sandbox profile per workload, for the machine owner's own workloads only | Built for notebooks and MLX endpoints (steps 7a and 7b, ADR-0017) |
| Linux containers on Mac | Apple's containerization framework, or Lima | Phase 2 |
| Remote access to workloads | A relay through the control plane (Solo and Lab); in a datacenter, a shared IP with a port per VM | Phase 2 |
| Inference gateway implementation | Built into the Go control plane (Lab); Envoy-based or Go in a datacenter | Phase 2 |
| Datacenter object store | SeaweedFS or Garage first, then Ceph RGW | Phase 3 |
| Untrusted notebooks in a datacenter | VMs until Kata + GPU is validated | Phase 3 |
| Does the project run a public community pool? | Not decided; anyone can run one | Phase 4 |
