# Einsora: product definition

> The product's name is **Einsora** ([ADR-0021](../adr/0021-rename-to-einsora.md)), and it's MIT-licensed
> ([ADR-0020](../adr/0020-name-and-licence.md)). Before the first release: a trademark attorney's view,
> then register `einsora.dev` and `einsora.com`.

This is the north star for what we're building and for whom. Engineering docs say *how*; this
document says *what* and *why*. Every roadmap item should trace back to something here.

## In one sentence

**Einsora is a free, open GPU cloud you run yourself.** Install it on any machine you have (a MacBook,
a Mac Studio, a gaming PC, a rack of NVIDIA servers) and launch **GPU VMs**, **Jupyter notebooks**,
and **OpenAI-compatible model endpoints** on it. Use it alone, share it with your team or class, or
lend your idle machines to others. The same software runs from one laptop to a datacenter, free for everyone.

## Who it's for

| Who | Typical setup | What they get |
|---|---|---|
| **Students and learners** | One laptop or Mac; a university lab | Notebooks and local models with no cloud bill; access to their lab's shared GPUs |
| **Developers and researchers** | A Mac, or a desktop with an RTX card | Private model endpoints, VMs for experiments, one API for every machine they own |
| **Startups and small companies** | A few servers, plus the team's Macs | A shared internal GPU cloud, with users, quotas, and isolation, at no licence cost |
| **Enterprises and big tech** | Datacenters, many sites | A full private GPU cloud: Kubernetes, VMs, SSO, audit logs, regions, HA. Every feature is free. |
| **People with idle machines** | Any of the above, unused at night or between jobs | A way to share spare compute with a team, a class, or a community pool, safely |

## What you can run

| Workload | What it is | Linux + NVIDIA | Mac (Apple Silicon) | CPU-only machine |
|---|---|---|---|---|
| **GPU VM** | A full VM with root access over SSH | KVM with whole-GPU passthrough | macOS VM with Metal (max 2 per Mac); Linux VM, CPU only | Linux VM |
| **Notebook** | JupyterLab with a persistent workspace | PyTorch with CUDA | PyTorch (MPS) and MLX on Metal | PyTorch on CPU |
| **Model endpoint** | An OpenAI-compatible API for an open model | vLLM or llama.cpp | MLX or llama.cpp on Metal | llama.cpp on CPU (small models) |
| **CPU instance** | A VM or container without a GPU, and the opt-in fallback when GPUs are busy | ✓ | ✓ | ✓ |

AMD GPUs (ROCm) and Windows (through WSL2) come later (M5).

## How you deploy it

Profiles ([ADR-0012](../adr/0012-deployment-profiles.md)) are the same product at three sizes:

| Profile | Install | Machines | Underneath |
|---|---|---|---|
| **Solo** | `brew install` or an install script, then `einsora up` | 1 | One binary, SQLite, built-in dashboard |
| **Lab** | Solo on one machine, plus `einsora agent` on the others | 2 to ~20, mixing Linux and Mac | Node agents ([ADR-0013](../adr/0013-node-agent-and-apple-silicon.md)), SQLite or Postgres |
| **Datacenter** | helmfile and Ansible (`deploy/`, `infra/`) | 3 to thousands | Kubernetes, KubeVirt, KServe, GPU Operator, Ceph, Postgres HA, SSO |

**Sharing** ([ADR-0014](../adr/0014-sharing-idle-compute.md)): every machine is **private** by default.
Its owner can open it to **invited** people, or to a **community pool**: any Einsora instance where hosts
lend idle machines to its users.

## Principles

1. **Free for everyone, forever** ([ADR-0011](../adr/0011-free-for-everyone.md)). No pricing, no paid tiers, and no paywalled enterprise features.
2. **Runs on what you have.** Linux and Apple Silicon Macs are first-class, and CPU-only machines are welcome.
3. **One API everywhere.** Code written against a laptop works unchanged against a datacenter.
4. **Private by default, safe when shared.** A machine is shared only by its owner's choice. Strangers' code never runs without strong isolation, and the dashboard is honest about what a host can see.
5. **No dead ends.** If the GPU you wanted is busy, opt-in fallbacks keep you working: another GPU, CPU, or another machine ([ADR-0010](../adr/0010-fallbacks-cpu-placement-and-inference-failover.md)).
6. **Open.** Open source, open formats (safetensors, GGUF), and standard APIs (OpenAI-compatible).
7. **Nothing hidden** ([ADR-0015](../adr/0015-logs-and-transparency.md)). Every decision about your work is logged with its reason. You can read all of it in the CLI or dashboard and download a complete copy. Only secrets are redacted, and the copy lists each one.

## Feature map

| Area | M1 Solo | M2 Lab | M3 Datacenter | M4 Sharing | M5 Ecosystem |
|---|---|---|---|---|---|
| **Platforms** | macOS (Apple Silicon), Linux (amd64 and arm64) | Mixed Linux and Mac machines | Kubernetes clusters | Any host type | Windows (WSL2), AMD ROCm |
| **Workloads** | Notebooks, model endpoints, CPU instances | VMs (libvirt, macOS VZ); fallbacks | KubeVirt VMs, KServe endpoints | Preemptible guest workloads on shared machines | Batch jobs, fine-tuning jobs |
| **Machines** | The local machine | Agents join over LAN, WireGuard, or the internet (outbound only) | Node pools, regions | Idle detection, schedules, caps, preemption | Auto-discovery on LAN |
| **Users and access** | One local account, API tokens | Users, invites, teams, quotas | SSO (OIDC/SAML), RBAC | Community accounts, fair share, reports and bans | SCIM |
| **Inference** | Endpoints from Hugging Face (safetensors, GGUF) | API keys, request failover | Autoscaling, regional inference gateway | Managed inference on shared hosts | Serverless model API across a pool; model registry; LoRA adapters |
| **Logs** ([details](../architecture/logs-and-transparency.md)) | Workload timeline with reasons, complete export, request IDs (built); workload output and `einsora logs --follow` | Activity log per team (who did what); output from remote machines | Output logs through Loki; activity log (audit log) retention | Hosts see their machine's activity, never guests' output | — |
| **Reliability and ops** | `einsora backup` | Machine health, alerts in the dashboard | HA control plane, Prometheus, backups | Host reputation | Upgrades without downtime |
| **Developer tools** | REST API, dashboard, `einsora` CLI | Python SDK | Verified Datacenter install docs and charts | Pool directory | Terraform provider, templates gallery |

## Quality bar

| Property | Target |
|---|---|
| Install to first notebook (Solo) | **10 minutes or less** on a fresh Mac or Linux box |
| Install footprint (Solo) | One binary plus the runtimes it downloads on demand. No Kubernetes, no Docker required on macOS. |
| Isolation | Untrusted code only behind strong isolation, per the trust matrix in tenancy-and-isolation.md. Never a shared kernel between strangers. |
| Host safety (sharing) | Guests never touch the owner's files or LAN. Reclaim within seconds when the owner returns. Community workloads get no internet by default. |
| Data residency | A workload's data stays on its machine or region unless the user moves it (ADR-0009) |
| No dead ends | With fallbacks opted in, a busy GPU never produces an error while an accepted substitute exists. Responses always say what served them. |
| Usage accounting | Per second, idempotent, and visible within the hour, for quotas and fairness only |
| Security response | Critical vulnerabilities fixed in a release within 72 hours |

## How it compares

| Tool | Great at | Einsora adds |
|---|---|---|
| Ollama, LM Studio | Running models on one machine | VMs and notebooks too; many machines; many users; sharing |
| exo and similar | Pooling home devices for inference | VMs, notebooks, isolation between users, a datacenter path |
| GPUStack and similar GPU cluster managers | Serving models across mixed GPU clusters | VMs and notebooks, idle-machine sharing, a fallback policy |
| dstack, SkyPilot | Orchestrating jobs across clouds and on-prem | A self-contained cloud with a dashboard, from one Mac up |
| DIY Kubernetes (KubeVirt + KServe) | Datacenter flexibility | A product on top: API, users, quotas, isolation rules, and a Solo or Lab mode for small setups |
| Petals, BOINC (volunteer computing) | Pooling volunteers' machines | Interactive workloads, invite-only sharing, and strong host protection |

Check this list again before launch, because these projects move fast.

## Free and open: what that means

- **No money anywhere:** no pricing pages, payment flows, invoices, or "contact sales". Usage accounting exists only for quotas, fairness, and reports.
- **Open source, under the MIT licence** ([ADR-0020](../adr/0020-name-and-licence.md)): anyone may use, change, and redistribute it, including commercially.
- **Anyone can host a pool.** Universities, companies, communities, or the project itself. Pool operators set their own rules and are responsible for them.
- **Models keep their own licences.** Users must follow each model's licence (for example, Llama's). The catalog shows the licence when one is known.

## Legal and safety

| Topic | What it means | Needed by |
|---|---|---|
| Licence and trademark | Licence: MIT (decided). Name: Einsora (decided); a trademark search (classes 9 and 42) and the `einsora.dev` domain before the first release | M1 |
| Privacy | Solo keeps everything local. Pools hold account data, so a pool operator needs a privacy notice (GDPR, DPDP, CCPA and others apply to operators, not to the software). | M4 |
| Acceptable use | A default AUP template for pools: no attacks, no illegal content, no crypto mining on shared hosts | M4 |
| Host disclosure | Clear in-product warnings that host owners can see guest data | M4 |

## Building this solo with AI agents

One person with AI agents can build the software, and this repo is set up for that. The constraints are:

| Constraint | Mitigation |
|---|---|
| **Hardware for testing** | Solo and Lab need none beyond your own machines. **The founder's Mac is the first test host.** Datacenter testing needs one Linux GPU server, which can be borrowed or shared. |
| **Breadth of platforms** | Build in order: Mac and Linux Solo, then Lab, then Datacenter. CI covers linux/amd64, linux/arm64, and darwin/arm64. |
| **Trust in a solo project** | Open source, clear security docs, reproducible releases, and a public issue tracker |
| **Moderating community pools** | Pools are run by their operators. The project only ships the tools (reports, bans, rate limits). |

## Milestones

| Milestone | Exit criteria | Roadmap |
|---|---|---|
| **M0 Foundation** ✓ | Repo, architecture, core platform on a fake cluster, ML validator, dashboard skeleton | Phase 0 |
| **M1 Solo** | On a fresh Mac (Apple Silicon) or Linux box: one-command install; `einsora up`; launch a notebook and a model endpoint on the local GPU (Metal or CUDA) or CPU from the dashboard, CLI, or API | Phase 1 |
| **M2 Lab** | Mixed Mac and Linux machines in one instance; invites, users, and quotas; VMs (libvirt, macOS VZ); fallbacks; API keys for endpoints; usage reports | Phase 2 |
| **M3 Datacenter** | The Kubernetes profile in production use: KubeVirt, KServe, Ceph, Postgres HA, SSO, regions, audit logs, published install docs | Phase 3 |
| **M4 Sharing** | Invite-only sharing, then community pools: idle detection, caps, preemption, managed inference, fair share, abuse tools | Phase 4 |
| **M5 Ecosystem** | Windows (WSL2), AMD ROCm, serverless model API across pools, Terraform, templates | Phase 5 |

## Not doing (until a milestone says so)

Any pricing, payments, or paid features; Intel Macs; workloads spanning machines or regions while
running; user access to the Kubernetes API; native (unsandboxed) code from strangers on personal
machines; crypto mining on shared hosts; training fabrics (InfiniBand) before there's demand.

## Decision log

| Date | Decision | Status |
|---|---|---|
| 2026-10-23 | Go for the core platform, Python for the ML layer, TypeScript for the dashboard (ADR-0003) | Current |
| 2026-10-23 | Launch with GPU VMs, notebooks, and model endpoints together | Current |
| 2026-10-23 | Solo builder with AI agents; the first hire, if any, is infra/SRE | Current |
| 2026-10-23 | Regions are first-class for multi-location deployments (ADR-0009) | Current, reframed for self-hosting |
| 2026-10-23 | Opt-in fallbacks: CPU or other-class placement, and inference failover (ADR-0010) | Current |
| 2026-10-23 | **Free for everyone**: no pricing, no billing, open source (ADR-0011) | Current |
| 2026-10-23 | **Self-hosted in three profiles**: Solo, Lab, Datacenter (ADR-0012) | Current |
| 2026-10-23 | **Node agent; Apple Silicon Macs as first-class hosts** (ADR-0013) | Current |
| 2026-10-23 | **Sharing idle compute**: private, invite-only, community pools (ADR-0014) | Current |
| 2026-10-24 | **Logs and transparency**: complete, exportable logs; nothing hidden except secrets, and redactions are reported (ADR-0015) | Current |
| 2026-10-23 | ~~"Tensora" is a working name; choose and clear a final name before the first release~~ | Superseded by ADR-0020 |
| 2026-11-01 | ~~Name: TensoraFi~~; licence: MIT (ADR-0020) | Name superseded by ADR-0021; the licence still applies |
| 2026-11-01 | **Name: Einsora** (ADR-0021): a US mark TENSORA is registered in the same class | Current |
| 2026-10-23 | ~~Paid cloud with USD pricing, payments, and a first region in India~~ | Superseded by ADR-0011 and ADR-0012 |
