# 0015. Logs and transparency: complete, exportable, nothing hidden except secrets

- **Status:** Accepted (timeline, export, CLI, request IDs, and log file built; output logs built for macos-native, [ADR-0024](0024-workload-output.md); activity log planned)
- **Date:** 2026-10-24

## Context

People need to trust a system that runs their work on machines they may not own. That requires detailed
logs they can read from the CLI or dashboard, and a full copy they can download, with nothing quietly
withheld. Operators and hosts need the same for their side.

## Decision

### Four log streams

| Stream | Contents | Who sees it | Status |
|---|---|---|---|
| **Workload timeline** | Every state change, with time, reason, and who triggered it, plus notes for decisions that don't change state (such as "retrying"). Includes placement and fallback decisions and why a workload is waiting. | The workload's tenant | **Built:** `GET /v1/workloads/{id}/events`, `tensora events` |
| **Workload output** | stdout and stderr of notebooks, model servers, and jobs. Not collected from inside VMs (you have root, so read them over SSH), nor from guest workloads on community hosts beyond driver status lines ([observability](../architecture/observability.md#user-data-in-logs)). | The workload's tenant | Planned: agents (Phases 1–2), Loki in a datacenter (Phase 3) |
| **Activity log** | Every API action by every person in a tenant: launches, stops, deletes, sign-ins, token changes, each with a request ID | The tenant's admins | Planned (Phase 2) |
| **System logs** | Control plane and agent process logs (`slog`), with detail in debug mode | The operator; hosts for their own agent | **Built:** `TENSORA_LOG_LEVEL=debug`, `TENSORA_LOG_FILE` |

### Rules

- **Nothing hidden from its owner.** Every decision the platform makes about a workload is recorded with a human-readable reason in its timeline. No stream is sampled, truncated, or filtered for the person entitled to it.
- **Not collected is not hidden, but it is said.** Some data is deliberately never collected, to protect privacy (the inside of VMs, community guests' output). Wherever that applies, the timeline, dashboard, and export say so explicitly instead of showing an empty list.
- **Reasons are written for the owner, from a fixed vocabulary.** Runtime messages (from Kubernetes, KServe, or agents) are never copied into a timeline. They can name internal objects and machines, describe other people's use of the cluster, or echo credentials. A runtime failure becomes a fixed reason such as `couldn't start it on the runtime; retrying`. The raw error goes to the operator's log, keyed by workload ID, with credentials redacted. As a backstop, every reason is cleaned before it's stored: control characters become spaces, credentials in URIs are redacted, and very long text is cut with a marker saying so.
- **Detail about shared capacity depends on trust.** Trusted tenants (a machine's owner and their team) see how many GPUs are free when they wait. Invited and community users see `not enough free right now`, because a count would show when other people use the machines.
- **Complete export.** `GET /v1/workloads/{id}/export` (`tensora export`, and "Download full log" in the dashboard) returns one JSON file with everything recorded about a workload: the workload, its inputs, the full timeline, usage records, output logs once they exist, and a list of redactions. Deleted workloads stay listed (`include_deleted=true`, `tensora workloads -all`, "Show deleted" in the dashboard), readable, and exportable.
- **Only secrets are redacted, and never silently.** Credentials (`user:password@` in URIs, and token, signature, or key parameters in the query or fragment) are replaced with `REDACTED`. Redaction edits the raw text, so everything else stays byte for byte. It fails closed: a part of a URI that can't be read reliably is removed too. Every redaction is listed in the export's `redactions` field. Model URIs carrying credentials are refused at create time, so new workloads never store them.
- **Isolation still applies.** Logs follow tenant scoping: another tenant's request returns 404 (rule 8). On shared machines, hosts don't get guests' output through the product (ADR-0014 discloses that a host could technically see it).
- **Request IDs everywhere.** Every response carries `X-Request-ID`, and every error body includes `request_id`. The same ID appears on every log line for that request, so a user can hand it to an operator. The server always generates the ID, so it points at exactly one request. A caller's own `X-Request-ID` is logged as `client_request_id`. Request log lines name the tenant and subject, so an operator can check that whoever hands them an ID owns that request. A request ID is not proof of identity.
- **Internal errors:** users get a generic message plus the request ID. The operator's log has the full error. Internals never leak into responses.
- **Retention defaults:** timeline and usage are kept for the workload's lifetime and after deletion (they're small). Output logs are kept 14 days. System logs follow the operator's choice. All are configurable.

## Consequences

- New architecture rule 14 in AGENTS.md.
- New tenant-scoped store methods `GetWorkloadHistory`, `ListWorkloadHistory`, and `EventsForWorkload`, so deleted workloads stay readable by their owner and request paths never use the system-wide `Events`.
- Timeline entries can be **notes** (`from` equals `to`, written by `service.Note`). Usage accounting ignores them, because only changes into or out of a metered state matter to it.
- `cluster.Backend.Observe` returns reasons from a fixed vocabulary. Controller failures that are retried, including a runtime that can't be reached, get one note in the timeline instead of only a line in the operator's log. Usage accounting skips notes by construction (`from == to`).
- Output logs need a pipeline that respects rule 1 (the API never talks to the runtime): runtimes (agents, Kubernetes via Loki) ship logs to a per-region log store, and the API reads only from that store.
- The `tensora` binary gains client commands (`workloads`, `events`, `export`). Its `tensora logs <id> --follow` command arrives with output logs.
- `tensora export` now means one workload's log. ADR-0012's planned profile move (`tensora export` and `tensora import`) becomes `tensora backup` and `tensora restore`. A backup is portable between SQLite and Postgres.
- Exports may contain personal data (subjects, emails). The CLI writes them owner-only (`0600`), never replaces an existing file unless asked (`-f`), and accepts a file name from the server only if it has the expected shape. A browser download's permissions are up to the browser.
- The CLI refuses to send a token over plain HTTP to another machine unless `TENSORA_INSECURE_HTTP=true`, and never follows redirects.
- `TENSORA_LOG_FILE` must be readable by its owner only. The server creates it `0600` and refuses an existing file that others can read, a symlink, or a file swapped while it was being opened. It never changes a file's permissions.

## Alternatives considered

- **Operator-only logs:** simpler, but users can't verify or debug what happened to their work.
- **Dropping debug logs in production:** saves disk, but makes incidents unexplainable. Instead, detail is configurable and nothing is sampled.
