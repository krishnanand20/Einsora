# Add a machine or node

Every machine belongs to one region (ADR-0009), so use that region's inventory and environment file.

## Lab: add an agent machine (planned, Phase 2)

1. Install TensoraFi on the machine (see [00-quickstart.md](00-quickstart.md), or [mac-host.md](mac-host.md) for a Mac).
2. In the dashboard: Machines → Add machine → copy the enrolment token.
3. On the new machine, run `tensorafi agent --join <token>`. The agent dials out, so no ports need opening.
4. **Check:** the machine shows Online, with the right CPU, RAM, and accelerators (NVIDIA GPUs, or the Apple GPU and its unified memory).
5. **Burn-in** if it has a GPU: `tensorafi-ml burnin` (CUDA today; Metal planned).
6. **Sharing level:** new machines are **private**. Change this only if the owner wants to share (ADR-0014).

## Datacenter: add a Kubernetes node

**CPU worker nodes** (for CPU instances and fallback, ADR-0010) and control-plane CPU servers skip the
`iommu` role, the GPU steps, and the GPU burn-in. Check that their CPU capacity shows up for the CPU classes instead.

1. **Before racking:** firmware updates and BIOS settings as in [01, step 1](01-node-bootstrap.md#1-biosuefi).
2. **Install Ubuntu** as in 01, step 2.
3. **Inventory:** add the host to `infra/ansible/inventory/hosts.yml`:
   - under `k3s_servers` if you're growing to 3 servers for HA, otherwise under `k3s_agents`
   - set `gpu_workload_config` for its role (container or vm-passthrough)
   - add its WireGuard key to the vault
4. **Ansible**, limited to the new host:
   ```bash
   ansible-playbook site.yml --limit <new-host> --check --diff --ask-vault-pass
   ansible-playbook site.yml --limit <new-host> --ask-vault-pass -e iommu_allow_reboot=true
   ```
5. **Join check:** `kubectl get nodes` shows the node Ready, with GPU Operator pods running on it.
6. **Burn-in** on every GPU ([02, burn-in](02-gpu-passthrough.md#burn-in-required-before-anyone-else-uses-a-gpu)). If a GPU fails, keep the node out of service: cordon it and arrange a repair or replacement.
7. **Capacity:** if the node has a new GPU model, add a GPU class to `images/catalog.yaml` and the device to `gpuPassthrough.devices`.
   The controller picks up capacity automatically. Verify that its next admission pass uses the new GPUs.
8. **Storage:** with Longhorn, raise `storage.replicas` once there are at least 3 nodes, and check that existing volumes rebuild. With Ceph, check that new OSDs joined and wait for `HEALTH_OK`.
9. **Watch the first week:** XID and ECC errors, temperatures under real load, and power draw against the circuit budget.

## Removing a node or machine

1. **Tell the affected users first**, then stop or move their workloads. Datacenter: `kubectl cordon <node>`. Lab: disable the machine in the dashboard.
2. Datacenter: `kubectl drain <node> --ignore-daemonsets --delete-emptydir-data`.
3. Datacenter storage: for Ceph, mark the node's OSDs out and wait for rebalance. For Longhorn, evict its replicas.
4. Remove it: `kubectl delete node <node>` and remove the host from inventory, or revoke the agent's certificate in the dashboard.
