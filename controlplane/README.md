# controlplane

Tensora's core platform: one Go binary, `tensora`, that runs on macOS (Apple Silicon) and Linux.
The subcommand picks the role:

| Command | Role | Needs | Status |
|---|---|---|---|
| `tensora api` | REST API under `/v1` | A store: a SQLite file or Postgres | Built |
| `tensora controller` | Reconciles workload intent with the runtime. With `TENSORA_CLUSTER_BACKEND=agent`, also runs this machine's agent in-process | A store, plus Kubernetes credentials, or the agent backend | Built |
| `tensora metering` | Usage accounting: lifecycle events → hourly usage records, for quotas and fairness (no money) | A store | Built |
| `tensora migrate` | Applies the SQL migrations embedded in the binary | Postgres, or a SQLite file (also migrated whenever it's opened) | Built |
| `tensora dev` | API, controller, and usage accounting in one process with seeded tenants | Nothing | Built |
| `tensora setup -email you@example.com` | Creates the first account, the machine's owner, for `TENSORA_AUTH_MODE=local`, with a trusted tenant and a quota on every class. Asks for the password without echoing it (or `-password-stdin`). Refused once an account exists. With `-reset-password`, sets a new password for an existing account and signs it out everywhere: the way back in, since there's no email reset. | A store | Built |
| `tensora up` | Solo: everything in one process, with SQLite and the embedded dashboard | Nothing | Planned |
| `tensora agent` | Node agent on other machines (Linux, macOS), joining over the network | An enrolment token | Planned (Phase 2); the agent runs in-process today |

The same binary is a **CLI client** for a running API ([logs and transparency](../docs/architecture/logs-and-transparency.md)):

| Command | Does |
|---|---|
| `tensora workloads [-all]` | Lists your workloads (`-all` includes deleted ones) |
| `tensora events <id> [-json]` | Prints the full timeline: every state change and note, when, why, and by whom |
| `tensora export <id> [-o file] [-f]` | Saves everything recorded about the workload to a JSON file, owner-only (`0600`). It never replaces an existing file without `-f`; `-o -` writes to stdout. Works for deleted workloads too. |

The client reads `TENSORA_API_URL` (default `http://127.0.0.1:8080`), `TENSORA_TOKEN` (an API token from the
dashboard's **API tokens** page, `tsk_…`),
`TENSORA_DEV_TENANT` (dev mode), and `TENSORA_INSECURE_HTTP`. It won't send a token over plain `http://` to
another machine unless `TENSORA_INSECURE_HTTP=true`. Errors print the API's message and request ID, and exit
with status 1.

## Run locally

```bash
go run ./cmd/tensora dev
curl -s localhost:8080/v1/workloads -H 'X-Dev-Tenant: acme'
TENSORA_DEV_TENANT=acme go run ./cmd/tensora events <id>     # in another terminal
```

Logs go to stderr. `TENSORA_LOG_LEVEL=debug` adds detail, such as each admission check and authenticated
request, and `TENSORA_LOG_FILE=<path>` appends a complete copy to a file. A new file is created `0600`. An
existing file that others can read, a symlink, or a device is refused, so put the log in a directory only
the operator can write to.

`dev` uses the in-memory store and the fake backend unless `TENSORA_DATABASE_URL` or
`TENSORA_CLUSTER_BACKEND` is set. With `TENSORA_CLUSTER_BACKEND=agent`, the controller runs this machine's agent in
its own process ([ADR-0016](../docs/adr/0016-agent-protocol-and-machine-placement.md)): it registers the machine,
owned by the tenant `TENSORA_MACHINE_OWNER` (`home` by default, which `tensora setup` creates), reports its CPUs,
memory, and GPU (an Apple Silicon GPU through sysctl, NVIDIA GPUs through `nvidia-smi`), and runs what the
controller places on it with its drivers (`TENSORA_AGENT_DRIVERS`; empty means every one this machine can run).

- **`macos-native`** ([ADR-0017](../docs/adr/0017-local-workload-access-and-native-drivers.md)), on an Apple Silicon
  Mac with [uv](https://docs.astral.sh/uv/) installed (`brew install uv`), runs notebooks: JupyterLab with PyTorch and
  MLX on Metal, as a process in its own process group under `sandbox-exec`, with its data in
  `native/workloads/<id>/data` next to the SQLite database. It listens on a Unix socket, not a port, and its kernels
  talk over IPC, both in a short run directory (`TENSORA_AGENT_RUN_DIR`, `~/.tensora/run` by default; macOS limits
  socket paths to 104 bytes). Its Python environment is built once per machine from a lock file with hashes
  (`internal/agent/drivers/macosnative/envs/`): a download of a few hundred MB, about 750 MB on disk. Notebooks keep
  running across a `tensora` restart, and the driver finds them again. Endpoints are next (roadmap step 7b).
  If an upgrade changes the sandbox, the driver stops older servers and kernels and records a
  `sandbox-changed` failure asking you to restart; notebook data is kept.
- **`sim`** runs nothing and reports workloads as running, in dev only:
  `TENSORA_CLUSTER_BACKEND=agent TENSORA_AGENT_DRIVERS=sim TENSORA_MACHINE_OWNER=internal go run ./cmd/tensora dev`,
  then launch on `apple-silicon-32gb` (or whichever tier this Mac's memory holds) or a CPU class with `X-Dev-Tenant: internal`.

A running notebook opens through the **access proxy** on `TENSORA_ACCESS_ADDR` (`127.0.0.1:8090`), at its own host,
`w-<id>.localhost:8090`: the dashboard's Open button asks the API for a one-time link, and the proxy turns it
into a cookie for that host only and adds the notebook's own token to every request, so the token is never
in a URL.
The machine's ID is saved in `machine-id` next to the SQLite database (or in `TENSORA_AGENT_STATE_DIR`), so a new
hostname is only a new label; with the in-memory store nothing is saved. The agent backend runs with SQLite until
agents join over the network (Phase 2). Only one controller reconciles a region: another started for the same
region stands by until the first stops (a lease in the store). To keep data between restarts without Postgres, use a SQLite
file: `TENSORA_DATABASE_URL=sqlite:$PWD/tensora.db go run ./cmd/tensora dev`. It's created readable by you
only and migrated when opened, and the path in use is logged at startup. The file and its `-wal` and `-shm`
files must stay owner-only, in a directory other users can't write to; otherwise Tensora refuses to start
and prints the `chmod` that fixes it. The fake backend still forgets its workloads on restart, so the controller
marks those that were running as failed, and their timelines say why. To point the controller at a real Kubernetes cluster from
your laptop, use `TENSORA_CLUSTER_BACKEND=kubernetes`. It reads `KUBECONFIG`. Never point it at production.

## API

| Method | Path | Notes |
|---|---|---|
| GET | `/healthz`, `/readyz` | Liveness, and readiness (checks the store) |
| GET | `/v1/catalog` | Regions, and the classes (with accelerator and regions) and images (with each runtime's accelerators and server) visible to the caller's tenant |
| GET | `/v1/workloads` | `{"items": [...]}`. `?include_deleted=true` adds deleted workloads. |
| POST | `/v1/workloads` | `{name, kind, image_id, gpu_class, gpu_count, region?, ssh_public_key?, model_uri?}` → 201. `region` defaults to the catalog's first. `kind` is `vm` (GPU VM, needs `ssh_public_key`), `notebook`, or `endpoint` (needs `model_uri`: `s3://` or `hf://`, with no credentials in it). |
| GET | `/v1/workloads/{id}` | `?include_deleted=true` also finds a deleted workload. A workload shows where it runs once admitted: `region`, `placed_class`, and on agents `machine_id` and `driver`. |
| POST | `/v1/workloads/{id}/stop` | 202; VMs and notebooks keep their disks |
| POST | `/v1/workloads/{id}/start` | 202; re-checks quota |
| POST | `/v1/workloads/{id}/access` | 201 `{url, expires_at}`: a one-time link that opens a running notebook in the browser through the access proxy, valid for 60 seconds ([ADR-0017](../docs/adr/0017-local-workload-access-and-native-drivers.md)). Agent backend only. |
| DELETE | `/v1/workloads/{id}` | 202; idempotent |
| GET | `/v1/workloads/{id}/events` | `{"items": [{id, from?, to, reason, at}]}`, the full timeline, oldest first. A note has `from` equal to `to`. Works after deletion. |
| GET | `/v1/workloads/{id}/export` | The complete log as a JSON attachment (`tensora.workload-export/v1`): workload, inputs, timeline, usage (with its machine), `output_logs`, and `redactions`, plus the last placement (`last_placed_class`, `last_machine_id`, `last_driver`). Works after deletion. |
| GET | `/v1/auth/me` | Who's calling: `{subject, email, tenant, auth_mode}` |
| POST | `/v1/auth/login` | Local mode. `{email, password}` → sets the session cookie (HttpOnly, SameSite=Strict, 7 days; over HTTPS, `Secure` and named `__Host-tensora_session`). Needs the `X-Tensora-CSRF` header, and HTTPS or this machine (see below). Repeated failures are slowed down (429 with `Retry-After`). |
| POST | `/v1/auth/logout` | Local mode. Ends the session |
| POST | `/v1/auth/password` | Local mode, signed-in browser only. `{current_password, new_password}`; signs out every other session → `{status, api_tokens_active}`, the API tokens that still work |
| GET, POST | `/v1/tokens` | Local mode. List your API tokens, or create one (signed-in browser only, with your current password): `{name, expires_in_days, password}` (0 days = never) → 201 with `token`, shown only this once |
| DELETE | `/v1/tokens/{id}` | Local mode. Revokes one of your tokens; another user's is 404 |

**Authentication** (`TENSORA_AUTH_MODE`): `local` takes an API token (`Authorization: Bearer tsk_…`) or the
dashboard's session cookie; with the cookie, a request that changes something must also send
`X-Tensora-CSRF` (any value), which other sites can't add. Passwords and session cookies are accepted only
over HTTPS or from this machine: behind a reverse proxy that terminates HTTPS, list its own addresses (not a
whole network) in `TENSORA_TRUSTED_PROXIES`, which also makes its `X-Forwarded-For` count for sign-in limits.
Every listed address can claim HTTPS and any client address. The proxy must
send `X-Forwarded-For` and `X-Forwarded-Proto`, and sign-in through a proxy that isn't listed is refused,
even on this machine. `TENSORA_INSECURE_HTTP=true` allows plain HTTP from other machines on a network you
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
go build -o bin/tensora ./cmd/tensora          # for this machine
make -C .. build-all                           # linux/amd64, linux/arm64, darwin/arm64 into bin/
docker build -t tensora-controlplane .         # Linux container image (Datacenter profile)
```

On Apple Silicon, the optional real-notebook test reuses a Jupyter environment already built by the
driver from its embedded lock. It starts a sandboxed kernel, checks IPC, and executes PyTorch MPS and
MLX Metal operations without downloading packages. From `controlplane/`:

```bash
TENSORA_TEST_NATIVE_ENV="/absolute/path/to/native/envs/jupyterlab-pytorch-mps-<hash>" \
  go test ./internal/agent/drivers/macosnative -run TestRealNotebook -count=1 -v
```
