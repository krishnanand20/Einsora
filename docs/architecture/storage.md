# Storage

## Solo and Lab (node agents)

| Data | Where | Notes |
|---|---|---|
| Control plane store | SQLite file in the data directory, or Postgres for larger Labs | `einsora backup` (planned) takes a consistent copy |
| Data directory | macOS: `~/Library/Application Support/Einsora`. Linux: `/var/lib/einsora` (service) or `~/.local/share/einsora` (user). | Planned |
| Workload disks (VM images, notebook workspaces) | Local disk on the machine that runs the workload | **No replication.** A workload is tied to its machine's disk. |
| Model cache | One per machine, deduplicated by sha256 and shared between workloads | Validated before first use |
| Disk caps | Per workload, and a total for guests on shared machines (ADR-0014) | Planned |

**Durability:** a Solo or Lab machine's disk is as safe as the machine. Back up the store, and keep
anything precious off community hosts.

## Datacenter (Kubernetes)

| Need | Phase 3 start | Later | Referenced as |
|---|---|---|---|
| Block (VM disks, notebook dirs, Postgres, Prometheus) | Longhorn | Ceph RBD | StorageClass **`einsora-block`** (cluster-config) |
| Object (models, datasets, backups) | SeaweedFS or Garage ([object-store](../../deploy/platform/object-store/README.md)) | Ceph RGW | S3 endpoint in `einsora-models` |
| Shared filesystem | none | CephFS | future `einsora-shared` |
| Local scratch | `emptyDir` on NVMe | TopoLVM or local-path | future |

The control plane only knows `einsora-block` (`EINSORA_STORAGE_CLASS`), so switching backends changes `deploy/`, not code.

**VM images** are CDI `DataSource`s in `einsora-images`, imported per region. Each VM's root disk is a clone of one.

### Durability and backups

| Data | Protection |
|---|---|
| Postgres (tenants, workloads, events, usage) | WAL archiving plus daily base backups, with monthly restore tests |
| etcd | `k3s etcd-snapshot` to object storage |
| User volumes | Not backed up by default. Snapshots are a later feature. |

**Regions:** every region keeps its own block and object storage, and workload data stays in its
region (ADR-0009). Only the control plane's store is shared across regions.

**Encryption:** volumes aren't encrypted at rest yet, and freed blocks can hold earlier users' data.
The plan is encrypted StorageClasses with per-volume keys.
