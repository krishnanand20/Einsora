# AGENTS.md: deploy/ (Datacenter profile)

These rules add to the root [AGENTS.md](../AGENTS.md); read that first.

**`deploy/` is the Datacenter profile only** ([ADR-0012](../docs/adr/0012-deployment-profiles.md)): Kubernetes
add-ons and charts. Solo and Lab installs (Macs, personal Linux machines) use none of it; they run the
`tensorafi` binary and node agents. Don't add Mac or personal-machine support here.

## Layout

```
helmfile.yaml.gotmpl          every add-on in install order, grouped by layer, with feature flags
catalog.yaml                  the Datacenter launch catalog: every region, and only classes Kubernetes can run
environments/<env>.yaml       one file per regional cluster: flags, IPs, GPU devices, storage
platform/<component>/         values files (or kustomizations) for third-party components
charts/cluster-config/        OUR cluster objects: namespaces, MetalLB pool, issuer, Gateway,
                              KubeVirt/CDI CRs, tensorafi-vllm runtime, tensorafi-block StorageClass
charts/tensorafi/               the control plane (api, controller, metering, migrate Job) and web
examples/                     hand-applied smoke tests used by the runbooks
```

## Rules

- **Never apply.** Agents run `make deploy-check` (helm lint, helm template, kubeconform) and nothing that touches a cluster. `helmfile apply` and `sync` are for humans.
- **Pin everything.** Chart `version:` and image tags must be exact; no `latest` and no ranges. When you bump a version, read the upstream upgrade notes and record the date in `README.md#versions`.
- **Free to use** (rule 12): only add components that need no licence key or paid tier.
- **No secrets in values.** Reference Secrets created out of band (`existingSecret`, `secretKeyRef`). List any new one under "Secrets created out of band" in `README.md`.
- **Keep the control plane in sync.** If you change a kind the controller creates, update `charts/tensorafi/templates/controller.yaml` RBAC and `controlplane/internal/cluster/kube/kube.go` (`allowed`) together. If you change `tensorafi.dev/role` namespace labels, check `controlplane/internal/cluster/manifests/tenant.go`.
- **Isolation-sensitive files** need a `tenancy-reviewer` pass:
  - `gpu-operator/values.yaml`: never add time-slicing between users
  - `serving-runtime.yaml`: keep `--load-format=safetensors`; never add `--trust-remote-code`
  - Gateway `allowedRoutes`
  - anything that grants RBAC
- **Order matters.** Express dependencies with `needs:`. Custom resources go in `cluster-config`, which `needs` their operators.
- **Feature flags:** a component that isn't core gets an `installed: {{ .Values.features.<x> }}` flag. Add the flag to every environment file.
- **Regions (ADR-0009):** each environment's `region` must be listed in `catalog.yaml`; the chart refuses to render otherwise. Most add-ons deploy into every regional cluster. Mark control-plane-only components (Keycloak, the control plane's Postgres) as such. A regional cluster must never depend on another region. While there's one region, its cluster also hosts the control plane.
- **Fallbacks (ADR-0010):** the planned `tensorafi-vllm-cpu` runtime follows the same rules as `tensorafi-vllm` (safetensors only, no remote code). CPU worker nodes get the same isolation runtime classes as GPU nodes.

## Commands

```bash
make deploy-check                                  # from repo root: lint, render, and validate
helmfile -e single-node template > /tmp/out.yaml   # full render (needs helmfile and kustomize)
```
