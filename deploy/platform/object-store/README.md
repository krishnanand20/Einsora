# Object store (Datacenter profile: models, datasets, backups)

**Open decision for the Datacenter profile (Phase 3, ADR-0005).** Solo and Lab installs don't need
this; they keep models in each machine's local cache. A datacenter needs S3-compatible storage for:

- model weights, written by `tensorafi-ml ingest` and read by the KServe storage initializer
- backups (Velero, Postgres WAL archives)

| Option | Notes |
|---|---|
| **Rook-Ceph RGW** | The answer once there are ≥3 nodes; it comes with the Ceph cluster |
| **SeaweedFS** | Light, Apache-2.0, and has a Helm chart. A reasonable single-node choice. |
| **Garage** | Light and built for small self-hosted clusters. AGPL. |
| **MinIO** | The community edition stopped shipping prebuilt binaries and images in late 2025 and moved to maintenance mode. Check its current status before choosing it. |

All of these are free to use. The object store is **regional**: every region runs its own, and a
region's model weights and backups stay in it (ADR-0009). `tensorafi-ml ingest` uploads to the store of the
region where the model will be served.

Whichever you pick:

- Deploy it in the **`tensorafi-models`** namespace. cluster-config creates it with the label
  `tensorafi.dev/role: model-store`, and tenant NetworkPolicies allow egress only to that namespace's
  pods, which is how the KServe storage initializer reaches the weights.
- Use one bucket per purpose (`tensorafi-models`, `tensorafi-backups`) and scope credentials per bucket.
- Add the release to `helmfile.yaml.gotmpl` under Layer 5, behind `features.objectStore`.
