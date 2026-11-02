# AGENTS.md

Instructions for AI coding agents (Codex, Claude Code, others) working in this repository.
Humans should start with [README.md](README.md).

This file is the **single source of truth** for agent instructions. `CLAUDE.md` imports it.
Each top-level component has its own `AGENTS.md` with component rules — read the one
closest to the files you are changing. Rules here apply everywhere; nested files add to them.

## What Einsora is

Einsora is a **free, open, self-hosted GPU cloud** (MIT licence). Anyone installs it on machines they
have, from one Apple Silicon Mac or Linux PC up to a datacenter. They launch **GPU VMs**, **Jupyter notebooks**,
and **OpenAI-compatible model endpoints**, and can share idle machines with others. It's for everyone:
students, developers, startups, small companies, enterprises, and big tech.

- **Profiles** ([ADR-0012](docs/adr/0012-deployment-profiles.md)): **Solo** (one machine, SQLite), **Lab** (mixed Mac and Linux machines running node agents), and **Datacenter** (Kubernetes: KubeVirt, KServe, GPU Operator, Ceph, Postgres, SSO).
- **Node agent** ([ADR-0013](docs/adr/0013-node-agent-and-apple-silicon.md)): runs on Linux and macOS (Apple Silicon), with drivers for Metal, NVIDIA, VMs, and containers.
- **Free** ([ADR-0011](docs/adr/0011-free-for-everyone.md)): there is no pricing or billing anywhere, only usage accounting for quotas and fairness.
- **Sharing** ([ADR-0014](docs/adr/0014-sharing-idle-compute.md)): private by default, then invite-only, or community pools.
- **Transparent** ([ADR-0015](docs/adr/0015-logs-and-transparency.md)): detailed logs anyone can read and export in full, from the CLI, dashboard, or API. Nothing is hidden except secrets.

**Design assumption:** the person using a machine may not be its owner. Protect hosts from guests and
guests from each other, and be honest that hosts can see guest data. See
[docs/architecture/tenancy-and-isolation.md](docs/architecture/tenancy-and-isolation.md).

## Repo map

| Path | Owns | Language / tooling |
|---|---|---|
| `controlplane/` | The `einsora` binary: REST API, controller, usage accounting (`metering`), the node agent (in-process for Solo) and its drivers, and `einsora up`; the inference gateway is planned. Must build for linux/amd64, linux/arm64, and darwin/arm64. | Go 1.27, stdlib `net/http`, pgx, modernc.org/sqlite (pure Go), client-go, goose, golangci-lint |
| `ml/` | ML layer: model validation (safetensors and GGUF), ingestion, endpoint benchmarking, GPU burn-in (CUDA and Metal) | Python 3.12, uv, ruff, mypy, pytest |
| `web/` | Dashboard (to be embedded in the binary for Solo) | TypeScript, React, Vite, TanStack Query |
| `deploy/` | **Datacenter profile:** cluster add-ons (helmfile), `cluster-config` chart, `einsora` chart | Helm, helmfile, Kustomize |
| `infra/` | **Datacenter profile:** Linux server bootstrap (OS, IOMMU/VFIO, k3s, WireGuard) | Ansible, bash |
| `images/` | Launch catalog (`catalog.yaml`); notebook and VM image definitions | Dockerfile, cloud-init |
| `docs/` | Product spec, architecture, ADRs, roadmap, runbooks, playbooks, hardware | Markdown |
| `site/` | The website: landing page, the thinking behind Einsora, the licence, and every doc in this repo, rendered at build time (never copied) | Astro Starlight, published by Cloudflare Pages to einsora.com |

## Commands

Run from the repo root (`make help` lists everything):

- `make doctor` — check that the required tools (Go, uv, Node) are installed; `make setup` runs it first
- `make setup` — install dev dependencies for all components
- `make dev` — **all-in-one local stack**: API + controller + usage accounting in one process, in-memory store, fake backend, seeded tenants. No GPU, Kubernetes, or Docker needed. Runs on macOS and Linux. API on `:8080`.
- `make web-dev` — dashboard on `:5173`, proxied to the local API
- `make test` — all tests (`make test-cp`, `make test-ml`, `make test-web` for one component)
- `make lint` — gofmt, go vet, golangci-lint, ruff, mypy, tsc, yamllint
- `make build-all` — cross-compile `einsora` for linux/amd64, linux/arm64, and darwin/arm64
- `make fmt` — auto-format everything
- `make deploy-check` — render the Datacenter charts and validate with kubeconform (no cluster needed)

With `EINSORA_AUTH_MODE=local`, create the owner account once with `einsora setup -email you@example.com`,
then sign in on the dashboard and make API tokens there. The `einsora` binary is also a CLI client for a
running API (set `EINSORA_TOKEN`): `einsora workloads [-all]`, `einsora machines`, `einsora events <id>`,
`einsora export <id>` ([logs and transparency](docs/architecture/logs-and-transparency.md)). For full
server logs, set `EINSORA_LOG_LEVEL=debug` and `EINSORA_LOG_FILE=<path>`.

**Before saying a task is done:** run the lint and test targets for every component you
touched and report the actual output. If a tool is missing, say so — do not claim success.

## Architecture rules (changing one requires an ADR)

1. **The store owns business state; the runtime owns runtime state.** The store is SQLite or Postgres and holds tenants, quotas, machines, workload intent, events, and usage. The runtime is node agents or Kubernetes. The controller reconciles between them. The API never talks to the runtime: it records intent and returns ([ADR-0008](docs/adr/0008-postgres-intent-kubernetes-runtime.md), [ADR-0012](docs/adr/0012-deployment-profiles.md)).
2. **All runtime access goes through `cluster.Backend`** (`controlplane/internal/cluster`): `fake`, `kubernetes`, and `agent`. The interface is runtime-neutral: the controller places a workload and hands the backend a spec ([ADR-0016](docs/adr/0016-agent-protocol-and-machine-placement.md)). Kubernetes manifests and agents' desired state are built from specs by pure functions and unit-tested as data. Placement is decided in one place, `controller/placement.go`. No ad-hoc client calls elsewhere.
3. **Tenant resources are isolated per tenant.** In a datacenter they live in the namespace `t-<slug>` with the labels in `internal/cluster/labels.go`. On agents, each workload gets its own sandbox or VM, data directory, and network rules. Never share them between tenants.
4. **Untrusted code runs only behind strong isolation, per the trust matrix** in `docs/architecture/tenancy-and-isolation.md`. Invited and community users get VMs, Kata, or Linux-in-VM containers; native macOS execution and plain runc are for owners only. No GPU time-slicing between different users.
5. **Usage accounting comes from allocation intervals** (workload lifecycle events), not utilization. It drives quotas, fair share, and reports. **There is no billing** ([ADR-0011](docs/adr/0011-free-for-everyone.md)).
6. **Model weights must be in a code-free format:** safetensors or GGUF ([ADR-0013](docs/adr/0013-node-agent-and-apple-silicon.md)), checked by `einsora-ml validate` in the format their server loads. Never pickle, and never `trust_remote_code`. The validator (`ml/src/einsora_ml/validate.py`) is the enforcement point.
7. **Every workload state change goes through `service.Transition`**, which validates against `domain` state rules and appends a `workload_events` row. Usage accounting depends on those rows. Never write `state` directly.
8. **Every store query that touches tenant data is scoped by tenant ID.** A request from tenant B for tenant A's object returns 404, never 403 (don't leak existence).
9. **Every workload belongs to exactly one region, and its data never leaves that region** unless the user explicitly opts in. Solo has one region, `local` ([ADR-0009](docs/adr/0009-regions-global-control-plane-regional-cells.md)). Every workload, machine, and usage record carries its region; each controller reconciles one (`EINSORA_REGION`); a machine hosts only workloads in its own region. New code must not assume a single region.
10. **Fallbacks are opt-in and visible** ([ADR-0010](docs/adr/0010-fallbacks-cpu-placement-and-inference-failover.md)). Never silently give a user a different GPU class, CPU, model, machine, or region. Record `placed_class`, say what served each response, and account usage for what actually ran.
11. **Cross-platform:** `controlplane/` must build and run on linux/amd64, linux/arm64, and darwin/arm64 (`make build-all`). OS-specific code lives only behind build tags in driver packages. No Linux-only assumptions anywhere else.
12. **Free for everyone:** no pricing, payment, invoice, paid-tier, or paywall logic in code, UI, or docs ([ADR-0011](docs/adr/0011-free-for-everyone.md)).
13. **Hosts are protected:** sharing is opt-in per machine. Guest workloads never get the host's files or LAN, and community workloads get no internet by default ([ADR-0014](docs/adr/0014-sharing-idle-compute.md)). *Not in code yet.*
14. **Nothing is hidden from its owner** ([ADR-0015](docs/adr/0015-logs-and-transparency.md)). Every decision about a workload (admission, queueing, placement, fallback, failure, retry) is recorded in its timeline with a human-readable reason from a fixed vocabulary, never raw runtime text. Exports are complete. Only secrets are redacted, and every redaction is listed in the export. Logs never cross tenants.

## Conventions

- **Go:** `gofmt`/`goimports`; `context.Context` first parameter; wrap errors with `fmt.Errorf("...: %w", err)`; domain errors from `internal/domain/errors.go`; table-driven tests; no package-level mutable state; everything under `internal/`. CGO-free, so cross-compiles stay trivial.
- **Python (ml/):** type hints everywhere (mypy strict), ruff, pytest. Heavy deps (`torch`, `mlx`) are optional extras and imported lazily.
- **TypeScript (web/):** strict mode; API types in `web/src/api/types.ts` mirror the Go JSON shapes — update both together.
- **API:** JSON over HTTP, versioned under `/v1`. Errors are `{"error": {"code", "message", "request_id"}}`, and every response carries a server-generated `X-Request-ID`; log it with every request-scoped line. Responses never expose runtime internals (node names, object names, IPs, host paths).
- **Config:** environment variables prefixed `EINSORA_` (see `.env.example`). No secrets in code, values files, tests, or docs.
- **Kubernetes/Helm (Datacenter):** pin every chart and image version; never `latest`.
- **Words:** say *user*, not *customer*; *usage accounting*, not *billing*. There's nothing to buy.
- **Commits:** Conventional Commits — `feat(api): ...`, `fix(deploy): ...`, `docs(adr): ...`.

## Workflows

Step-by-step playbooks live in `docs/playbooks/` (Claude Code exposes them as skills):

- Add an API resource → [docs/playbooks/add-api-resource.md](docs/playbooks/add-api-resource.md)
- Add a Datacenter add-on → [docs/playbooks/add-platform-component.md](docs/playbooks/add-platform-component.md)
- Record a decision → [docs/playbooks/write-an-adr.md](docs/playbooks/write-an-adr.md)
- Verify a change → [docs/playbooks/verify-changes.md](docs/playbooks/verify-changes.md)

## Safety rules for agents

- **Never mutate real infrastructure** unless the user explicitly asks in the current session: no `kubectl apply|delete|patch|edit|drain`, `helm install|upgrade|uninstall`, `helmfile apply|sync|destroy`, or `ansible-playbook` without `--check`. Rendering, linting, and `--dry-run=client` are always fine.
- **Never change the developer's own machine configuration** (launchd or systemd services, `sysctl` values such as the GPU memory limit, firewall rules) unless they explicitly ask.
- **Never read or print secrets:** `.env`, kubeconfigs, `*.key`, `*.pem`, `secrets/`, Ansible vault files, agent certificates.
- Don't add a dependency without stating why; prefer the standard library or what is already in `go.mod` / `pyproject.toml` / `package.json`.
- If a change contradicts an architecture rule, stop and propose an ADR instead.
- Keep docs in sync: if you change behavior described in `docs/architecture/` or a runbook, update it in the same change.

## Where to find things

- What we're building, for whom, and the milestone scope: [docs/product/product-spec.md](docs/product/product-spec.md). Check new features against its milestone tables before building.
- Current phase, the next task breakdown, and open decisions: [docs/roadmap.md](docs/roadmap.md)
- Why things are the way they are: [docs/adr/](docs/adr/README.md). Read ADRs 0011–0015 first.
- Logs: what's recorded, who can see it, and how to export it: [docs/architecture/logs-and-transparency.md](docs/architecture/logs-and-transparency.md)
- Terms (Metal, MIG, VFIO, node agent, profile, pool, …): [docs/glossary.md](docs/glossary.md)
