# AGENTS.md: infra/ (Datacenter profile: server bootstrap)

These rules add to the root [AGENTS.md](../AGENTS.md); read that first.

**`infra/` bootstraps Linux servers for the Datacenter profile** ([ADR-0012](../docs/adr/0012-deployment-profiles.md)).
Macs and personal Linux machines don't use Ansible. They install the `tensorafi` binary and run `tensorafi up` or
`tensorafi agent` (Phases 1–2).

## Layout

```
ansible/site.yml               entry point: base → iommu → wireguard → k3s
ansible/inventory/             hosts.example.yml is committed; hosts.yml (real IPs) is gitignored
ansible/group_vars/all/        main.yml (non-secret); vault.yml (ansible-vault encrypted, gitignored)
ansible/roles/<role>/          tasks/, handlers/, templates/, defaults/
scripts/                       read-only diagnostics to run on a node (IOMMU groups, GPU inventory)
```

## Rules

- **Never run playbooks against real hosts.** Agents may run `ansible-lint`, `ansible-playbook --syntax-check`, and, if asked, `--check --diff`. Applying changes is for humans.
- **Idempotent tasks only.** Every task must be safe to re-run. Use `creates:`, `changed_when:`, or proper modules rather than bare `shell`.
- **Reboots are opt-in.** Kernel cmdline changes notify a handler that reboots only if `iommu_allow_reboot: true`.
- **Secrets** (k3s token, WireGuard private keys) live only in `group_vars/all/vault.yml`, encrypted with `ansible-vault`. Reference them as `vault_*` variables.
- Use fully qualified module names (`ansible.builtin.apt`, not `apt`).
- **Scripts** in `scripts/` are diagnostics only: read-only, `set -euo pipefail`, and they must pass shellcheck.
- Keep k3s flags in sync with deploy/: Cilium needs flannel, network policy, and kube-proxy disabled, and MetalLB needs servicelb disabled.
- **One inventory per region** (ADR-0009). Never mix regions' hosts in one inventory, because each region is its own cluster with its own network ranges.
- **CPU worker nodes** (ADR-0010) and control-plane CPU servers use `base`, `wireguard`, and `k3s`, but not `iommu`. Put them in a separate group (planned: `cpu_nodes`).
- The fleet runs on **UTC**, so logs and usage-accounting events line up across regions.

## Commands

```bash
cd infra/ansible
uvx --from ansible-core ansible-playbook --syntax-check -i inventory/hosts.example.yml site.yml
uvx ansible-lint
shellcheck ../scripts/*.sh
```
