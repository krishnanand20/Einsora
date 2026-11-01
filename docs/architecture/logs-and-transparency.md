# Logs and transparency

Tensora keeps detailed logs and lets the person entitled to them read or download all of it.
Nothing is hidden except secrets, and every redaction is reported ([ADR-0015](../adr/0015-logs-and-transparency.md)).

## What you can get, and how

| You are | You can see | CLI | Dashboard | API |
|---|---|---|---|---|
| **A user** | Your workloads, including deleted ones | `tensora workloads -all` | Workloads → **Show deleted** | `GET /v1/workloads?include_deleted=true` |
| **A user** | Your workloads' full timeline: every state change and note, when, why, and by whom | `tensora events <id>` (`-json` for raw) | Workload page → Timeline | `GET /v1/workloads/{id}/events` |
| **A user** | A complete copy of everything recorded about a workload, including deleted ones | `tensora export <id> [-o file] [-f]` | Workload page → **Download full log** | `GET /v1/workloads/{id}/export` |
| **A user** | Your workloads' output (stdout and stderr; never collected from inside VMs, see below) | `tensora logs <id> --follow` (planned) | Planned | Planned |
| **A tenant admin** | The activity log: everything anyone in the tenant did | Planned | Planned | Planned |
| **An operator** | Every log line from the control plane and agents, at any detail level | `TENSORA_LOG_LEVEL=debug`, `TENSORA_LOG_FILE=/path` | — | — |
| **Anyone with an error** | A request ID that finds the exact log lines | Printed with every CLI error | Shown in error messages | `X-Request-ID` header, `request_id` in error bodies |

## What the timeline says

Each entry is a state change (`pending → queued`) or a **note**, which records a decision that leaves the
state as it was (`still pending`). Reasons are written for you, from a fixed vocabulary:

| Situation | Example reason |
|---|---|
| You acted, with a local account (the credential is named, never its secret) | `created by you@example.com (API token "laptop" (tsk_Ab3x…))`, `stop requested by you@example.com (browser session)`, `opening in the browser requested by you@example.com (browser session)` (a note) |
| Waiting for GPUs (trusted tenant: a machine's owner and their team) | `waiting for l40s-48gb capacity: requested 2 GPU(s), 0 free` |
| Waiting for GPUs (invited or community user) | `waiting for l40s-48gb capacity: requested 2 GPU(s), not enough free right now` |
| The runtime refused an action and it's being retried (a note) | `couldn't start it on the runtime; retrying. The operator's log has the details under this workload's ID.` |
| The runtime can't be reached (a note) | `couldn't read its status from the runtime; retrying. …` or `couldn't check for free GPUs on the runtime; retrying. …` |
| Something retrying can't fix | `can't start: GPU class "x" is no longer offered by this installation` |
| A model server that isn't ready | `model server not ready (RevisionFailed)` |
| Placed on a machine (node agents) | `admitted: 1 × apple-silicon-32gb on studio with macos-native`: the class, the machine, and the driver |
| Waiting for a machine | `waiting for a machine that can run apple-silicon-32gb to come online`; when the tenant's machines are online but none has a driver for it, `waiting for a machine with a driver that runs notebooks on cpu-4c-16g: none here has one yet`; or for a VM or notebook that ran before, `waiting for its machine to come online: its disk is there` |
| The machine is offline (a note) | `its machine is offline; waiting for it to reconnect` |
| What the machine's agent reports | A fixed reason for each code it sends, such as `waiting for its machine's GPU to be free` or `it ran out of memory`. An agent's own words never appear; a code the control plane doesn't know reads `its machine reported a problem`. |

Raw runtime messages from Kubernetes, KServe, or agents never appear in a timeline. They can name internal
objects and machines, describe other people's use of the cluster, or echo credentials. The operator's log
keeps them, with credentials redacted, under the workload's ID. Invited and community users don't see free-GPU
counts, because a count would show when other people use the machines.

## The export file

`tensora export <id>` writes a single JSON file (`tensora.workload-export/v1`). The CLI creates it readable by
the owner only (`0600`), never replaces an existing file unless you pass `-f`, and uses the server's suggested
name only if it has the expected shape (`tensora-<name>-<id>.json`). A dashboard download is saved by your
browser, with whatever permissions it uses.

| Field | Contents |
|---|---|
| `format`, `exported_at`, `exported_by` | What this file is, when it was made, and by whom |
| `workload` | The workload as the API shows it, including its state (even `deleted`) |
| `last_placed_class` | The stored placement. A workload that's waiting again hides it in `workload` (it isn't where the next run will go), so it's kept here. |
| `created_by`, `params` | Who created it, and every input it was created with |
| `events` | The complete timeline, oldest first |
| `usage` | Every hourly usage record: seconds, GPU-seconds, the class that actually ran, and region (usage accounting; never money) |
| `output_logs` | Workload output once the pipeline exists. Until then, `available: false` with a note saying so. |
| `redactions` | Every value that was removed, and why. Empty when nothing was redacted. |

## What's redacted, and only that

- **Credentials:** `user:password@` inside URIs, and query or fragment parameters that look like credentials (names containing token, secret, password, signature, credential, auth, session, or key). Each value is replaced with `REDACTED` and listed in `redactions` with the reason. The rest of the value is kept byte for byte, and SSH public keys are kept because they're public.
- **Anything that can't be read reliably** where credentials could be is redacted too, and listed. When a URI's host part has an `@` or a colon, everything between `://` and the last `@` is removed, because an unescaped password can contain `/`, `?`, or `#`. A malformed host or parameter name is removed too. New model URIs that could carry credentials (an `@` anywhere, or a port) are refused when the workload is created.
- **Other tenants' data:** never part of your logs (tenant scoping, 404).
- **Internal error details** in API responses: replaced with a request ID. The operator's log keeps the full error.

Everything else is included.

## What's never collected (and said so)

For privacy, some things are never collected, so they can't be in any log. The product says so wherever it applies, rather than showing nothing:

- **The inside of your VMs.** You have root, so read their logs over SSH. The timeline still shows every step the platform took.
- **Guest workloads on community hosts:** only the driver's own status lines ([observability](observability.md#user-data-in-logs)).
- **Until output capture lands**, the export's `output_logs` says it isn't captured yet.

## Examples

```bash
$ tensora events 01a16a2a-d622-7d69-94ec-23b0f3327338
TIME (UTC)            CHANGE                  REASON
2026-10-23T19:09:14Z  ∅ → pending             created by dev@acme
2026-10-23T19:09:15Z  pending → provisioning  admitted: 2 × l40s-48gb
2026-10-23T19:09:15Z  provisioning → running  ready

$ tensora export 01a16a2a-d622-7d69-94ec-23b0f3327338
wrote tensora-llm-01a16a2a-d622-7d69-94ec-23b0f3327338.json (1008 bytes)

$ tensora workloads -all       # includes deleted workloads, whose history stays available

$ make dev TENSORA_LOG_LEVEL=debug TENSORA_LOG_FILE=$PWD/tensora.log   # operators: everything, also to a file
```

The CLI reads `TENSORA_API_URL`, `TENSORA_TOKEN`, and `TENSORA_DEV_TENANT`. It refuses to send a token over
plain `http://` to another machine. Use `https://`, or set `TENSORA_INSECURE_HTTP=true` if you trust every
network in between. It never follows redirects, so a token can't be carried somewhere else, and it shows
server error text with control characters removed.

## Request IDs

The server generates an ID for every request and returns it in `X-Request-ID` and in every error body. Give it
to an operator and they can find every log line for that request. Each request's log line also names the tenant
and the person who made it, so an operator can check the ID belongs to you before sharing details. If you send
your own `X-Request-ID`, it's logged as `client_request_id` for correlation and never replaces the server's.

## Where logs live (by profile)

| Stream | Solo and Lab | Datacenter |
|---|---|---|
| Timeline, usage | The store (SQLite or Postgres) | Postgres |
| Workload output (planned) | Agents stream it to the control plane, which keeps it per workload | Loki (Alloy ships pod logs), read by the API through the log store |
| System logs | stderr and `TENSORA_LOG_FILE` | stderr, collected by Loki |

The API reads output logs from the log store, never from the runtime directly (rule 1).
