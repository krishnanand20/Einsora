# Incident response

For whoever operates a TensoraFi instance: an individual, a team, a company, or a pool operator.
In Solo, most of this collapses into "check the dashboard and `tensorafi` logs".

## Severity

| Sev | Meaning | Response |
|---|---|---|
| **1** | User data at risk, a cross-tenant or guest-to-host exposure, or the whole instance down | Act now. Tell affected users within 1 hour. |
| **2** | One machine, node, or workload kind down; usage accounting wrong (quotas and fair share affected) | Fix the same day |
| **3** | Degraded but working: slow admission, one GPU lost | Fix this week |

## First five minutes

1. **Acknowledge** and write down what you see, the start time, and the severity.
2. **Stop the bleeding** before diagnosing:
   - cordon or disable a sick machine
   - pause the controller (stop it, or scale it to 0) if it's doing damage
   - suspend one abusive user
   - set a shared machine back to private
3. **Preserve evidence:** logs, events, and GPU history. Don't delete broken objects yet.
4. Keep a timeline as you go, with timestamps.

## Common incidents

| Incident | Profile | Detect | Action |
|---|---|---|---|
| **GPU XID 79** (the GPU fell off the bus) | Datacenter, Linux agents | DCGM or NVML alert, `dmesg` | Drain or disable, then reboot. If it repeats, the GPU or riser is failing. |
| **XID 48 / ECC double-bit** | Datacenter | DCGM | Drain; check retired pages; replace if it recurs |
| **Machine offline** | Lab | Dashboard | Usually asleep, rebooted, or on a new network; the agent reconnects by itself. Its workloads go to `failed` after the grace period. |
| **Node NotReady** | Datacenter | Alert | BMC console: power, kernel panic, or network? |
| **Disk full** | All | Alert at 85% | Model cache, old workload disks, logs, container images |
| **Workloads stuck in provisioning** | All | Dashboard, alerts | Check the controller logs, then the agent or runtime logs; compare capacity with what's allocated |
| **Store down** (SQLite file or Postgres) | All | readyz 503 | The API fails closed and running workloads continue. Restore from backup (`tensorafi backup` or Postgres backups). |
| **Region outage** | Datacenter, multi-site Labs | Alerts from that region | Only that region is affected (ADR-0009). Don't move workloads to another region unless users opted in. |
| **GPU class busy for a long time** | All | Queue depth, fallback rate | Expected under load: opted-in workloads use fallbacks and the rest queue. Add capacity or adjust quotas. |
| **Host reclaim storm** (many owners return at once) | Sharing | Preemption rate | Expected on shared pools. Check that re-queueing and fallbacks work, and that nothing was lost that should have been kept. |
| **User abuse** (attacks, illegal content, crypto mining on a shared host) | Lab, pools | Reports, flows, host complaints | Stop the workloads, keep logs, suspend the account, and follow the pool's AUP. Get legal advice if needed. |
| **Suspected guest-to-host or cross-tenant exposure** | All | Anything | **Sev 1.** Stop the affected workloads and set the affected machines to private. Preserve evidence, and investigate before restoring. Operators may have a legal duty to notify users and regulators (GDPR requires it within 72 hours; DPDP and other laws have their own rules). |
| **Certificate expired** | Datacenter | Alert at 14 days | `kubectl get certificate -A`; check the ACME route through the Gateway |

## After every Sev 1 or Sev 2

Within 5 working days, write a blameless postmortem: timeline, impact (users and GPU-hours affected),
root cause, what went well, and action items with owners. Turn every new failure mode into an alert,
a runbook step, or a test.
