# Playbook: add an API resource

Used by humans, Codex, and the Claude Code `/add-api-resource` skill. Example: adding `machines` or `models`.

## 0. Decide

- What are the fields, and which are user-supplied? How is each validated?
- Is it tenant-scoped? (It almost always is.)
- Does it have a lifecycle? If so, use explicit states, transitions, and events, like workloads.
- Does it run anything, through Kubernetes objects, agent driver specs, or ML layer jobs? If so, the controller does it, never the API. Which **trust levels** may use it (the trust matrix)?
- Does it affect quotas or usage accounting? If so, update [usage-accounting.md](../architecture/usage-accounting.md) and get a `tenancy-reviewer` pass. There is no billing, so never add prices or payment fields (rule 12).
- Is it **regional** (it lives in one region, like workloads, machines, and models) or **global** (like users, teams, and API keys)? Regional resources carry an immutable `region`, and their data stays in that region (rule 9, ADR-0009).
- Could a **substitute** ever serve it (another class, CPU, machine, model, or region)? If so, the substitute must be opt-in and visible in responses and usage accounting (rule 10, ADR-0010).
- Does it touch a host's machine or a shared machine? Then the host protections apply (rule 13, ADR-0014).

## 1. Schema

Add `controlplane/migrations/postgres/NNNNN_<name>.sql` and the same change in
`controlplane/migrations/sqlite/NNNNN_<name>.sql`, each with `-- +goose Up` and `-- +goose Down` sections
(`TestDialectsMatch` fails if one is missing). Include a `tenant_id` column referencing `tenants(id)`, an index
on `(tenant_id, created_at)`, and CHECK constraints for enums. In SQLite, use `STRICT` tables, text UUIDs, and
fixed-width UTC text times, as `00001_init.sql` does (ADR-0012). Add the new table, columns, and indexes to
`SchemaColumns` and `SchemaIndexes` in `internal/store/storetest`: both stores' tests compare their database
with that list, so a migration written for only one database fails.

## 2. Domain

Add types to `internal/domain/`. If there's a lifecycle, add a state table and a test mirroring `state_test.go`.

## 3. Store

1. Add the methods to `store.Queries`. Tenant-scoped methods take `tenantID` and filter by it.
2. Implement them in **every** store: `memstore`, `pgstore`, and `sqlitestore`.
3. Add a case to `storetest` (including tenant scoping), so every implementation is tested the same way.

## 4. Service

Add `internal/service/<resource>.go`:

- an input struct with JSON tags and a `validate()` method: lengths, character sets, URI schemes, no control characters
- methods that take `domain.Principal` and use `withTenant` or `withWorkload`-style helpers
- expected failures returned as `domain.Errorf(code, …)`
- a specific, human-readable reason for every state change or decision (rule 14), written by you, never copied from a runtime message or error. If the resource takes inputs that can hold secrets, redact them wherever they're exported, and list each redaction.

## 5. API

1. Add view structs to `internal/api/json.go`. Never return domain structs directly, and never expose internals (host paths, machine hostnames, object names, IPs).
2. Add handlers to `server.go` and register routes under `/v1`. Async actions return 202.
3. Add tests to `server_test.go`: the happy path, validation errors, **cross-tenant 404**, unknown fields rejected, and region validation for regional resources.
4. `POST` handlers honour `Idempotency-Key` once that exists (ADR-0010). Never add a non-idempotent create without it.

## 6. Runtime (if it runs anything)

1. **Datacenter:** add a pure builder in `internal/cluster/manifests/` and unit-test it (`CheckJSONTypes`, labels, namespace, hardening). Add any new kinds to `kube.allowed` **and** the controller ClusterRole (`deploy/charts/einsora/templates/controller.yaml`).
2. **Agents** (once they exist): add a pure spec builder for the driver, and unit-test it. Put OS-specific code behind build tags in the driver package (rule 11), and enforce the trust matrix.
3. Handle the new kind in a controller step, with tests using `fake.Backend`.

## 7. Dashboard and docs

- Update `web/src/api/types.ts` and `web/src/api/client.ts`, plus a page or component if the resource is user-facing.
- Update [control-plane.md](../architecture/control-plane.md) (lifecycle, API conventions) and `controlplane/README.md` (endpoint list).
- If users need it from a terminal, add a method to `internal/client` and a command to `cmd/einsora/client.go`, with a test against the real API.
- If it has history users should see, make it readable and exportable ([logs and transparency](../architecture/logs-and-transparency.md)).

## 8. Verify

Run [verify-changes.md](verify-changes.md), including `make build-all`, then the `tenancy-reviewer`.
