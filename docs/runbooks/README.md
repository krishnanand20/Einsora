# Runbooks

Step-by-step procedures for installing and operating Einsora. These are for **humans**: agents may help
draft and review them, but they never execute steps that change hosts, clusters, or a developer's machine.

| Runbook | Profile | When | Status |
|---|---|---|---|
| [00-quickstart.md](00-quickstart.md) | Solo | Install on one Mac or Linux machine | Works from source; the one-line install from the first release |
| [mac-host.md](mac-host.md) | Solo, Lab, sharing | Run an Apple Silicon Mac as a host | Planned (Phases 1–4) |
| [01-node-bootstrap.md](01-node-bootstrap.md) | Datacenter | A new server, from bare metal to a Ready k3s node | Written; not yet run on hardware |
| [02-gpu-passthrough.md](02-gpu-passthrough.md) | Datacenter | GPU smoke test, burn-in, then the first passthrough VM | Written; not yet run |
| [03-first-model-endpoint.md](03-first-model-endpoint.md) | Datacenter | The first vLLM endpoint through KServe | Written; not yet run |
| [add-node.md](add-node.md) | Datacenter, Lab | Adding a server or an agent machine | Written |
| [incident-response.md](incident-response.md) | All | Something is broken | Written |
| [release.md](release.md) | Maintainers | Publish a version: archives, packages, signatures, the Homebrew formula | Written; no release cut yet |

Every runbook lists the expected output of each step. If reality differs, stop and fix the
runbook before continuing.
