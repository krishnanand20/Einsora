# Tenancy, trust, and isolation

Einsora involves three parties. In a Solo install they're all the same person. Everywhere else they can differ.

| Party | Who | Can be hostile to |
|---|---|---|
| **Operator** | Whoever runs the control plane: you, a company's platform team, a university, a pool operator | — (holds the keys) |
| **Host** | The owner of a machine that runs workloads | Users: a host has root on their own machine and can see guest data |
| **User** | The person launching a workload (a tenant) | Hosts, the operator's infrastructure, and other users |

**The design premise:** treat every non-owner as potentially hostile, and be honest about what hosts can see.

## Trust levels

| Level | Who | Typical setting |
|---|---|---|
| **Owner** | The machine's owner, or the operator's own team (`trusted` tier in code) | Solo; a company's internal deployment |
| **Invited** | Named people, groups, or organizations | A Lab: a class, a team, friends |
| **Community** | Anyone with an account on a pool | A community pool ([ADR-0014](../adr/0014-sharing-idle-compute.md)) |

The code today has two tenant tiers: `trusted` (Owner) and `untrusted` (Invited and Community).
Per-machine trust from sharing levels is planned (Phase 4).

## Trust matrix: what may run where

| Runtime or driver | Isolation | Owner | Invited | Community |
|---|---|---|---|---|
| `kubernetes`: KubeVirt VM or Kata container | Strong | ✓ | ✓ | ✓ |
| `kubernetes`: plain runc container | Weak | ✓ | ✗ | ✗ |
| `libvirt` VM (Linux KVM) | Strong | ✓ | ✓ | ✓ if the host opts in |
| `docker` with Kata or gVisor | Strong | ✓ | ✓ | ✓ if the host opts in |
| `docker` (runc) | Weak (shared kernel) | ✓ | ✗ | ✗ |
| `macos-vz` macOS guest | Strong | ✓ | ✓ | ✓ if the host opts in (max 2 per Mac) |
| `macos-container` (a Linux container in its own VM, CPU only) | Strong | ✓ | ✓ | ✓ if the host opts in |
| `macos-native` | Weak (macOS sandbox) | ✓ | Managed inference only | Managed inference only |

**Managed inference:** the host runs a model server from the catalog, with validated weights, and the
user only sends prompts. No user code runs on the host.

**Enforced in code** by `catalog.CheckLaunch`, at create, at start, and again at admission (so a changed
catalog or tier can't be bypassed): on Apple Silicon, notebooks and endpoints run natively on macOS, so
only trusted tenants may launch them; untrusted tenants get macOS VMs there. An endpoint's model format
is checked again for the driver it's placed with (`catalog.LaunchRuntime`): GGUF only on drivers that run
llama.cpp (`docker`, `macos-native`), never on Kubernetes. Every driver validates each model with
`einsora-ml validate` in its format before a server loads it.

On agents, placement (`controller/placement.go`, [ADR-0016](../adr/0016-agent-protocol-and-machine-placement.md))
checks each machine:

- Only the tenant's own machines, until sharing lets a user choose someone else's (ADR-0014, Phase 4): a
  workload never lands where a host could see its data without its user saying so. `Machine.CanHost` also
  checks the machine is in the workload's region and shared with its tenant.
- `Machine.Permits`: a driver with weak isolation (`macos-native`, plain `docker`, `sim`) is only for the machine
  owner's own workloads. The **Owner** column above means the machine's owner here, not the trusted tier, since
  it's the owner's machine at risk.
- A driver's isolation comes from the control plane's table (`domain.LookupDriver`), never from what an agent
  reports. A host that lied about its drivers would only expose its own machine, whose guests' data it can see
  anyway.
- `macos-vz` gives a macOS guest the Mac's GPU, so a macOS VM always takes the GPU's whole unit (no CPU-only
  macOS VMs that would share it), and a Mac runs at most two (the macOS licence).
- A VM or notebook that has run keeps its driver, since its data is in that driver's storage.
- Stores re-check the same rules when the placement is recorded (`SetPlacement`).
- Agents check again: they refuse a driver they don't have, weak isolation for a workload marked as needing
  strong, and starting a workload while any other workload is still active on the GPU it needs (one being
  stopped or removed counts until it has stopped). So a bug or a race can't put two users on one GPU.
- What an agent reports must fit its machine: one Apple GPU, only on an Apple Silicon Mac; NVIDIA and AMD GPUs
  only on Linux; drivers only on their own OS.
- `docker` runs only on Linux through a local rootful Engine without user-namespace remapping
  ([ADR-0018](../adr/0018-linux-docker-driver.md)). Each owner workload has non-root execution, a
  read-only root, no capabilities or privilege escalation, CPU/memory/no-swap/PID limits, its own
  bridge network, and private data/home/run mounts. There are no published ports or host namespaces,
  devices, files, or daemon socket mounts. The access proxy connects to its Unix socket. Create
  warnings and effective settings are checked before start, including recovery. NVIDIA allocations
  reserve exact physical UUIDs; CPU containers use runc with no GPUs. Missing toolkit support never
  changes an allocation to CPU. Unknown owned containers remain visible through parking and explicit
  removal, with durable cleanup records. Outbound network access includes the LAN; this is weak,
  owner-only isolation. Disk quotas and output export are still planned. Linux/CUDA validation is pending.
- Native CPU notebooks and endpoints can still use Metal (owner code controls its device choice; a
  CPU endpoint's llama.cpp is started with `--device none`, but the sandbox can't enforce that). The
  controller and agent therefore reserve the Apple GPU exclusively while one is active, including
  preparing, stopping, or removing. This also prevents two native CPU workloads from running together.
  Their CPU class and usage accounting are unchanged; the admission timeline states the reservation
  ([ADR-0019](../adr/0019-driver-specific-runtime-selection.md)).
- llama.cpp endpoints ([ADR-0022](../adr/0022-llama-cpp-endpoints.md)) run one pinned release: on Macs,
  an archive checked by SHA-256 before it's unpacked (regular files and same-directory links only);
  in Docker, llama.cpp's own image pinned by digest. The worker validates the GGUF model, then execs
  llama-server with the validated file's absolute path, a private Unix socket, and its token in the
  environment, after removing download credentials and llama.cpp's `LLAMA_*`, `AIP_*`, and `GGML_*`
  settings. The web UI, slots, router, tools, MCP, LoRA, and host-memory prompt cache stay off, and
  layers go only on the allocated devices with memory fitting off, so a model that doesn't fit fails rather than running partly on the CPU (rule 10). The access
  proxy forwards only `/v1/models` and the two completion routes.
- `macos-native` runs each workload as its own process group under `sandbox-exec` (ADR-0017). The profile
  allows writing only to the workload's data, home, and temporary directories. It denies reading or writing
  the Einsora state directory (other than reading the workload's own directory and the shared environment),
  even inside a temporary directory, so the workload can't change its state file or logs, which the driver
  writes outside the sandbox (and opens without following symlinks). It denies the control plane's database
  and log file wherever they are, and common credential stores (`~/.ssh`, `~/.aws`, `~/.gnupg`, `~/.kube`,
  `~/.docker`, gcloud, `gh`, `~/.git-credentials`, `~/.npmrc`, `~/.pypirc`, the Hugging Face token, shell
  history, browser profiles, keychain files, and others), by path and by their symlinks' targets. It allows
  signals only within the sandbox. It listens on no TCP or UDP port, only on Unix sockets in its own run
  directory (`EINSORA_AGENT_RUN_DIR`, `~/.einsora/run` by default), and can't reach another workload's
  run directory or sockets. Both explicit binds and implicitly bound listeners are denied. Each run
  directory is reserved atomically, and saved names cannot escape that directory. The driver records
  the sandbox fingerprint: after a sandbox change, older servers and their kernels stop before a
  `sandbox-changed` failure asks the owner to restart; notebook data is kept. Stopping a workload stops every process descended from it, including
  kernels in sessions of their own. It protects the owner's files from mistakes, not from an attacker:
  network access is allowed, the keychain is still reachable through macOS's security service (which asks
  before another program reads an item), and the matrix keeps it to the owner's own workloads. A workload
  that ran before but whose data directory is gone fails instead of starting with an empty one.
- Native MLX endpoints fetch models into per-workload storage and validate them with the same
  `einsora_ml.validate` policy before loading weights. Accepted snapshots have a checksum manifest;
  reused files are revalidated. Serving re-execs offline without the download credentials and loads
  only that local model with remote code disabled. API requests cannot select another model, an
  adapter, or a file. The access proxy accepts existing API tokens only, checks their active user,
  revocation, expiry, tenant, and endpoint state on every request, and strips all browser cookies and
  the user's credential before forwarding. Bounded bodies, context/output tokens, concurrent request
  threads, and socket timeouts limit resource use; only one generation runs at a time. Preparing
  environments is serialized with a child-inherited OS lock and is canceled before the agent releases
  its machine lock on shutdown.
- An agent deletes a workload's data only when told to remove that workload. One it has but wasn't told about
  is stopped and kept, so a mistake about which machine it is can't wipe anything. A machine is found by the ID
  saved next to its database, never by its hostname.

## Threat model

| Threat | Example | Controls |
|---|---|---|
| Guest escapes to the host | A container breakout, or a macOS sandbox escape | The trust matrix: non-owners only get strong isolation. Plus PSA baseline, no privileged pods, and seccomp. |
| Guest attacks the host's network | Scanning a home LAN or the router admin page | No access to private, link-local, or LAN ranges. Community workloads have **no internet by default**. |
| Guest abuses the host's IP address | Spam, attacks, or illegal downloads from someone's home connection | No community egress by default; AUP, reports, and bans |
| Guest reads or fills the host's disk | | No host mounts; per-workload disk caps |
| Guest exhausts the host | Hogging RAM, overheating a laptop | Caps; Guaranteed QoS in a datacenter; share only when idle and on AC power; instant reclaim |
| **Host reads guest data** | An owner inspects a VM disk or process memory | Unavoidable without confidential computing. **Disclosed in the product.** Users shouldn't put secrets on community hosts. |
| Guest-to-guest access | Reading another user's data or traffic | Tenant-scoped queries (404); a namespace or VM per workload; volumes deleted with the workload; **one untrusted user per Mac GPU** (Metal has no partitioning) |
| Code execution through model files | A pickle payload, or `auto_map` custom code | `einsora-ml validate` allows only safetensors or GGUF, checks their headers, and never loads weights; never `trust_remote_code` |
| Control plane or agent compromise | Stolen controller or agent credentials | The API has no runtime access; controller RBAC is an allowlist; each agent has its own revocable mTLS certificate; datacenter admin access only over WireGuard |
| Usage-accounting tampering | Gaming quotas or fair share | Append-only events; metered states must hold capacity |
| Data leaving its region | A fallback or bug moves data abroad | Rule 9; cross-region fallback only with explicit opt-in |
| Gateway misrouting | User A's request served by user B's endpoint | The gateway routes by API key → tenant → endpoint, never by hostname alone |
| Leaks through logs | Another user's timeline or export, a credential inside an export, internals or other tenants' activity in a timeline reason, internals in an error message | Logs and exports are tenant-scoped (404), including the store queries behind them (`EventsForWorkload`). Reasons come from a fixed vocabulary, never raw runtime text. Secrets are redacted (failing closed), and each redaction is listed in the export. Model URIs with credentials are refused. Internal errors return only a request ID, and the CLI writes exports owner-only (`0600`). See [logs and transparency](logs-and-transparency.md). |
| Capacity side channel | Repeatedly queueing a workload to learn when another user is using a shared machine | Untrusted tenants see `not enough free right now`, never a free-GPU count (see Open risks) |
| Hidden decisions | A user can't tell why their work waited, moved, or failed | Rule 14: every decision is in the timeline with its reason, and the full log is exportable |

## Controls by layer

**Compute**
- Datacenter tenant pods get `automountServiceAccountToken: false`, no host namespaces, dropped capabilities, RuntimeDefault seccomp, requests equal to limits, and the Kata runtime class for untrusted tenants.
- Agents run non-owner workloads only through strong-isolation drivers, per the matrix (enforced by placement and checked again by the agent). Drivers that can cap memory, CPU, GPU memory, and disk will (`docker` enforces CPU and memory through cgroups; the VM drivers are Phase 2; disk quotas are planned). `macos-native` can't: macOS has no per-process memory or CPU limits, so a native notebook can use the whole Mac, which is one reason it's for the owner's own workloads only. A machine's owner keeps a quarter of its CPUs and memory: placement uses only what the agent reports as usable.
- No GPU time-slicing between different users, ever. On a Mac, an untrusted user gets the whole GPU or none of it.

**Network** (details in [networking.md](networking.md))
- Datacenter: default-deny NetworkPolicy per tenant, egress to the public internet only, and no Kubernetes credentials for users.
- Agents: guest workloads can't reach the LAN, private ranges, or the host. Community workloads have no internet unless the pool operator allows it.

**Identity**
- The tenant comes from a verified identity: a local account, OIDC, or an API token. There's no API parameter that names a tenant.
- **Local accounts** (built):
  - *Passwords* are hashed with argon2id, at most a few at a time, so guessing can't overload the server. Unknown emails cost the same to check as real ones, and older hashes are upgraded at sign-in, so timing doesn't reveal which accounts exist.
  - *Guessing* is limited per account and caller pair (10 failures in 15 minutes) and per caller (30, and 2 attempts at a time), counted as attempts start, so parallel guesses can't slip through (429 with `Retry-After`). A caller is an IPv4 address or an IPv6 /64; an IPv6 /48, which one person can easily hold whole, gets a looser limit of its own (100, and 4 at a time). Where an ISP gives each home a /56 or /64 out of a shared /48, a neighbour can still use up that /48's limit and block sign-in from it for 15 minutes; the per-pair and per-caller limits are the ones that hold for everyone. Knowing the owner's email isn't enough to lock them out, and password checks for a signed-in user (making a token, changing the password) are limited per caller too, so someone holding a stolen session can't use up the owner's. Only attempts whose password was checked count, so the limiter's memory is bounded by how fast passwords can be checked, and it's capped too. New passwords must not be common or mostly one character. Someone with many addresses can still guess slowly (see Open risks).
  - *Proxies:* behind a reverse proxy, the caller's address comes from `X-Forwarded-For` (every header line, nearest entry first) only if the proxy is listed in `EINSORA_TRUSTED_PROXIES`. List only the proxies' own addresses: every listed address can claim HTTPS and any client address, and the server warns about ranges. A proxied request without `X-Forwarded-For` is refused, since everyone behind the proxy would share one limit. A request with proxy headers from a peer that isn't listed is refused for sign-in and sessions, even from this machine: everyone behind that proxy would share one limit, and what it says about HTTPS can't be believed. A proxy must send `X-Forwarded-For` and `X-Forwarded-Proto`; one that sends neither can't be told apart from a browser on the same machine.
  - *Passwords and session cookies cross only private connections*: HTTPS (directly, or from a listed proxy that says so), or a browser on this machine. Plain HTTP from another machine is refused unless the operator sets `EINSORA_INSECURE_HTTP=true` for a network they trust.
  - *Session and API tokens* are 256 random bits, stored only as SHA-256 hashes; a new API token is shown once. The session cookie is HttpOnly and SameSite=Strict, and over HTTPS it's `Secure` with the `__Host-` name, so no other host can set it; over HTTPS a cookie with the plain name is ignored, and a request carrying the session cookie twice is refused. Every cookie request that changes something needs the `X-Einsora-CSRF` header, which other sites can't add.
  - *An API token can't make tokens or change the password*, and a browser session needs the password again to make a token: a token outlives the session that makes it. Changing the password signs out every other session and reports how many API tokens still work. A sign-in that checked the old password can't finish after a password change: both lock the user's row.
  - A user lists and revokes only their own tokens. Disabling an account ends its sessions and tokens. Timelines name the credential that acted (`browser session`, or the token's name and prefix).
  - The first account comes from `einsora setup` on the machine itself, never from a first-come web form, and `einsora setup -reset-password` is the way back in. Both need access to the database. A reset also signs out every session, which is how to end one that was stolen.
- **Workload web UIs** (Jupyter, endpoint UIs) must be served from a different site than the dashboard and API, such as their own hostname. Code running in a workload controls what its UI serves, and on the dashboard's own origin it could send requests with the user's session cookie and the CSRF header. Another port on the same host isn't a different site for cookies (a notebook on `localhost:8888` can set cookies for the dashboard on `localhost:8080`), and neither is a subdomain of the dashboard's domain.
  - *Built, on the control plane's machine* ([ADR-0017](../adr/0017-local-workload-access-and-native-drivers.md)): the access proxy serves each workload at its own site, `w-<id>.localhost`, and answers no other host name. It opens a workload only with a one-time ticket from the API, which checks the caller's tenant (404 otherwise) and stores only the ticket's hash, for 60 seconds. The ticket is good at its own workload's host only, and the cookie it becomes (HttpOnly, SameSite=Strict) is for that host only and never reaches the workload. A session ends after 12 hours, or when the workload stops. The proxy refuses requests a browser says came from another origin (`Origin`, or Fetch Metadata for anything but a navigation), since a page on another port of the same host is same-site; it keeps the workload's cookies host-only, and the workload can't replace the proxy's. The workload's own token is added by the proxy, and is never in a URL, the database, a log, or an export (JupyterLab's page includes it, for its own scripts). The agent backend takes it only from the agent in its own process. The proxy listens on a loopback address only. The relay for other machines is Phase 2.
  - Native workloads listen on no TCP or UDP port (the sandbox denies it; JupyterLab is on a Unix socket, its kernels on IPC), because a program listening on a loopback port receives the cookies the browser sends to that host, whatever the port. Other programs on the machine still can (see Open risks).
- Every store query is scoped by tenant. Dev header auth is refused outside `EINSORA_ENV=dev`.

**Logs** (details in [logs-and-transparency.md](logs-and-transparency.md))

| Who | Sees |
|---|---|
| A user | Their own workloads' timeline, usage, and export (output logs planned). Never another tenant's. |
| A tenant admin | The tenant's activity log (planned, Phase 2) |
| A host | Their machine's status and which workloads ran on it. Never a guest's output through the product, though as root a host could technically read guest data (disclosed). |
| The operator | System logs. In a datacenter, Loki access is limited to on-call staff, and audited. |

**Supply chain**
- Notebooks run catalog images or environments. Endpoints use pinned runtimes: vLLM by version, MLX and
  the workers' Python packages by hash, llama.cpp by SHA-256 (Macs) and image digest (Docker).
- Releases ([ADR-0023](../adr/0023-releases-and-packaging.md)) are built only by the release workflow, from a
  tag on `main`, with its tools pinned by commit. Their checksums are signed with Sigstore, keyless, by
  that workflow's identity, and `install.sh` refuses an archive whose SHA-256 doesn't match. macOS builds
  aren't notarized (that needs a paid Apple membership): they're signed ad hoc, install with the script
  or with the Homebrew formula, which builds from the signed source archive, and the quarantine flag is
  never removed to get past Gatekeeper. The tap's token is used only by the job that publishes the formula.
- `einsora service install` runs `einsora up` as the user, never as root: a launch agent or a systemd user
  service, whose definition, which can hold variables the user named with `-env`, only they can read.

## Open risks

| Risk | Status | Plan |
|---|---|---|
| **GPU memory remanence** between users: VFIO passthrough, and Metal on shared Macs | Open | Test both (Phase 3 for passthrough; Phase 4 before Macs serve untrusted users). Add a scrub step if data survives. |
| Strength of the macOS sandbox for native workloads | Accepted for owners only | The trust matrix limits non-owners to managed inference or VMs |
| Kata + GPU passthrough not yet validated | Open | Phase 3 gate |
| Block storage isn't zeroed on reuse (datacenter) | Open | Encrypted volumes with per-volume keys |
| Datacenter admin network not firewalled | Open | Phase 3 |
| The controller's ServiceAccount can create pods in any namespace | Accepted | Kind allowlist, RBAC limited to listed resources, audit logging |
| Hosts can see guest data | **Accepted and disclosed** | Product warnings; confidential computing out of scope |
| Before machines are shared (Phase 4) | Required safeguards implemented for current drivers | Native CPU workloads count as holding Metal access in controller placement and agent checks (ADR-0019). Both `macos-native` and `docker` refuse to replace missing saved notebook data. Future drivers need the same safeguards; placement still uses only the tenant's own machines, and sharing remains planned. |
| A program listening on a loopback port gets the browser's cookies for that host: cookies ignore ports | Mitigated for workloads | A page sent to `127.0.0.1:7777`, then on to another path there, carries the dashboard's session cookie to whatever listens there (or, at `w-<id>.localhost:7777`, another workload's access session). Native workloads can't listen on any TCP or UDP port: the sandbox denies it, JupyterLab is on a Unix socket in the workload's run directory, and kernels talk over IPC. Other programs on the machine, and other OS users, still can; Solo assumes a machine whose other users are trusted. The cost: native notebooks can't serve their own web apps on a port (TensorBoard, Gradio) or use JupyterLab's debugger, until the access proxy serves them on Unix sockets. |
| Access-proxy sessions aren't checked against the user's access again | Accepted | A session lasts 12 hours or until its workload stops, so a user whose account is disabled keeps a notebook tab they already opened until then. Stop the workload to end every session for it. Phase 2 (users and teams) ends sessions with the account. |
| Slow guessing from many addresses, and making sign-in busy | Accepted (mitigated) | Per-caller limits, a cap on concurrent password checks, and refusing common passwords keep guessing slow, but someone with many IPv4 addresses or IPv6 /48s can still guess slowly, or keep the password checks busy so sign-in answers "busy" for a while. Use a long, uncommon password. Before exposing sign-in to the internet, put a rate-limiting proxy in front; Phase 2 adds per-account alerts. |
| Admission outcomes reveal whether a GPU class is full | Accepted (mitigated) | Untrusted tenants get no free-GPU counts, but queued versus admitted still shows whether a class has room. When sharing lands (Phase 4), compute capacity only over machines that user may use, so the signal covers only what they could run on anyway. |

## Compliance notes

Einsora is software. **Whoever operates an instance is responsible** for the law where they operate.

- **Solo** keeps everything on the user's own machine.
- **Labs and pools** hold account data, so their operator needs a privacy notice (GDPR, DPDP, CCPA and others apply to operators), an acceptable use policy, and a way to handle abuse reports and lawful requests.
- **Models** come with their own licences, and users must follow them.
