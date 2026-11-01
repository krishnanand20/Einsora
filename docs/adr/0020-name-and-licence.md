# 0020. Name the product TensoraFi, and license it under MIT

- **Status:** Accepted
- **Date:** 2026-11-01
- **Deciders:** Krishnanand
- **Amends:** ADR-0011 (which left the licence open)

## Context

"Tensora" was a working name. Other projects already use it (a Python tensor library and a crypto
token among them), so the roadmap asked for a final name, a rename in one change before the first
release, and an open-source licence.

## Decision

**The product is TensoraFi.** Every identifier changed in one change:

| What | Before | Now |
|---|---|---|
| Product | Tensora | TensoraFi |
| Binary and CLI | `tensora` | `tensorafi` |
| Go module | `github.com/tensora/tensora` | `github.com/krishnanand20/tensorafi` |
| Environment variables | `TENSORA_*` | `TENSORAFI_*` |
| HTTP headers | `X-Tensora-*` | `X-TensoraFi-*` |
| Kubernetes labels and annotations | `tensora.io/*` | `tensorafi.dev/*` |
| Cookies | `tensora_session`, `tensora_access` | `tensorafi_session`, `tensorafi_access` |
| Python package and CLI | `tensora-ml`, `tensora_ml` | `tensorafi-ml`, `tensorafi_ml` |
| Helm chart, namespaces, Docker networks, container paths | `tensora…` | `tensorafi…` |
| Export format | `tensora.workload-export/v1` | `tensorafi.workload-export/v1` |
| Native workloads' run directories | `~/.tensora/run` | `~/.tensorafi/run` |
| Development database (compose) | user and database `tensora` | `tensorafi` |

Two things keep their old names. The Postgres function `tensora_forbid_mutation` was created by an
applied migration, which is never edited, and nothing outside the database sees it. Token prefixes
(`tsk_`, `tat_`) never had the name in them. ADRs 0001 to 0019 keep the working name and the old
identifiers, as history: read them through this table.

**The licence is MIT** (`LICENSE`): anyone may use, change, and redistribute TensoraFi, including
commercially. Every dependency's licence (Apache-2.0, BSD, MIT, and others like them) allows that,
and their notices ship with the binaries (packaging, roadmap step 11).

## Consequences

- Nothing is released yet, so there are no compatibility shims:
  - An existing `.env` needs `TENSORA_` renamed to `TENSORAFI_`.
  - Browser sessions end, so users sign in again.
  - Native workloads started before the rename have their sockets in `~/.tensora/run`: stop them
    before upgrading.
  - The development database's volume must be recreated (`docker compose down -v`), since its
    user and database are named `tensorafi` now.
- `go install` works once the GitHub repository is renamed to `tensorafi`. GitHub redirects the old
  URL.
- Labels use `tensorafi.dev`, which the project must register. A trademark search in the main
  markets (classes 9 and 42) is still to do before the first release.
- MIT has no explicit patent grant. Apache-2.0 has one, and some companies prefer it for
  infrastructure. MIT is shorter, widely understood, and asks the least of anyone who reuses the
  code. A contributor's grant would come only from the licence's implied terms.

## Alternatives considered

- **Keep "Tensora":** others already use the name, so it couldn't be cleared.
- **Apache-2.0:** an explicit patent grant and a NOTICE convention, at the cost of a longer licence.
  It was the earlier recommendation; MIT was chosen for simplicity.
- **Rename after the first release:** every release under the old name would need compatibility
  for old environment variables, headers, labels, and cookies.
