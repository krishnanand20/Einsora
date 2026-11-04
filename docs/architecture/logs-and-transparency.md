# Logs and transparency

Einsora keeps detailed logs and lets the person entitled to them read or download all of it.
Nothing is hidden except secrets, and every redaction is reported ([ADR-0015](../adr/0015-logs-and-transparency.md)).

## What you can get, and how

| You are | You can see | CLI | Dashboard | API |
|---|---|---|---|---|
| **A user** | Your workloads, including deleted ones | `einsora workloads -all` | Workloads → **Show deleted** | `GET /v1/workloads?include_deleted=true` |
| **A user** | Your workloads' full timeline: every state change and note, when, why, and by whom | `einsora events <workload>` (`-json` for raw) | Workload page → Timeline | `GET /v1/workloads/{id}/events` |
| **A user** | A complete copy of everything recorded about a workload, including deleted ones | `einsora export <workload> [-o file] [-f]` | Workload page → **Download full log** | `GET /v1/workloads/{id}/export` |
| **A user** | Your workloads' output: stdout and stderr, kept 14 days ([below](#workload-output)) | `einsora logs <workload> [-f]` | Workload page → Output | `GET /v1/workloads/{id}/logs` |
| **A tenant admin** | The activity log: everything anyone in the tenant did | Planned | Planned | Planned |
| **An operator** | Every log line from the control plane and agents, at any detail level | `EINSORA_LOG_LEVEL=debug`, `EINSORA_LOG_FILE=/path` | — | — |
| **Anyone with an error** | A request ID that finds the exact log lines | Printed with every CLI error | Shown in error messages | `X-Request-ID` header, `request_id` in error bodies |

## Workload output

What a notebook or model server prints, stdout and stderr, as it printed it
([ADR-0024](../adr/0024-workload-output.md)). The machine's agent sends it to the control plane within a
second or two, whole lines at a time, and the control plane keeps it in its store, for the workload's
owner only.

- **Credentials are removed on the way in**: a value in a URL that may be a credential, such as a
  notebook server's `?token=`, becomes `REDACTED`, as everywhere else. The export lists how many.
  Output is sent in whole lines, so a URL arrives whole; one in a line longer than 256 KiB, or in a
  line printed in parts more than two seconds apart, can be missed.
- **Kept 14 days** (`EINSORA_OUTPUT_RETENTION`), **and up to 64 MiB per workload**
  (`EINSORA_OUTPUT_MAX_MIB`, at most 256); past that, the oldest output goes. Whatever is removed, or never arrived
  from the machine, is said: the API's `notes`, the CLI, the dashboard, and the export all say how much,
  and a note in the output marks where output was lost.
- **Deleted workloads** keep their output until it expires. Deleting one waits until its last output
  has reached the control plane (its status says so meanwhile), so nothing it printed is lost unsaid.
- **Not collected:** the inside of VMs (below), and runtimes that don't send it yet: Kubernetes
  (Loki, Phase 3) and, for now, the Linux docker driver (roadmap step 14b). Reading those says so.

```bash
einsora logs lab           # everything kept
einsora logs lab -f        # and new output as it comes, until Ctrl+C
einsora logs lab -json     # one JSON object per piece: id, stream, at, text, redactions
```

`einsora logs` removes terminal escape sequences (colours, titles, cursor movement) before printing,
so output can't control your terminal; `-json` keeps them, escaped. The dashboard shows the most recent
1 MiB and says when it leaves earlier output out; **Download full log** has all of it.

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

`einsora export <id>` writes a single JSON file (`einsora.workload-export/v1`). The CLI creates it readable by
the owner only (`0600`), never replaces an existing file unless you pass `-f`, and uses the server's suggested
name only if it has the expected shape (`einsora-<name>-<id>.json`). A dashboard download is saved by your
browser, with whatever permissions it uses.

| Field | Contents |
|---|---|
| `format`, `exported_at`, `exported_by` | What this file is, when it was made, and by whom |
| `workload` | The workload as the API shows it, including its state (even `deleted`) |
| `last_placed_class` | The stored placement. A workload that's waiting again hides it in `workload` (it isn't where the next run will go), so it's kept here. |
| `created_by`, `params` | Who created it, and every input it was created with |
| `events` | The complete timeline, oldest first |
| `usage` | Every hourly usage record: seconds, GPU-seconds, the class that actually ran, and region (usage accounting; never money) |
| `output_logs` | All of the workload's kept output: `items` (id, stream, time received, text, and redactions), each stream's `stored_bytes`, `lost_bytes`, `trimmed_bytes`, and `expired_bytes`, `available` (whether its output is collected), and `notes` saying what isn't collected or was removed |
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
- **Output from runtimes that don't send it yet** (Kubernetes, and for now Linux docker): the export's `output_logs` says so.

## Examples

```bash
$ einsora events 01a16a2a-d622-7d69-94ec-23b0f3327338
TIME (UTC)            CHANGE                  REASON
2026-10-23T19:09:14Z  ∅ → pending             created by dev@acme
2026-10-23T19:09:15Z  pending → provisioning  admitted: 2 × l40s-48gb
2026-10-23T19:09:15Z  provisioning → running  ready

$ einsora export 01a16a2a-d622-7d69-94ec-23b0f3327338
wrote einsora-llm-01a16a2a-d622-7d69-94ec-23b0f3327338.json (1008 bytes)

$ einsora workloads -all       # includes deleted workloads, whose history stays available

$ make dev EINSORA_LOG_LEVEL=debug EINSORA_LOG_FILE=$PWD/einsora.log   # operators: everything, also to a file
```

The CLI reads `EINSORA_API_URL`, `EINSORA_TOKEN`, and `EINSORA_DEV_TENANT`. It refuses to send a token over
plain `http://` to another machine. Use `https://`, or set `EINSORA_INSECURE_HTTP=true` if you trust every
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
| Workload output | Agents send it to the control plane, which keeps it in the store (built for macos-native; docker next) | Loki (Alloy ships pod logs), read by the API through the log store (planned) |
| System logs | stderr and `EINSORA_LOG_FILE` | stderr, collected by Loki |

The API reads output logs from the log store, never from the runtime directly (rule 1).
