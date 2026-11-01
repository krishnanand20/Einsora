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
backend, which knows where each local workload listens. Each workload has its own host,
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
3. Every proxied request gets Jupyter's token added on the way in (`Authorization: token …`), and its
   `Host` and `Origin` rewritten to the loopback address Jupyter expects. Because the proxy vouches for
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
per workload (attempt, process ID and start time, port, token) in the agent's state directory, so the
driver finds its workloads again after a restart and never mistakes a reused process ID for one. The
workload can read its state file and logs but not change them, and the driver opens them without
following symlinks: it runs outside the sandbox. Health is an HTTP check on the workload's port, which
is chosen just before the server starts. A workload is its process and everything descended from it,
found by parent process ID, since Jupyter starts each kernel in a session of its own: stopping sends
SIGTERM to all of them, then SIGKILL to whatever is left, and when the server dies by itself, what it
left is stopped too. A process that leaves the tree on purpose (it starts a new session and its
parent exits before the driver looks) isn't found; macOS has no cgroups to hold it.

**`macos-native`** builds one environment per runtime (`jupyterlab-pytorch-mps`), from a lock file with
hashes embedded in the binary, with `uv` (found on `PATH` for now; downloading it is packaging, step
11). It installs only wheels (`--no-build`), checked against the lock's hashes, with an environment
of its own (not the control plane's), since the build runs outside the sandbox. It runs the workload
under `sandbox-exec` with a profile that allows writing only to the workload's data, home, and
temporary directories; denies reading or writing the Tensora state directory (other than reading the
workload's own directory and environment), even when it's inside a temporary directory, so a workload
can't change the database or the environment other workloads share; denies the control plane's
database and log file wherever they are, and common credential stores (`~/.ssh`, `~/.aws`, browser
profiles, shell history, keychain files, and others), by their paths and their symlinks' targets; and
allows signals only within the sandbox. Network access is allowed: `macos-native` is for the machine
owner's own workloads only (the trust matrix), and the sandbox protects the owner's files from
mistakes, not from an attacker.

## Consequences

- Opening a workload never puts a long-lived credential in a URL, and a notebook's pages can't act as
  the user on the dashboard, or on another workload.
- **Open risk:** a workload can listen on a loopback port, and cookies ignore ports, so if its page
  sends the browser to that port, the browser sends it the cookies of that host: the dashboard's
  session (on `127.0.0.1` or `localhost`), or another workload's access session (at its
  `w-<id>.localhost`). Any local program can do this; a `macos-native` workload is one that may run
  code its owner didn't write. Closing it means workloads can't listen on TCP (the server on a Unix
  socket, kernels over IPC). See tenancy-and-isolation.md, Open risks.
- `*.localhost` must resolve to the loopback address. Chrome, Firefox, Safari, and curl do this; a
  browser that doesn't can't open workloads until the relay (Phase 2) gives them real hostnames.
- Proxy sessions live in memory: after a restart, the user opens the workload again from the
  dashboard. A session isn't checked against the user's access again until it ends, so a user
  removed from a tenant keeps an open notebook tab for up to 12 hours, or until it stops. Tickets live
  in the store, so the API and the proxy can run in different processes.
- Endpoints (step 7b) are reached through the same proxy, with an API token instead of a cookie.
- `uv` must be installed until packaging (step 11) ships it. The first notebook downloads its
  environment (a few hundred MB, with PyTorch) once per machine.
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
