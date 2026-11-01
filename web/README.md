# web: Einsora dashboard

The dashboard, built with React, Vite, and TanStack Query. Users launch **GPU VMs**, **Jupyter
notebooks**, and **model endpoints** from a catalog-driven form, watch their state live, and stop,
start, or delete them. Each workload's page shows its full timeline (every state change and note, when,
why, and by whom) and a **Download full log** button that saves the complete export as JSON
([logs and transparency](../docs/architecture/logs-and-transparency.md)). **Show deleted** on the
workloads list brings back deleted workloads, whose pages and logs stay available. Error messages include
the request ID, so an operator can find the exact log lines.

```bash
make dev        # from the repo root: API on :8080
npm install
npm run dev     # http://localhost:5173
```

In dev mode, requests carry `X-Dev-Tenant: acme`. Set `VITE_DEV_TENANT` to act as another seeded
tenant: `globex` (untrusted), or `internal` (trusted, which also sees internal-only GPU classes).

How it's served:

- **Solo:** embedded in the `einsora` binary and served on localhost (planned).
- **Datacenter:** the static build is served by nginx (see `Dockerfile`). The Gateway routes `app.<domain>` here and `api.<domain>` to the control plane.

Planned (ADR-0009, 0010, 0013, 0014):

- a machines page showing Macs and Linux boxes with their GPUs, health, and sharing level
- a machine picker and a fallback opt-in on the launch form
- invites, teams, and admin pages (Phase 2); today there's sign-in, sign-out, and an **API tokens** page for local accounts (it already picks a region, when there's more than one, and offers only classes that can run the image there)
- the placed class and a fallback badge on each workload
- sharing settings and host disclosures
- usage reports in hours (never money)
- workload output logs, live (ADR-0015, Phase 1), and the team activity log (Phase 2)
