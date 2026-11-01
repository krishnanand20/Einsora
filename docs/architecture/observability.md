# Observability

## Solo and Lab: built into the dashboard

| Signal | Source | Status |
|---|---|---|
| Machine health: online, CPU, RAM, disk, temperature where available | Agent heartbeat | Inventory and last-seen time recorded (built, ADR-0016); disk, temperature, and the dashboard planned (Phase 1–2) |
| NVIDIA GPU use and memory | NVML, through the agent | Planned |
| Apple GPU use and unified memory | macOS IOKit, through the agent (no root needed) | Planned (approach to validate) |
| Workload timeline | Every state change and note with its reason; readable and exportable by the owner ([logs and transparency](logs-and-transparency.md)) | **Built** |
| Workload output logs | The agent ships the driver's stdout and stderr to the control plane, for the workload's owner only | Planned (Phase 1) |
| Control plane logs | `slog` to stderr; `EINSORA_LOG_LEVEL=debug` for detail and `EINSORA_LOG_FILE` for a complete copy on disk; `request_id` on every request line | **Built** |
| Alerts | Dashboard notifications: machine offline, disk nearly full, workload failed | Planned (Phase 2) |
| Prometheus | `/metrics` on the control plane and agents, for people who already run Prometheus | Planned (Phase 3) |

## Datacenter

| Signal | Tool | Notes |
|---|---|---|
| Metrics | kube-prometheus-stack (Prometheus, Alertmanager, Grafana) | ServiceMonitors from every chart are picked up |
| GPU metrics | NVIDIA DCGM exporter (through the GPU Operator) | Container-mode nodes only; passthrough GPUs are invisible to the host |
| Logs | Loki (single binary) + Grafana Alloy | Pod logs labelled with `namespace`, `pod`, `container`, and `tenant` |
| Network flows | Cilium Hubble | Policy debugging and abuse detection |
| Hardware | IPMI/BMC | Exporter or SNMP |

Each region runs its own stack, so logs stay in-region. A central Grafana can query every region read-only.

### Alerts that matter

| Alert | Source | Why |
|---|---|---|
| GPU XID errors | DCGM | XID 79 (the GPU fell off the bus) or 48 (double-bit ECC) means draining the node, and possibly an RMA |
| ECC double-bit errors, retired pages | DCGM | Hardware degradation |
| GPU temperature or power at its limit for 10 min or more | DCGM, IPMI | Cooling problems, common in hot climates and summer months |
| Node NotReady, or disk over 85% | kube-state-metrics, node-exporter | |
| Longhorn or Ceph degraded | Their exporters | Data at risk |
| Workloads stuck in `provisioning`, or queue depth growing | Control plane `/metrics` (planned) | Stuck admission or no capacity |
| Usage accounting lag (last closed hour not recorded after 30 min) | Control plane `/metrics` (planned) | Quotas and fair share depend on it |
| Fallback rate rising | Control plane and gateway metrics (planned) | A class is short of capacity |
| Inference gateway errors after failover | Gateway metrics (planned) | Endpoints failing beyond what fallbacks absorb |
| Certificate expiry within 14 days | cert-manager | |
| Postgres down or replication lag | Postgres exporter | |

## User data in logs

Workload logs are **user data** in every profile.

- **Visibility:** only the workload's owner sees them in the dashboard, CLI, and export ([logs and transparency](logs-and-transparency.md)). Another tenant gets 404. In a datacenter, Loki access is limited to on-call staff for operational reasons, and audited. Retention is 14 days (`loki.yaml`).
- **Never collected:** logs from inside VMs, and anything from guest workloads on community hosts beyond the driver's own status lines.
