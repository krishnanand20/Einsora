# Contributing

Einsora is free and open for everyone ([ADR-0011](docs/adr/0011-free-for-everyone.md)), and contributions are welcome.

## Setup

Works on macOS (Apple Silicon) and Linux. You need Go 1.27+, uv, and Node 22+; on a Mac,
`brew install go uv node`.

```bash
make doctor       # checks the prerequisites and tells you what's missing
make setup        # Go modules, uv sync for ml/, npm install for web/, pre-commit hooks
make dev          # local all-in-one stack (no GPU or cluster required)
make lint test    # what CI runs
make build-all    # cross-compile for linux/amd64, linux/arm64, darwin/arm64
```

**macOS tip:** avoid keeping the repo in an iCloud-synced folder (`~/Documents` or `~/Desktop` with
"Desktop & Documents Folders" turned on). iCloud can mark `ml/.venv` hidden, which makes Python 3.12+ ignore
its files, and syncing `.git`, `node_modules`, and `.venv` is slow and risky. The Makefile clears the hidden
flag automatically, but a folder like `~/Code` avoids the problem entirely.

## Workflow

1. Branch from `main`: `feat/<short-name>`, `fix/<short-name>`, `docs/<short-name>`.
2. Keep each PR to one concern. Datacenter infra changes (`deploy/`, `infra/`) go in separate PRs from application code.
3. Use [Conventional Commits](https://www.conventionalcommits.org/): `feat(agent): add macOS GPU inventory`.
4. Fill in the PR template, including the **isolation**, **host safety**, and **usage accounting** sections.
5. CI must pass on Linux and macOS. At least one human reviews every PR, including those written by an agent.
6. Maintainers publish a version by tagging a commit on `main`; the release workflow builds it ([release runbook](docs/runbooks/release.md)).

## Decisions

Anything that changes an architecture rule in [AGENTS.md](AGENTS.md), or adds, replaces, or removes a
major component, needs an ADR. See [docs/playbooks/write-an-adr.md](docs/playbooks/write-an-adr.md).

## Changes that need extra care

Changes to these need a reviewer who knows the trust model:

- `controlplane/internal/{api,service,store,cluster}`: tenant scoping, lifecycle, and manifests
- agent and driver code (`controlplane/internal/agent`): the trust matrix, host protections, sandbox profiles
- releases and installs (`packaging/`, `.goreleaser.yaml`, `.github/workflows/release.yml`, `internal/autostart`): what every user downloads and runs, and how it's verified
- `controlplane/internal/metering`: usage-accounting correctness (quotas and fairness depend on it)
- `ml/src/einsora_ml/validate.py`: the code-free model format rule
- `deploy/charts/cluster-config`, plus anything touching NetworkPolicy, RBAC, or RuntimeClass
- region filtering, fallback placement, sharing levels, and the inference gateway (AGENTS.md rules 9, 10, and 13)

## Never add

Pricing, payments, paid tiers, or feature gates that make anything cost money (AGENTS.md rule 12).

## AI agents

Agents follow [AGENTS.md](AGENTS.md). Review their changes as you would any contributor's, with extra
attention to the paths listed above.
