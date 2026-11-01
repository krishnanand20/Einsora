# AGENTS.md: controlplane/ (Go core platform)

These rules add to the root [AGENTS.md](../AGENTS.md); read that first.

This module is the `tensorafi` binary. It must build and run on **linux/amd64, linux/arm64, and
darwin/arm64** (rule 11): run `make build-all` from the repo root.

## Layout and dependency direction

```
cmd/tensorafi/            main: wiring only. Server subcommands: api | controller | metering | migrate | seed | dev
                        (planned: up | agent | gateway | backup | restore)
                        client.go: CLI client commands workloads | events | export (planned: logs)
internal/domain/        types, the state machine, and error codes. Imports nothing internal.
internal/config/        TENSORAFI_* env parsing and production guards
internal/catalog/       loads images/catalog.yaml
internal/store/         Store/Queries interfaces
  memstore/             in-memory implementation (dev and tests)
  pgstore/              Postgres implementation (pgx), plus Migrate()
  sqlitestore/          SQLite implementation for Solo and Lab (modernc.org/sqlite: pure Go, no CGO)
  storetest/            conformance suite every implementation must pass
internal/service/       business operations, tenant-scoped; Transition() is the single path for state changes
internal/api/           HTTP handlers (stdlib net/http); thin, and calls the service only
internal/access/        the access proxy (ADR-0017): opens a running workload at w-<id>.localhost with a one-time ticket
internal/auth/          authenticators: Local (API tokens and session cookies, with the CSRF check), OIDC (JWT), and Dev (header)
internal/credential/    argon2id password hashing, and random tokens stored only as SHA-256 hashes
internal/redact/        removes credentials from URIs and free text; imports nothing internal, so the agent can use it
internal/client/        Go client for the /v1 API, used by the CLI commands (and tests); keeps exports byte-for-byte
internal/cluster/       Backend interface (runtime-neutral: Capacity, Run, Park, Observe, Delete), labels, CheckJSONTypes
  manifests/            pure builders that return Kubernetes objects as map[string]any
  fake/                 in-memory backend
  kube/                 client-go backend with server-side apply and a kind allowlist; builds manifests from specs
  agents/               the agent backend (the hub): agents sync with it; desired state comes from the store
internal/controller/    reconcile loop: admit → observe → park → remove; placement.go decides where workloads run
internal/agent/         the node agent: syncs with the control plane and runs workloads through drivers
  protocol/             the Sync messages, status codes, and the fixed reasons they map to
  drivers/sim/          a driver that runs nothing (development and tests only)
  drivers/inventory/    CPUs, memory, and GPUs, per OS behind build tags
  drivers/hostlock/     one agent per machine: an OS lock (flock) on its state directory
  drivers/process/      the process supervisor for native drivers: process groups, log files, start-time checks (Unix)
  drivers/macosnative/  macos-native (darwin): JupyterLab (Apple GPU or CPU) and MLX endpoints in hashed uv environments under sandbox-exec
  drivers/docker/       docker (Linux): owner-only JupyterLab and NVIDIA vLLM through a local rootful Engine
  drivers/pythonruntime/ shared embedded Python model policy and workers (regenerate with ml/scripts/bundle_native.py)
internal/metering/      usage accounting: events → metered intervals → hourly usage records (no money)
internal/devseed/       dev tenants (acme and globex untrusted, internal trusted)
internal/testutil/      fixtures: real catalog, seeded store, manual clock
migrations/             goose SQL files embedded into the binary: postgres/ and sqlite/, one file per version in each
(planned) internal/agent/drivers/    macosvz, macoscontainer, libvirt (Phase 2)
(planned) the HTTPS agent transport, enrolment, and agent certificates (Phase 2)
```

Allowed imports flow downward: `api → service → store, domain`; `controller → service, cluster, store`;
`cluster/agents → agent/protocol, store, catalog`; `agent → agent/protocol, domain`; `access → store, credential,
domain` (it finds workloads through an interface the agent backend implements). `api` must never import
`cluster`, `controller`, `agent`, or `access`; `agent` never imports `store` or `cluster` (an agent knows only what it's
told); and `domain` imports nothing internal.

## Rules specific to this module

- **New store method:** add it to `store.Queries`, implement it in **every** store (memstore, pgstore, and sqlitestore), and add a case to `storetest`. Tenant-scoped methods take `tenantID` and filter by it.
- **Schema change:** add `migrations/postgres/NNNNN_*.sql` and the same change in `migrations/sqlite/NNNNN_*.sql` (`TestDialectsMatch` checks both exist), and update `storetest.SchemaColumns` and `SchemaIndexes` (both stores' tests compare their schema with them). Never edit an applied migration. SQLite tables are `STRICT`, UUIDs are text, and times are fixed-width UTC text, so text order is time order (ADR-0012).
- **SQLite runs one transaction at a time** (one connection, `BEGIN IMMEDIATE`). That replaces Postgres's row locks, so never hold a transaction open while waiting on something else, never start one inside another, and keep transactions short: another process sharing the file waits at most 15 seconds.
- **The SQLite file is private:** it and its `-wal`, `-shm`, and `-journal` files must be `0600` regular files, in a directory others can't write to. Paths are resolved to absolute file paths; `:memory:` and `file:` URIs are refused, because SQLite would read them as something other than the file that was checked. Keep `workload_events` append-only (a trigger enforces this).
- **New cluster kind:** add the builder in `manifests/`, add the GVK to `kube.allowed`, and add the RBAC rule in `deploy/charts/tensorafi/templates/controller.yaml`. `TestBuildersOnlyEmitAllowedKinds` fails if the allowlist is missing an entry.
- **Unstructured types:** builders may use only `map[string]any`, `[]any`, `string`, `bool`, `int64`, and `float64`. Quantities are strings (`"64Gi"`, `"2"`). `cluster.CheckJSONTypes` enforces this in tests and in the fake backend.
- **OS-specific code** (agents and drivers): only in driver packages, behind build tags (`//go:build darwin`, `//go:build linux`), with a stub for other platforms so `make build-all` stays green.
- **No CGO.** Pick pure-Go dependencies so cross-compiling stays trivial.
- **State changes:** call `service.Transition(ctx, q, &w, to, reason, now)` inside `store.InTx`. Never call `UpdateWorkloadState` directly.
- **Time:** inject `now func() time.Time`, never call `time.Now()` inside logic, and store UTC.
- **Errors:** expected failures are `domain.Errorf(code, ...)`, and their message is shown to users. Wrap everything else with `%w`; it becomes a logged 500.
- **Logging:** `log/slog` with key-value pairs. Never log passwords, tokens, session values, SSH keys, agent certificates, or model URIs that carry credentials.
- **Credentials:** store only hashes (`credential.HashPassword`, `credential.HashToken`), and show a new API token only in the response that creates it. Hash passwords outside any store transaction: argon2id is slow on purpose, and SQLite runs one transaction at a time. Errors that may echo inputs (runtime, Kubernetes, and driver errors) go through `redact.Secrets` (or its wrapper `service.RedactSecrets`) first. Request-scoped lines use `requestInfoFrom(ctx).logAttrs()` in `api`: the request ID, plus the tenant and subject once authenticated.
- **Transparency** (rule 14, ADR-0015): every controller decision gets a specific, human-readable `reason` in `service.Transition` (for example `waiting for l40s-48gb capacity: requested 2 GPU(s), 0 free`, not `queued`). A decision that doesn't change state, such as a retry, gets a note (`service.Note`). New fields a user supplied belong in the export. If a field can hold a secret, redact it in `service.ExportWorkload` and add a `Redaction` entry; never drop data silently.
- **Reasons are a fixed vocabulary.** Never put raw runtime messages or errors into a reason: they can name internal objects, describe other tenants' use of the cluster, or echo credentials. `cluster.Backend.Observe` returns fixed text, and the raw error goes to the operator's log. Only trusted tenants see free-capacity counts.
- **Agents are not trusted with policy** (ADR-0016). A machine's owner and region come from its registration, never from its agent. A driver's isolation comes from `domain.LookupDriver`, never from the agent. Inventory is bounded and validated (`Machine.Validate`, `agents.checkInventory`) before it's stored. What an agent reports reaches a timeline only as a fixed reason (`protocol.Reason`), never as its own text. Desired state is computed from the store, never kept only in memory.
- **Placement lives in `controller/placement.go`**, as pure functions with unit tests: region and sharing (`Machine.CanHost`), isolation per the trust matrix (`Machine.Permits`), and free devices, CPUs, and memory. Stores re-check `CheckPlacement` in `SetPlacement`, and agents re-check the driver, isolation, and free GPUs.
- **No money** (rule 12): `metering` is usage accounting for quotas and fairness only.

## Testing

- `go test ./...` runs everything except Postgres tests, on macOS and Linux. `make test-cp-pg` runs those too, using Docker.
- Use `testutil.SeededStore`, `testutil.Catalog`, and `testutil.NewClock` rather than new fixtures.
- Every new endpoint needs a cross-tenant test: tenant B gets 404 for tenant A's object.
- Prefer table-driven tests. Controller tests drive `ReconcileOnce` with `fake.Backend` and assert on states.

## Planned changes (design accepted, not in code yet)

The [roadmap's Phase 1 task breakdown](../docs/roadmap.md#task-breakdown-the-solo-foundation-next-code-change)
has the full list. Until it lands:

- **Profiles and agents (ADR-0012, ADR-0013, ADR-0016):** don't assume Kubernetes, Postgres, or Linux outside their own packages. Everything above `cluster.Backend` and `store.Store` must work with the agent backend and SQLite. Agents joining over the network (HTTPS with mutual TLS, enrolment tokens, per-machine certificates) are Phase 2.
- **Regions (ADR-0009):** don't write code that assumes one region. Every workload, machine, and usage record carries one; the controller reconciles `Options.Region` only; regional queries (`WorkloadsInStates`, `GPUsAllocated`) take a `region` parameter.
- **Fallbacks (ADR-0010):** don't treat the requested class (`gpu_class`) as the class that runs. Capacity, usage accounting, and display use `placed_class`, which the controller's `place` step records at admission, as `controller/placement.go` chose. `service.Transition` refuses provisioning or a metered state for an unplaced workload.
- **Sharing (ADR-0014):** invite lists will join `Machine.CanHost`, and per-machine trust the placement in `controller/placement.go`. Keep placement logic there, in one place.

## Commands

```bash
go run ./cmd/tensorafi dev        # local stack on :8080 (macOS or Linux)
go test ./...                   # tests
go vet ./... && golangci-lint run
gofmt -l .                      # must print nothing
make -C .. build-all            # cross-compile for linux/amd64, linux/arm64, darwin/arm64
```
