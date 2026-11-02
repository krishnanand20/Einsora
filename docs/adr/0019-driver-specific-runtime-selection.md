# 0019. Select image runtimes by accelerator and agent driver

- **Status:** Proposed (roadmap step 7)
- **Date:** 2026-11-01
- **Deciders:** Krishnanand
- **Amends:** ADR-0013 (runtime selection), ADR-0018 (Docker image refs)

> **Amended by [ADR-0022](0022-llama-cpp-endpoints.md) (2026-11-02):** the format gate is per driver:
> GGUF runs with the drivers that serve llama.cpp (`docker`, `macos-native`), never Kubernetes. CPU
> model endpoints are no longer refused, and native ones reserve the Apple GPU as CPU notebooks do.

## Context

The same CPU notebook needs a container image on Linux and a pinned Python environment on macOS.
Selecting only by accelerator hands the native driver a container image. NVIDIA endpoints also
need a container image for Docker and a KServe runtime name for Kubernetes. Placement must know
which combination is supported before admitting it. Model validation and the trust matrix still
apply to the selected runtime.

## Decision

Add an operator-only `driver_refs` map to each catalog runtime. `ref` remains the default/Kubernetes
ref. For an agent, select the first runtime supporting both its class's accelerator and driver,
using that driver's ref. A nonempty map lists the supported agent drivers explicitly; an absent
entry cannot use the default ref. Preserve legacy catalogs without maps, including Docker's older
`container_ref`. The simulation driver continues using the default ref.

Use the same selector during placement, admission reasons, runtime specs, and agent dispatch.
Recheck the selected endpoint's model format; a different driver must not bypass the safetensors
validation gate. Catalog/API launch checks remain general until a machine and driver are known.
Driver refs stay out of public catalog responses, URLs, and workload exports.

Enable owner-only native CPU notebooks using the existing hashed Jupyter/PyTorch environment and
sandbox. A CPU class is an explicit CPU allocation, with CPU-class usage accounting. Native code
can still access Metal: macOS does not provide the isolation needed to enforce a CPU-only device
boundary here. The controller and agent therefore reserve the Apple GPU exclusively while any
native CPU notebook is active, including preparation and stopping. The admission timeline explains
this limitation. Notebook files survive stop/start and control-plane restarts. CPU model endpoints
remain blocked until GGUF validation and llama.cpp support arrive.

## Consequences

- No new dependency, migration, or public API shape. Existing Kubernetes runtime names and Docker
  catalog overrides keep working. New native CPU notebooks cannot be dispatched to a container-only
  environment, and notebooks keep their original machine and driver when restarted.
- CPU-native notebooks serialize with each other and Apple GPU workloads even when CPU and memory
  would fit more. This is conservative: the sandbox cannot revoke Metal access from owner code.
  A VM-backed CPU driver can remove that restriction later.
- Owners still choose the class explicitly; missing GPU support never changes a GPU request to CPU.
  Weak native execution remains unavailable to guests. Sharing remains planned.

## Alternatives considered

- **Select only by accelerator:** cannot distinguish native CPU from Docker CPU.
- **Keep a Docker-only override:** duplicates selection rules as other drivers arrive.
- **Advertise native CPU without reserving Metal:** permits CPU-labelled arbitrary code to overlap
  a whole-GPU allocation, breaking isolation once strong-isolation guest drivers arrive.
- **Disable Metal through environment variables:** arbitrary notebook code can ignore them.
