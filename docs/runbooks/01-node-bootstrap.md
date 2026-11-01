# 01: Node bootstrap (Datacenter profile)

For the Kubernetes Datacenter profile ([ADR-0012](../adr/0012-deployment-profiles.md)). For one Mac or Linux PC, use [00-quickstart.md](00-quickstart.md) instead.

Takes a racked server to a Ready k3s node with the platform installed. It takes about 2 hours the first time.

## Prerequisites

- A server cabled with data NICs and a BMC, with BMC access working
- Static IP plan filled in, using this region's own non-overlapping ranges ([networking.md](../architecture/networking.md))
- A region ID for this cluster (for example `dc-1`) and its own inventory and environment file ([ADR-0009](../adr/0009-regions-global-control-plane-regional-cells.md))
- An admin workstation with Ansible, helm, helmfile, kustomize, kubectl, and WireGuard

## 1. BIOS/UEFI

Set these in the BMC console:

| Setting | Value |
|---|---|
| Intel VT-x and VT-d / AMD SVM and AMD-Vi (IOMMU) | **Enabled** |
| SR-IOV | Enabled |
| Above 4G Decoding | **Enabled** (large GPU BARs) |
| ACS (Access Control Services), if offered | Enabled (gives cleaner IOMMU groups) |
| Boot mode | UEFI only, CSM disabled |
| Power profile | Maximum performance; disable deep C-states if latency matters |

Also update the BIOS and BMC firmware to the vendor's latest stable release first.

## 2. Install Ubuntu 24.04 LTS Server

- Use a minimal install with OpenSSH, a static IP on the data NIC, and your SSH key for user `ubuntu`.
- Put the OS on the RAID 1 boot disks. Leave the data NVMe drives **unpartitioned** for Longhorn or Ceph.

## 3. Run Ansible

```bash
cd infra/ansible
cp inventory/hosts.example.yml inventory/hosts.yml        # set ansible_host, node_ip, wireguard_address
cp group_vars/all/vault.yml.example group_vars/all/vault.yml && $EDITOR group_vars/all/vault.yml
ansible-vault encrypt group_vars/all/vault.yml
ansible-galaxy collection install -r requirements.yml
ansible-playbook site.yml --check --diff --ask-vault-pass       # review
ansible-playbook site.yml --ask-vault-pass -e iommu_allow_reboot=true
```

**Expected:** the node reboots once (IOMMU), and k3s is running.
`sudo k3s kubectl get nodes` shows the node **NotReady**. That's correct, because there's no CNI until Cilium is installed.

## 4. Verify the node

```bash
sudo /path/to/infra/scripts/check-iommu.sh    # each GPU in its own IOMMU group
/path/to/infra/scripts/gpu-inventory.sh        # note the [10de:xxxx] IDs
```

Put the GPU device IDs into `deploy/environments/<env>.yaml` under `gpuPassthrough.devices`.

## 5. Get the kubeconfig (over WireGuard)

```bash
scp ubuntu@<node>:/etc/rancher/k3s/k3s.yaml ~/.kube/einsora.yaml   # needs sudo on the node to read
sed -i '' 's/127.0.0.1/<node-ip>/' ~/.kube/einsora.yaml
export KUBECONFIG=~/.kube/einsora.yaml
```

## 6. Install the platform

```bash
kubectl -n observability create secret generic grafana-admin \
  --from-literal=admin-user=admin --from-literal=admin-password="$(openssl rand -base64 24)"
cd deploy
helmfile -e single-node template > /tmp/render.yaml && less /tmp/render.yaml
helmfile -e single-node apply
```

**Expected:**

- `cilium status` reports OK
- the node is **Ready**
- `kubectl get pods -A` shows everything Running within about 20 minutes
- GPU Operator pods run on the node

Record what you verified in the [deploy/README.md versions table](../../deploy/README.md#versions).

Next: [02-gpu-passthrough.md](02-gpu-passthrough.md).
