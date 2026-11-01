# 0018. Run owner workloads through a local Linux Docker Engine

- **Status:** Proposed (roadmap step 7c)
- **Date:** 2026-11-01
- **Deciders:** Krishnanand
- **Amends:** ADR-0013 (drivers), ADR-0016 (agent), ADR-0017 (local access)

> **Amended by [ADR-0019](0019-driver-specific-runtime-selection.md):** `driver_refs` generalizes
> the Docker image override for per-driver selection. Legacy `container_ref` catalogs remain supported.

## Context

Solo needs real Linux notebooks and NVIDIA endpoints. The store and controller retain intent and
placement. Plain containers share the host kernel, so the [trust matrix](../architecture/tenancy-and-isolation.md)
permits them only for the machine owner's workloads. GPU fallback remains opt-in (ADR-0010).

## Decision

Implement `agent.Driver` with a narrow stdlib HTTP client to the local Docker Engine Unix socket.
Only Linux, rootful Docker without user-namespace remapping, and working CPU, memory, swap, and PID
controllers are supported initially. Negotiate Engine API 1.41–1.47. Use version- or digest-pinned
catalog images, resolve their immutable image IDs, and reject create warnings or effective settings
that differ from the requested isolation and limits. Docker on a Mac and remote daemons are excluded:
GPU inventory, bind mounts, and access sockets must all belong to the agent's machine.

Each workload gets a non-root container, read-only root, dropped capabilities, no-new-privileges,
CPU/memory/PID limits, private data/home/run mounts, and its own bridge network. There are no
published ports, host namespaces, host devices, or daemon socket mounts. The existing access proxy
uses a private Unix socket for JupyterLab or vLLM. Docker's NVIDIA runtime and `nvidia-smi` physical
UUID inventory are both required to advertise NVIDIA. Persist exact matching GPU UUID reservations
before asynchronous preparation; CPU containers use runc without GPU access. Missing NVIDIA support
leaves only explicitly requested CPU workloads available; it never changes a GPU allocation.

Persist attempt, requested spec hash, protection verification, runtime policy, and lifecycle locally.
Image pulls are cancelable. Adopt matching running containers after restart; never restart a failed
attempt. Keep notebook data on stop and refuse to replace a missing saved disk. Discover unknown
owned containers by instance/workload labels, park them while retaining data, and remove them only
on explicit desired removal. Record a removal tombstone before deleting their last container so
failed network or filesystem cleanup remains visible and retryable. Hold capacity while uncertain.

Docker vLLM endpoints use the same embedded Python validation and checksum policy as MLX, mounted
read-only. Fetch into private workload storage, validate safetensors, then exec vLLM with local model
and tokenizer paths, offline mode, no remote code or dynamic adapters, and download credentials
removed. The proxy replaces the caller's API token with the worker's private Bearer token.
The catalog's optional operator-only `container_ref` distinguishes a Docker image from a KServe
runtime name; choosing runtimes by both driver and accelerator is the next roadmap step.

## Consequences

- No new Go dependencies or store/API schema changes. The controller remains the only placement
  authority, and reasons use the existing fixed protocol vocabulary.
- Operators configure Docker and, optionally, NVIDIA's toolkit themselves. The agent needs local
  daemon access, which grants substantial host authority; containers never receive that access.
- These containers have outbound network access, including the LAN. Bridge networks and runc are
  insufficient for guests; strong isolation and network policy remain prerequisites for sharing.
- Memory, CPU, and PID limits are enforced; disk quotas and workload output export remain planned.
  Only curated pinned images should be used. GPU memory is bounded by allocating whole devices.
- CPU notebooks work without NVIDIA. CPU endpoints await GGUF validation and llama.cpp (step 10).
  Rootless/userns support requires a design for remapped bind-mount ownership and is deferred.
- Engine conformance and recovery tests run without Docker. Live Linux notebook and CUDA endpoint
  validation still require Linux hardware; cross-compilation alone does not establish CUDA support.

## Alternatives considered

- **Docker CLI subprocesses:** harder to cancel and parse without losing ownership/recovery details.
- **Docker SDK:** unnecessary dependency surface for this small, bounded API subset.
- **TCP ports for access:** avoid exposing browser cookies to arbitrary workload listeners; keep the
  existing workload host and Unix socket proxy.
- **Silent CPU fallback:** contradicts ADR-0010; placement must record an opted-in fallback first.
