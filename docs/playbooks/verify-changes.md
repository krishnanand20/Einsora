# Playbook: verify a change

Run the checks for every component you touched, then report the **actual** results. A check
you couldn't run is reported as "not run (reason)", never as passing. Everything below runs on
macOS (Apple Silicon) and Linux.

| Paths changed | Run (from repo root) |
|---|---|
| `controlplane/**` | `make lint-cp test-cp build-all` (`make test-cp-pg` too if `store/` or `migrations/` changed; the SQLite store's tests always run) |
| `images/catalog.yaml` | `make test-cp` (the tests load the real catalog) |
| `ml/**` | `make lint-ml test-ml` |
| `web/**` | `make lint-web test-web` and `cd web && npm run build` |
| `deploy/**` | `make deploy-check` |
| `infra/**` | `cd infra/ansible && uvx ansible-lint --offline`, plus `shellcheck infra/scripts/*.sh` |
| Any `*.yaml` | `make lint-yaml` |
| Docs only | Check that links resolve and that code references (paths, function names) still exist |

Also check these when they apply:

- **API shape changed:** `web/src/api/types.ts` matches `controlplane/internal/api/json.go`.
- **New cluster kind:** `kube.allowed` and the controller ClusterRole were both updated (`TestBuildersOnlyEmitAllowedKinds` passes).
- **Lifecycle changed:** the diagram in `docs/architecture/control-plane.md` matches `domain/state.go`.
- **Region or fallback logic changed:** there's a test that one region never sees another's workloads, and a test that fallbacks are recorded (`placed_class`) and accounted as what ran (rules 9 and 10).
- **OS-specific code added:** it's behind build tags in a driver package, and `make build-all` still passes (rule 11).
- **Nothing costs money:** no pricing, payment, or paid-tier logic appears (rule 12).
- **Workload behavior changed:** each new decision shows up in the timeline with a specific reason from a fixed vocabulary (never raw runtime text), any new user input is in the export, and secrets in it are redacted and listed (rule 14). Check with `einsora events <id>` and `einsora export <id>` against `make dev`.
- **Isolation-sensitive paths changed** (see `CONTRIBUTING.md`): run the `tenancy-reviewer`.

## Missing tools

`make doctor` lists what's missing on this machine.

| Tool | Install |
|---|---|
| Go 1.27+ | https://go.dev/dl or `brew install go` |
| uv | `curl -LsSf https://astral.sh/uv/install.sh \| sh` or `brew install uv` |
| Node 22+ | https://nodejs.org or `brew install node` |
| golangci-lint | `brew install golangci-lint` |
| helm, kubeconform | `brew install helm kubeconform` |
