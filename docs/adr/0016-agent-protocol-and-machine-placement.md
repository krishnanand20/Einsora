# 0016. Sync agents by long-polled desired state over HTTPS, and place on machines in the controller

- **Status:** Proposed (in code: the runtime-neutral backend, machine placement, the agent backend, the protocol types, the in-process agent, inventory, and a `sim` driver for development. The network transport and enrolment are Phase 2)
- **Date:** 2026-10-25
- **Deciders:** Krishnanand
- **Amends:** ADR-0013 (the agent), and AGENTS.md rule 2

## Context

ADR-0013 decided that a node agent runs workloads on Macs and Linux machines, connecting outbound with
mTLS, and that an `agent` implementation of `cluster.Backend` dispatches to agents. It left the protocol
open (roadmap: "gRPC streams, or HTTPS with WebSocket").

Constraints:

- **Rule 1:** the store owns intent; the runtime owns runtime state. A restart of the control plane must
  never make agents stop or delete workloads.
- **Rule 2:** all runtime access goes through `cluster.Backend`. Today that interface takes Kubernetes
  manifests and namespaces, which an agent can't use.
- **ADR-0014 and the trust matrix:** which machine and which driver a workload may use depends on who owns
  the machine, the tenant, and how strongly the driver isolates. The roadmap asks for that decision to be
  made in one place, before anything is applied.
- **Hosts may be hostile** (Phase 2 onward): nothing an agent reports may decide its region, its
  isolation level, or what text reaches a user's timeline.
- **Solo** runs the agent in the same process; **Lab** agents sit behind home routers and NAT.
- The Go standard library has HTTPS and mTLS; gRPC would be the largest dependency in the module.

## Decision

**One call, `Sync`.** The agent sends what it has (the status of every workload, from a fixed vocabulary
of codes, and its inventory when it changes) and the generation of desired state it last applied. The
control plane records it and answers with the machine's complete desired state as soon as it changes, or
after a timeout (a long poll). Over the network (Phase 2) it's `POST /agent/v1/sync` over HTTPS with mutual
TLS, outbound from the agent, so it works behind NAT. In Solo, the same call is a function call to the
in-process agent.

**Desired state is computed from the store** on every `Sync`: each workload placed on the machine and
its state (provisioning or running: run; pending, queued, stopping, stopped, or failed: parked, keeping
its disk, except endpoints, which are removed; deleting: removed). The backend's `Run`, `Park`, and
`Delete` only wake the machine's long poll. Nothing about desired state lives only in memory, so a
restart changes nothing. An agent deletes a workload's data only when it's listed for removal; one it has
but isn't told about is stopped and kept, because a list can leave a workload out only by mistake (a
machine taken for another, a replaced database), and deleting data is never the safe guess. A machine is
known by the ID saved next to its database, never by its hostname.

**`cluster.Backend` becomes runtime-neutral:** `Capacity`, `Run(spec)`, `Park(spec)`, `Observe(ref)`,
and `Delete(ref)`, where a spec names the workload, its placed class, image, runtime, machine, and
driver. The Kubernetes backend builds manifests from a spec with the existing pure builders.

**The controller places workloads** in its admit step. A backend reports either cluster-wide units per
class (Kubernetes, which schedules nodes itself) or the machines connected now, with their schedulable
CPUs, memory, and devices per class. On machines, the controller chooses the machine and the driver:

- the machine must be the tenant's own until sharing lets a user choose someone else's (ADR-0014, Phase 4),
  and in the workload's region (`Machine.CanHost`);
- the driver must run the workload's kind on the class's accelerator, and weak isolation (`macos-native`,
  plain `docker`) is allowed only for the machine owner's own workloads (`Machine.Permits`). A driver's
  isolation comes from a table in the control plane, never from the agent;
- the class's devices, CPUs, and memory must be free on that machine, and the driver's per-machine limit
  must hold (two macOS VMs per Mac);
- a VM or notebook that has run before goes back to the machine holding its disk, with the driver holding
  its data.

Placement records the class, machine, and driver in the same transaction as the move to provisioning, and
the timeline says all three. The controller then calls `Run`; a workload that's provisioning but absent
from the runtime is run again on the next pass.

**Agents check too:** an agent refuses a workload whose driver it doesn't have, that needs stronger
isolation than the driver gives, or that it's asked to move to another driver while it holds data; and it
doesn't start one while any other workload is still active on the GPU it needs.

**Offline machines:** a machine that hasn't synced for 45 seconds isn't offered for placement, and its
workloads' status is unknown, which decides nothing. After 10 minutes, its running workloads fail and its
stopping ones are stopped, so nothing stays accounted or holds quota for a machine that's gone. Right
after a restart the backend says it's settling until its machines reconnect, and nothing is admitted.

**What agents report is never trusted for policy:** a machine's owner and region come from its
registration (in Phase 2, its enrolment token); its inventory must fit the machine (one Apple GPU, only on
an Apple Silicon Mac; drivers only on their own OS); reported text is bounded and never shown; status codes
map to fixed reasons in the control plane, and unknown codes read "its machine reported a problem".

## Consequences

- Restarting the control plane is safe, and placement rules live in one function with unit tests.
- One controller reconciles a region at a time, through a lease in the store (`controller:<region>`); another
  stands by. Each hub sees only the agents that sync with it, so until agents join over the network the
  agent backend runs with SQLite (one host).
- Agents apply desired state beside their sync loop, and every driver call has a time limit, so a long
  driver call never makes a busy machine look offline; anything worth reporting cuts a waiting sync
  short, so the long poll never delays news; drivers return promptly and report long work as
  progressing. A report says which desired state it was applied from (`Applied`): a workload's status
  from before the desired state that last changed its mode is read as still in progress, so an old
  failure never answers a restart. Each admission is a new attempt, which an agent starts at most once;
  a status answers only the attempt and mode the store expects, once the agent has applied them.
- One controller leads a region: it renews its lease in the background and steps down if it can't, and
  the in-process agent runs only in the leader, behind an OS lock that allows one agent per machine.
- Every runtime action is now "place, then run". When a runtime is down, workloads hold their capacity
  while provisioning, and the provisioning timeout fails them, instead of waiting in pending indefinitely.
- The Kubernetes backend takes the manifest options (storage class, images namespace, sandbox runtime
  class) that the controller used to hold.
- Each `Sync` reads the machine's workloads from the store: fine for the hundreds of machines a Lab or a
  fleet region has; a much larger fleet would cache it.
- The long poll adds up to its timeout of latency when a wake-up is missed; wake-ups are sent after
  every placement, stop, and delete.
- Phase 2 must add: the HTTPS transport, a certificate authority on the control plane, enrolment tokens
  from the dashboard (which fix the machine's owner and region), per-machine certificates and revocation,
  and machine health on the dashboard.
- Revisit if agents need to stream large data (output logs, roadmap step 14; [ADR-0024](0024-workload-output.md) added a separate `Output` call for them) through the same channel:
  that may justify a separate upload endpoint, not a new protocol.

## Alternatives considered

- **gRPC streams:** a big dependency, HTTP/2 everywhere, and harder to debug with curl; the long poll gives
  the same latency for our message rates.
- **HTTPS with WebSocket:** needs a WebSocket library and connection-state handling, for little gain over
  a long poll.
- **Desired state pushed and held in memory:** a control-plane restart would hand agents an empty list.
- **Keep the Kubernetes-shaped backend and have agents read manifests:** ties agents to Kubernetes types
  they can't run.
- **Let the agent choose the driver:** the agent can't be trusted with the trust matrix, and a choice made
  in two places drifts.
