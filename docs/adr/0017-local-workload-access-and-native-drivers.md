# 0017. Open workloads through an access proxy with one-time tickets, and run native drivers as supervised processes

- **Status:** Proposed (roadmap step 7a: the process supervisor, the `macos-native` notebook driver, and the access proxy)
- **Date:** 2026-11-01
- **Deciders:** Krishnanand
- **Amends:** ADR-0013 (drivers), ADR-0016 (the agent)

## Context

Roadmap step 7 makes workloads run for real: on a Mac, JupyterLab with PyTorch on Metal (`macos-native`).
Two things have to be decided.

**How a user opens a workload.** A notebook is a web server, and the code in it controls what it
serves. If it shared an origin, or a site, with the dashboard, it could send requests with the user's
session cookie and the CSRF header (tenancy-and-isolation.md, "Workload web UIs"). A different port on
the same host isn't enough: cookies aren't separated by port. Jupyter's own token protects it from
other local users and from web pages that guess its port, but the token must not end up in the
browser's address bar, history, or the store. The API never talks to the runtime (rule 1).

**How native workloads run.** `macos-native` runs processes on the host, not containers. They need a
Python environment with pinned packages (uv), a sandbox, a data directory that survives stops and
control-plane restarts, and supervision: starting, health, stopping, and finding them again after the
`tensora` process restarts. The driver contract (ADR-0016) says calls return promptly, statuses carry
the attempt across restarts, and `Stop` leaves a workload stopped.

## Decision

**An access proxy, with one-time tickets.** The control plane runs a small reverse proxy
(`TENSORA_ACCESS_ADDR`, `127.0.0.1:8090` by default) in the controller's process, beside the agent
backend, which knows each local workload's Unix socket. Each workload has its own host,
`w-<workload ID>.localhost:<port>`: a different site from the dashboard (`127.0.0.1`) and from every
other workload, so none can read or set another's cookies.

1. The dashboard (or `POST /v1/workloads/{id}/access`) asks the API to open a running workload. The API
   checks the caller's tenant owns it, stores a one-time ticket (only its SHA-256, the workload, the
   tenant, and the user; it expires in 60 seconds), notes in the workload's timeline who asked to open
   it, and returns the URL with the ticket.
2. The proxy takes the ticket once, and sets a cookie for that workload's host only (HttpOnly,
   SameSite=Strict), backed by a session in memory that lasts 12 hours or until the workload stops:
   the session is bound to the server it opened, and the next run has a new one. The proxy answers
   with a page that moves on to the workload, not a redirect, because a Strict cookie set during a
   navigation from the dashboard isn't sent until a navigation from the workload's own host.
3. Every proxied request goes to the workload's Unix socket, with Jupyter's token added on the way in
   (`Authorization: token …`), and its `Host` and `Origin` rewritten to `localhost`, as Jupyter
   expects. Connections to workloads aren't reused, so one workload's can never carry another's requests. Because the proxy vouches for
   the origin, it first refuses any request a browser says came from another origin (`Origin`, or
   Fetch Metadata for a request that isn't a navigation): cookies ignore ports, so a page on another
   port of the workload's host is same-site, and its requests carry the session. The workload's own
   cookies are parsed as a browser would and kept host-only, and it can't replace the proxy's. The
   token never leaves the machine: the driver makes it, the agent reports it with the workload's status
   (the agent backend keeps it only from the agent in its own process), and the agent backend keeps it
   in memory only. It's never stored in the database, logged, exported, or in a URL. JupyterLab does put
   it in its own page, for its scripts, which run as the workload anyway.

**Native drivers are supervised processes.** A shared supervisor (`agent/drivers/process`, Unix only)
starts each workload in its own process group, with output to log files, and keeps a small state file
per workload (attempt, process ID and start time, run directory, sandbox fingerprint, token) in the agent's state directory, so the
driver finds its workloads again after a restart and never mistakes a reused process ID for one. The
workload can read its state file and logs but not change them, and the driver opens them without
following symlinks: it runs outside the sandbox. Health is an HTTP check on the workload's socket. A workload is its process and everything descended from it,
found by parent process ID, since Jupyter starts each kernel in a session of its own: stopping sends
SIGTERM to all of them, then SIGKILL to whatever is left, and when the server dies by itself, what it
left is stopped too. A process that leaves the tree on purpose (it starts a new session and its
parent exits before the driver looks) isn't found; macOS has no cgroups to hold it.

**`macos-native`** builds one environment per runtime (`jupyterlab-pytorch-mps`, `mlx-lm`), from a lock file with
hashes embedded in the binary, with `uv` (found on `PATH` for now; downloading it is packaging, step
11). It installs only wheels (`--no-build`), checked against the lock's hashes, with an environment
of its own (not the control plane's), since the build runs outside the sandbox. It runs the workload
under `sandbox-exec` with a profile that allows writing only to the workload's data, home, and
temporary directories; denies reading or writing the Tensora state directory (other than reading the
workload's own directory and environment), even when it's inside a temporary directory, so a workload
can't change the database or the environment other workloads share; denies the control plane's
database and log file wherever they are, and common credential stores (`~/.ssh`, `~/.aws`, browser
profiles, shell history, keychain files, and others), by their paths and their symlinks' targets; and
allows signals only within the sandbox. Connecting out is allowed: `macos-native` is for the machine
owner's own workloads only (the trust matrix), and the sandbox protects the owner's files from
mistakes, not from an attacker.

**Native workloads listen on no TCP or UDP port.** Cookies ignore ports: if a workload could listen on
a loopback port, its page could send the browser there, and the browser would hand it the cookies of
that host, the dashboard's session (on `127.0.0.1` or `localhost`) or another workload's access
session (at its `w-<id>.localhost`). So the sandbox denies both binding an IP address and inbound IP
connections: `listen()` can bind a TCP port implicitly without calling `bind()`. JupyterLab listens
on a Unix socket (`--ServerApp.sock`, owner-only) and its kernels talk to it over IPC
(`--KernelManager.transport=ipc`), both in the workload's run directory. Socket paths are limited to
104 bytes on macOS, too short for the state directory, so run directories are in
`TENSORA_AGENT_RUN_DIR` (`~/.tensora/run` by default, owner-only, checked to be a real directory of
this user's; a short folder in the home directory, as lima and colima keep theirs, not `/tmp`, which
macOS cleans). Each workload's run directory is reserved atomically before its environment is built.
It also holds the workload's temporary files, and its contents are emptied at each start without
replacing that reservation. A workload can't reach another workload's run directory or connect to
its sockets. Saved run names are validated before the driver uses them as paths.

The driver records a fingerprint of the sandbox profile and its directory mappings in each attempt.
After a sandbox change, it stops older servers and their kernels before reporting a failure with the
fixed `sandbox-changed` reason asking the owner to restart. This includes servers from the older TCP
implementation, which have no fingerprint. Notebook data is kept, and a new attempt uses the updated
sandbox. An ordinary control-plane restart with the same sandbox adopts the existing processes.

**Native model endpoints (step 7b).** The MLX runtime is pinned separately from notebooks and needs
macOS 14 or later. The driver embeds a deterministic bundle of `ml/src/tensora_ml`, including the
same `validate_model_dir` enforcement point used by `tensora-ml validate`; a test refuses a stale
bundle. Changing the worker changes the endpoint's runtime and sandbox fingerprint.

The worker downloads only allowed files from the workload's `hf://` or `s3://` source into that
workload's private directory. Hugging Face downloads resolve a commit before fetching files. File
names are checked before joining paths, advertised sizes are bounded by the allocation's memory,
and the validator checks the complete downloaded model before any weights are loaded. Only accepted
files are published, with a SHA-256 manifest. A reused snapshot must match its source, policy, size,
and every checksum. Models are never shared between workloads or regions.

Download credentials (`HF_TOKEN` or explicit AWS environment credentials) are used only for fetching.
Before loading, the worker re-execs with those credentials removed and Hub/Transformers offline from
process startup. MLX loads only the validated local directory with remote code disabled and Metal
required. The HTTP API exposes only model listing, text completions, and text chat completions,
including SSE and token usage. It serves exactly one model; requests cannot select other models,
adapters, files, or custom code. Request bodies, output tokens, context length, concurrent requests,
and stalled socket reads are bounded; generation is serialized while health checks remain available.

`POST /v1/workloads/{id}/access` returns an endpoint's `/v1` base URL without a ticket or expiry. Each
inference request must use an existing local API token. The proxy rechecks its user, expiry,
revocation, tenant, and workload state on every request (404 across tenants), strips all browser
cookies, and replaces the user's token with the worker's private token. Responses identify their
region, actual compute class, and fallback status; errors carry a server-generated request ID.
Endpoint-scoped keys and request failover remain Phase 2.

Environment preparation resumes from saved intent after a control-plane restart. Builds take a
per-environment OS lock inherited by the `uv` child, so a child surviving a crash cannot race a
recovered build. Graceful shutdown cancels and joins builds before giving up the machine lock;
already running workload processes stay alive.

## Consequences

- Opening a workload never puts a long-lived credential in a URL, and a workload can't act as the user
  on the dashboard, or on another workload: its pages are another site, and it can't listen on a port
  the browser would send cookies to.
- Native notebooks can't serve their own web apps on a port (TensorBoard, Gradio, Streamlit), and
  JupyterLab's debugger, which listens on a port, doesn't work. Serving them through the access proxy,
  on Unix sockets, is a later change. Any other program on the machine can still listen on loopback
  ports; Solo assumes a machine whose other users are trusted.
- `*.localhost` must resolve to the loopback address. Chrome, Firefox, Safari, and curl do this; a
  browser that doesn't can't open workloads until the relay (Phase 2) gives them real hostnames.
- Proxy sessions live in memory: after a restart, the user opens the workload again from the
  dashboard. A session isn't checked against the user's access again until it ends, so a user
  removed from a tenant keeps an open notebook tab for up to 12 hours, or until it stops. Tickets live
  in the store, so the API and the proxy can run in different processes.
- Endpoints are reached through the same proxy, with an API token instead of a cookie. They support
  text chat and text completions with `max_tokens`, `temperature`, `top_p`, and streaming; unsupported
  options (tools, images, adapters, alternate models) are refused explicitly.
- `uv` must be installed until packaging (step 11) ships it. The first notebook downloads its
  environment (a download of a few hundred MB, about 750 MB on disk, with PyTorch and MLX) once per machine.
- Processes outlive a `tensora` restart, and the driver adopts them again. If the state directory is
  lost, its workloads' data is too; the driver can't tell a lost disk from a new one then.

## Alternatives considered

- **Open Jupyter's port directly with its token in the URL:** the token lands in history and logs, and
  the notebook shares a site with the dashboard (`localhost` vs `127.0.0.1` aside, ports don't separate
  cookies).
- **Serve workloads under a path on the dashboard's origin (`/w/<id>/`):** the notebook's code would run
  on the dashboard's origin, with the user's session.
- **Store proxy sessions in the database:** not needed while the proxy runs in one process; tickets are
  stored so the API can issue them.
- **Containers on macOS for notebooks:** no Metal; that's `macos-container`, for strong isolation
  without the GPU.
- **Let workloads listen on loopback TCP ports, and rely on Jupyter's token and the proxy's Origin
  checks:** a workload's own listener receives the browser's cookies for its host whatever the
  port, so no check in the proxy or the API can stop it.
- **Block workloads from connecting to the API and the proxy:** they need credentials there that they
  can no longer get, and a notebook calling the API with a token is a reasonable use.
