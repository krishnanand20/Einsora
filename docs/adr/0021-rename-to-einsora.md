# 0021. Rename the product to Einsora

- **Status:** Accepted
- **Date:** 2026-11-01
- **Deciders:** Krishnanand
- **Supersedes:** the name in ADR-0020 (TensoraFi). ADR-0020's MIT licence still applies.

## Context

ADR-0020 named the product TensoraFi. Clearing the name turned up a US trademark, TENSORA, registered
in August 2025 by Tensora, Inc., one of its registrations in Class 42 (software and cloud
services). TensoraFi contains the whole mark, so a trademark office or the owner could object. Its
"Fi" ending also reads as finance or crypto, next to an existing crypto token called Tensora (TORA).
Nothing is released, so changing the name now costs one change.

## Decision

**The product is Einsora**, after `einsum`, the standard way to combine tensors in NumPy and PyTorch:
related to the old name's idea without sounding like it. On 2026-11-01, `einsora.dev`, `einsora.com`,
the GitHub name, and the PyPI and npm package names were all unregistered, and a web search found
nothing called Einsora.

Every identifier changed in one change:

| What | TensoraFi (ADR-0020) | Einsora |
|---|---|---|
| Binary and CLI | `tensorafi` | `einsora` |
| Go module | `github.com/krishnanand20/tensorafi` | `github.com/krishnanand20/einsora` |
| Environment variables | `TENSORAFI_*` | `EINSORA_*` |
| HTTP headers | `X-TensoraFi-*` | `X-Einsora-*` |
| Kubernetes labels and annotations | `tensorafi.dev/*` | `einsora.dev/*` |
| Cookies | `tensorafi_session`, `tensorafi_access` | `einsora_session`, `einsora_access` |
| Python package and CLI | `tensorafi-ml`, `tensorafi_ml` | `einsora-ml`, `einsora_ml` |
| Helm chart, namespaces, Docker networks, container paths | `tensorafi…` | `einsora…` |
| Export format | `tensorafi.workload-export/v1` | `einsora.workload-export/v1` |
| Native workloads' run directories | `~/.tensorafi/run` | `~/.einsora/run` |
| Development database (compose project, user, and database) | `tensorafi` | `einsora` |

As in ADR-0020, the Postgres function `tensora_forbid_mutation` keeps its name (an applied migration
is never edited), and ADRs keep the names of their time: ADRs 0001 to 0019 say Tensora, and ADR-0020
says TensoraFi.

## Consequences

- Nothing is released, so there are no compatibility shims: an existing `.env` needs `TENSORAFI_`
  (or `TENSORA_`) renamed to `EINSORA_`; browser sessions end; native workloads started before the
  rename have their sockets in the old run directory, so stop them first; and the development
  database is a new compose project (`einsora`), with a new volume.
- The log pipeline's tenant label is fixed: ADR-0020's rename left the Alloy rule reading
  `__meta_kubernetes_pod_label_tensorafi_io_tenant`, a name derived from the old `.io` label, so a
  Datacenter install would have shipped workload logs without their tenant. It reads
  `…_einsora_dev_tenant` now.
- The repository is `github.com/krishnanand20/Einsora`. GitHub matches names case-insensitively and
  redirects old URLs, so the lowercase module path resolves to it.
- **Not yet cleared:** a web search isn't a trademark search. "Einsora" ends in a sound like "Sora",
  OpenAI's video model, though it's pronounced differently (*EYN-sor-ah*) and doesn't read as
  "Ein + Sora". A trademark attorney should check it in the main markets (classes 9 and 42) before the
  first release, and `einsora.dev` and `einsora.com` should be registered.

## Alternatives considered

- **Keep TensoraFi:** contains a registered mark in the same class, and reads as crypto.
- **Rhizogrid** (all names free, `.com` included): a good name, but unrelated to the old one.
- **Eigora** and **Lattora** (from *eigen* and *lattice*): free, but their `.com` domains are taken.
- **Names that sound like Tensora** (Tenzora, Tensoria): the same trademark risk, since sound counts
  as much as spelling.
