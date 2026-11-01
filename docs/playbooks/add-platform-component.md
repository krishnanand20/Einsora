# Playbook: add a Datacenter platform component

For third-party add-ons to the **Datacenter profile** (`deploy/`), such as Kueue, Velero, CloudNativePG,
or an object store. Solo and Lab installs don't use `deploy/`. Support for Macs and personal Linux
machines belongs in the node agent's drivers ([ADR-0013](../adr/0013-node-agent-and-apple-silicon.md)).

1. **Justify it.** Which layer is it in, what does it replace or enable, and which phase needs it? If it's a new category or replaces a component, write an ADR first ([write-an-adr.md](write-an-adr.md)).
   Also say whether it runs **in every regional cluster** (most add-ons) or **once alongside the control plane** (Keycloak, the control plane's Postgres). A regional component must never depend on another region (ADR-0009).
2. **Pick a version.** Use the current stable release from upstream. Read its install and upgrade notes, and note its Kubernetes version range against our k3s version. If you can't verify the version, say so in the PR. Don't guess.
3. **Check the licence.** TensoraFi is free and open (ADR-0011), so add-ons must be free to use, with no licence keys or paid tiers required.
4. **helmfile:** add a release under the right layer heading in `deploy/helmfile.yaml.gotmpl`:
   - an exact `version:`
   - `installed: {{ .Values.features.<name> }}`, unless it's core to the profile
   - `needs:` for real ordering dependencies
5. **Values:** add `deploy/platform/<name>/values.yaml`. Override only what you need, and comment every non-default choice. Use `.gotmpl` only if environment values are required. Secrets go in `existingSecret`-style references; list them in `deploy/README.md`.
6. **Custom resources** belong in `deploy/charts/cluster-config/templates/`, gated by the feature flag. Make `cluster-config` `need` the operator.
7. **Feature flag:** add it to **every** `deploy/environments/*.yaml`.
8. **Isolation:**
   - Can tenant pods reach it? They can't by default (tenant egress blocks private ranges). If they must, add a narrowly scoped rule in `manifests/tenant.go` with a reason, and get a `tenancy-reviewer` pass.
   - Does it need cluster-wide RBAC? Note that in the PR.
9. **Observability:** enable its ServiceMonitor and dashboards. Add alerts for its failure modes to [observability.md](../architecture/observability.md).
10. **Docs:** add a row to the components table in [overview.md](../architecture/overview.md) and the versions table in `deploy/README.md`, and update the roadmap if a checkbox is affected.
11. **Verify:** run `make deploy-check` and a `platform-reviewer` pass. **Never apply to a cluster**; that's a human step done through the runbooks.
