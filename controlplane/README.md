# controlplane

Einsora's core platform: one Go binary, `einsora`, that runs on macOS (Apple Silicon) and Linux.
The subcommand picks the role:

| Command | Role | Needs | Status |
|---|---|---|---|
| `einsora up` | **Solo**: everything below in one process, with the dashboard built in, on `127.0.0.1:8080`. Its data (SQLite, this machine's identity, workloads, and `einsora.log`) is in one owner-only directory: `EINSORA_DATA_DIR`, or `~/Library/Application Support/Einsora` on macOS and `~/.local/share/einsora` on Linux. The first time, it asks for the owner account | Nothing: every `EINSORA_*` setting overrides a Solo default | Built |
| `einsora api` | REST API under `/v1`, and the dashboard when the binary carries it | A store: a SQLite file or Postgres | Built |
| `einsora controller` | Reconciles workload intent with the runtime. With `EINSORA_CLUSTER_BACKEND=agent`, also runs this machine's agent in-process | A store, plus Kubernetes credentials, or the agent backend | Built |
| `einsora metering` | Usage accounting: lifecycle events → hourly usage records, for quotas and fairness (no money) | A store | Built |
| `einsora migrate` | Applies the SQL migrations embedded in the binary | Postgres, or a SQLite file (also migrated whenever it's opened) | Built |
| `einsora dev` | API, controller, and usage accounting in one process with seeded tenants | Nothing | Built |
| `einsora setup -email you@example.com` | Creates the first account, the machine's owner, for `EINSORA_AUTH_MODE=local`, with a trusted tenant and a quota on every class. Asks for the password without echoing it (or `-password-stdin`). Refused once an account exists. With `-reset-password`, sets a new password for an existing account and signs it out everywhere: the way back in, since there's no email reset. | A store | Built |
| `einsora up` | Solo: everything in one process, with SQLite and the embedded dashboard | Nothing | Planned |
| `einsora agent` | Node agent on other machines (Linux, macOS), joining over the network | An enrolment token | Planned (Phase 2); the agent runs in-process today |

The same binary is a **CLI client** for a running API ([logs and transparency](../docs/architecture/logs-and-transparency.md)):

| Command | Does |
|---|---|
| `einsora workloads [-all] [-json]` | Lists your workloads (`-all` includes deleted ones) |
| `einsora machines [-json]` | Lists your machines: online or not, region, CPUs, memory, GPUs, and your workloads on each |
| `einsora catalog [-json]` | Lists what you can launch: regions (the first is the default), GPU and CPU classes, and images with what runs them |
| `einsora launch <notebook\|endpoint\|vm> <name> -image <id> -class <id> [-gpus n] [-region id] [-model uri] [-ssh-key key-or-file] [-json]` | Launches a workload, pending until it's placed. `-ssh-key` takes a public key or a `.pub` file, and refuses anything else before sending it, so a private key never leaves the machine. |
| `einsora stop <workload> [-json]`, `start <workload> [-json]` | Releases its GPUs (a notebook or VM keeps its disk), or queues a stopped or failed workload again |
| `einsora delete <workload> [-y] [-json]` | Removes it with its disk and data, after asking on the terminal; without a terminal it needs `-y`. Its timeline and export stay. |
| `einsora events <workload> [-json]` | Prints the full timeline: every state change and note, when, why, and by whom |
| `einsora logs <workload> [-f] [-json]` | Prints its output (stdout and stderr), with terminal escape sequences removed; `-f` keeps printing new output until Ctrl+C. Notes about output that isn't collected, or was removed, go to stderr. |
| `einsora export <workload> [-o file] [-f]` | Saves everything recorded about the workload to a JSON file, owner-only (`0600`). It never replaces an existing file without `-f`; `-o -` writes to stdout. Works for deleted workloads too. |

`<workload>` is a workload's ID, or its name when only one of your workloads has it (`events` and
`export` look among deleted ones too, after the rest); several with one name are refused with their IDs.
`-json` prints the API's objects as it sent them, every field included.

The client reads `EINSORA_API_URL` (default `http://127.0.0.1:8080`), `EINSORA_TOKEN` (an API token from the
dashboard's **API tokens** page, `tsk_…`),
`EINSORA_DEV_TENANT` (dev mode), and `EINSORA_INSECURE_HTTP`. It won't send a token over plain `http://` to
another machine unless `EINSORA_INSECURE_HTTP=true`. Errors print the API's message and request ID, and exit
with status 1.

## Run locally

```bash
go run ./cmd/einsora dev
curl -s localhost:8080/v1/workloads -H 'X-Dev-Tenant: acme'
EINSORA_DEV_TENANT=acme go run ./cmd/einsora workloads      # in another terminal
```

Logs go to stderr. `EINSORA_LOG_LEVEL=debug` adds detail, such as each admission check and authenticated
request, and `EINSORA_LOG_FILE=<path>` appends a complete copy to a file. A new file is created `0600`. An
existing file that others can read, a symlink, or a device is refused, so put the log in a directory only
the operator can write to.

`dev` uses the in-memory store and the fake backend unless `EINSORA_DATABASE_URL` or
`EINSORA_CLUSTER_BACKEND` is set. With `EINSORA_CLUSTER_BACKEND=agent`, the controller runs this machine's agent in
its own process ([ADR-0016](../docs/adr/0016-agent-protocol-and-machine-placement.md)): it registers the machine,
owned by the tenant `EINSORA_MACHINE_OWNER` (`home` by default, which `einsora setup` creates), reports its CPUs,
memory, and GPU (an Apple Silicon GPU through sysctl, NVIDIA GPUs through `nvidia-smi`), and runs what the
controller places on it with its drivers (`EINSORA_AGENT_DRIVERS`; empty means every one this machine can run).

- **`macos-native`** ([ADR-0017](../docs/adr/0017-local-workload-access-and-native-drivers.md)), on an Apple Silicon
  Mac with [uv](https://docs.astral.sh/uv/) installed (`brew install uv`), runs notebooks: JupyterLab with PyTorch and
  MLX on Metal, as a process in its own process group under `sandbox-exec`, with its data in
  `native/workloads/<id>/data` next to the SQLite database. It listens on a Unix socket, not a port, and its kernels
  talk over IPC, both in a short run directory (`EINSORA_AGENT_RUN_DIR`, `~/.einsora/run` by default; macOS limits
  socket paths to 104 bytes). Its Python environment is built once per machine from a lock file with hashes
  (`internal/agent/drivers/macosnative/envs/`): a download of a few hundred MB, about 750 MB on disk. Notebooks keep
  running across a `einsora` restart, and the driver finds them again.
  If an upgrade changes the sandbox, the driver stops older servers and kernels and records a
  `sandbox-changed` failure asking you to restart; notebook data is kept.
  On macOS 14+, it also runs **MLX model endpoints** from `hf://org/model` or `s3://bucket/prefix`:
  it fetches only allowed files, validates safetensors and configs, and serves a single model through
  the access proxy. Endpoint weights stay in that workload's own directory and are removed with it.
  Model downloads and loading appear in the timeline. The MLX environment is installed separately
  from notebooks, from a pinned lock with hashes.
- **`sim`** runs nothing and reports workloads as running, in dev only:
  `EINSORA_CLUSTER_BACKEND=agent EINSORA_AGENT_DRIVERS=sim EINSORA_MACHINE_OWNER=internal go run ./cmd/einsora dev`,
  then launch on `apple-silicon-32gb` (or whichever tier this Mac's memory holds) or a CPU class with `X-Dev-Tenant: internal`.

A running notebook opens through the **access proxy** on `EINSORA_ACCESS_ADDR` (`127.0.0.1:8090`), at its own host,
`w-<id>.localhost:8090`: the dashboard's Open button asks the API for a one-time link, and the proxy turns it
into a cookie for that host only and adds the notebook's own token to every request, so the token is never
in a URL.
The machine's ID is saved in `machine-id` next to the SQLite database (or in `EINSORA_AGENT_STATE_DIR`), so a new
hostname is only a new label; with the in-memory store nothing is saved. The agent backend runs with SQLite until
agents join over the network (Phase 2). Only one controller reconciles a region: another started for the same
region stands by until the first stops (a lease in the store). To keep data between restarts without Postgres, use a SQLite
file: `EINSORA_DATABASE_URL=sqlite:$PWD/einsora.db go run ./cmd/einsora dev`. It's created readable by you
only and migrated when opened, and the path in use is logged at startup. The file and its `-wal` and `-shm`
files must stay owner-only, in a directory other users can't write to; otherwise Einsora refuses to start
and prints the `chmod` that fixes it. The fake backend still forgets its workloads on restart, so the controller
marks those that were running as failed, and their timelines say why. To point the controller at a real Kubernetes cluster from
your laptop, use `EINSORA_CLUSTER_BACKEND=kubernetes`. It reads `KUBECONFIG`. Never point it at production.

## API

| Method | Path | Notes |
|---|---|---|
| GET | `/healthz`, `/readyz` | Liveness, and readiness (checks the store) |
| GET | `/v1/catalog` | Regions, and the classes (with accelerator and regions) and images (with each runtime's accelerators and server) visible to the caller's tenant |
| GET | `/v1/machines` | `{"items": [...]}`: your machines, with region, system, CPUs, memory, GPUs, sharing, `status` (online if seen in the last 3 minutes), `last_seen_at`, and how many of your workloads hold each. Read-only: machines register through their agent. |
| GET | `/v1/machines/{id}` | One of your machines; another tenant's is 404 |
| GET | `/v1/workloads` | `{"items": [...]}`. `?include_deleted=true` adds deleted workloads. |
| POST | `/v1/workloads` | `{name, kind, image_id, gpu_class, gpu_count, region?, ssh_public_key?, model_uri?}` → 201. `region` defaults to the catalog's first. `kind` is `vm` (GPU VM, needs `ssh_public_key`), `notebook`, or `endpoint` (needs `model_uri`: `s3://` or `hf://`, with no credentials in it). |
| GET | `/v1/workloads/{id}` | `?include_deleted=true` also finds a deleted workload. A workload shows where it runs once admitted: `region`, `placed_class`, and on agents `machine_id` and `driver`. |
| POST | `/v1/workloads/{id}/stop` | 202; VMs and notebooks keep their disks |
| POST | `/v1/workloads/{id}/start` | 202; re-checks quota |
| POST | `/v1/workloads/{id}/access` | 201 `{url, expires_at}` for a notebook's one-time browser link (60 seconds), or `{url}` for an endpoint's `/v1` API base URL, requiring an API token per request ([ADR-0017](../docs/adr/0017-local-workload-access-and-native-drivers.md)). Agent backend only. |
| DELETE | `/v1/workloads/{id}` | 202; idempotent |
| GET | `/v1/workloads/{id}/events` | `{"items": [{id, from?, to, reason, at}]}`, the full timeline, oldest first. A note has `from` equal to `to`. Works after deletion. |
| GET | `/v1/workloads/{id}/logs` | `?after=<id>&limit=<n>&follow=true`: `{items: [{id, stream, at, text, redactions?}], next, collected, notes}`, the workload's output after the chunk `after`, oldest first (`id` counts this workload's output from 1) ([ADR-0024](../docs/adr/0024-workload-output.md)). `follow=true` waits up to 20 seconds for new output. `notes` say what isn't collected, or was lost or removed. Works after deletion, until it expires. |
| GET | `/v1/workloads/{id}/export` | The complete log as a JSON attachment (`einsora.workload-export/v1`): workload, inputs, timeline, usage (with its machine), `output_logs`, and `redactions`, plus the last placement (`last_placed_class`, `last_machine_id`, `last_driver`). Works after deletion. |
| GET | `/v1/auth/me` | Who's calling: `{subject, email, tenant, auth_mode}` |
| POST | `/v1/auth/login` | Local mode. `{email, password}` → sets the session cookie (HttpOnly, SameSite=Strict, 7 days; over HTTPS, `Secure` and named `__Host-einsora_session`). Needs the `X-Einsora-CSRF` header, and HTTPS or this machine (see below). Repeated failures are slowed down (429 with `Retry-After`). |
| POST | `/v1/auth/logout` | Local mode. Ends the session |
| POST | `/v1/auth/password` | Local mode, signed-in browser only. `{current_password, new_password}`; signs out every other session → `{status, api_tokens_active}`, the API tokens that still work |
| GET, POST | `/v1/tokens` | Local mode. List your API tokens, or create one (signed-in browser only, with your current password): `{name, expires_in_days, password}` (0 days = never) → 201 with `token`, shown only this once |
| DELETE | `/v1/tokens/{id}` | Local mode. Revokes one of your tokens; another user's is 404 |

**Authentication** (`EINSORA_AUTH_MODE`): `local` takes an API token (`Authorization: Bearer tsk_…`) or the
dashboard's session cookie; with the cookie, a request that changes something must also send
`X-Einsora-CSRF` (any value), which other sites can't add. Passwords and session cookies are accepted only
over HTTPS or from this machine: behind a reverse proxy that terminates HTTPS, list its own addresses (not a
whole network) in `EINSORA_TRUSTED_PROXIES`, which also makes its `X-Forwarded-For` count for sign-in limits.
Every listed address can claim HTTPS and any client address. The proxy must
send `X-Forwarded-For` and `X-Forwarded-Proto`, and sign-in through a proxy that isn't listed is refused,
even on this machine. `EINSORA_INSECURE_HTTP=true` allows plain HTTP from other machines on a network you
trust. API tokens
can't create tokens or change the password, and timelines name the credential that acted. `oidc` takes an
identity provider's token, and `dev` trusts the `X-Dev-Tenant` header (dev only).

Errors look like `{"error": {"code": "quota_exceeded", "message": "...", "request_id": "..."}}`, and every
response carries an `X-Request-ID` header generated by the server. The design behind the
lifecycle, admission, and usage accounting is in [docs/architecture/control-plane.md](../docs/architecture/control-plane.md).

**Planned** (ADR-0009, 0010, 0012, 0013, 0014; see the roadmap):

- create takes `fallback` and `fallback_after`
- responses include `machine` (they already include `region` and, once admitted, `placed_class`)
- new resources: machines (stored, not yet exposed), more users with invites and teams (Phase 2), and sharing settings
- `POST` accepts `Idempotency-Key`, and 503s carry `Retry-After`

## Build

```bash
make -C .. build                               # for this machine, with the dashboard built in: bin/einsora
go build -o bin/einsora ./cmd/einsora          # the same, without the dashboard (the API works)
make -C .. build-all                           # linux/amd64, linux/arm64, darwin/arm64 into bin/, with the dashboard
docker build -t einsora-controlplane .         # Linux container image (Datacenter profile)
```

On Apple Silicon, the optional real-notebook test reuses a Jupyter environment already built by the
driver from its embedded lock. It starts a sandboxed kernel, checks IPC, and executes PyTorch MPS and
MLX Metal operations without downloading packages. From `controlplane/`:

```bash
EINSORA_TEST_NATIVE_ENV="/absolute/path/to/native/envs/jupyterlab-pytorch-mps-<hash>" \
  go test ./internal/agent/drivers/macosnative -run TestRealNotebook -count=1 -v
```

For the real endpoint integration check, install `uv sync --extra serve` in `ml/`, then run from
`controlplane/`:

```bash
EINSORA_TEST_NATIVE_ENDPOINT_ENV="$PWD/../ml/.venv" \
  go test ./internal/agent/drivers/macosnative -run TestRealEndpoint -count=1 -v
```

It creates a tiny random model, fetches it from a local S3 fixture, and tests safetensors validation,
MLX Metal, chat completion, and SSE under the sandbox, without downloading model weights. After
changing Python runtime sources, regenerate `runtime.zip` with `python3 ml/scripts/bundle_native.py`
from the repository root; the native tests reject stale bundles.
