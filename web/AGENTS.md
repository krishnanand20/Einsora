# AGENTS.md: web/ (TypeScript dashboard)

These rules add to the root [AGENTS.md](../AGENTS.md); read that first.

The dashboard is the same in every profile. For Solo, it will be embedded in the `einsora` binary
(ADR-0012), so it must work when served from `127.0.0.1` with no separate web server.

## Layout

```
src/api/types.ts      mirrors controlplane/internal/api/json.go; change both together
src/api/client.ts     fetch wrapper, ApiError (with requestId), auth headers, and file downloads
src/workloads.ts      pure UI rules (which actions apply to which state); unit-tested
src/machines.ts       how machine facts are shown (last seen, memory, GPUs, system); unit-tested
src/output.ts         a workload's output as shown: merging pages, the most recent 1 MiB, terminal escapes removed; unit-tested
src/pages/            one component per route
src/styles.css        plain CSS with light/dark tokens; no CSS framework
```

## Rules

- **The API is the only backend.** Never call Kubernetes, agents, IdP admin APIs, or anything else directly.
- **Server state lives in TanStack Query.** Use `useState` for form and UI state only. Poll with `refetchInterval` only while something is mid-transition (`isSettling`), or, for a workload's output, while it runs.
- **Don't duplicate business rules.** The UI may hide buttons, as `availableActions` does, but the API decides. Show the API's error message to users verbatim.
- **Show what actually runs** (ADR-0009, ADR-0010): always display a workload's region and what it runs on with `classShown`, which names the requested class whenever a fallback placed it elsewhere. Show the machine it runs on, and its driver (the Machines page lists your machines and whether each is online). Never hide a fallback. The launch form picks a region and offers only classes that can run the image there (`launchableClasses`); a machine picker and an explicit fallback opt-in come later.
- **Be honest about sharing** (ADR-0014): before a user launches on someone else's machine, show that the host can see their data. Show owners exactly what they're sharing, and when.
- **Nothing hidden** (rule 14, ADR-0015): show every timeline reason in full, never truncated or summarized away. Keep **Download full log** on the workload page, and include anything new the API exports. Show a workload's output notes (what isn't collected, or was lost or removed) whenever they're there, and say when the page leaves earlier output out. Show the request ID with every error (`ApiError` already appends it). Take download file names from `filenameFromDisposition`, which strips directories. Deleted workloads stay reachable: the workload page loads with `include_deleted`, and the list has **Show deleted**. Show notes (`from` equals `to`) as notes, via `describeChange`.
- **Build workload URLs with `workloadPath`**, which encodes the ID, so an ID can never point at a different API path.
- **No money anywhere** (rule 12): no prices, plans, upgrade prompts, or payment screens. Usage pages show hours, never currency.
- **Auth:** in `client.ts` only. Local accounts sign in on `/login` and get an HttpOnly session cookie, which the browser sends by itself; every request carries `X-Einsora-CSRF`. A 401 from `/v1/auth/me` sends you to sign-in, and `safeNext` keeps the return path inside the dashboard. Dev mode adds `X-Dev-Tenant`. Planned: OIDC with PKCE (Datacenter). Never store tokens in `localStorage`; a new API token is shown once on the tokens page and kept only in memory.
- **Relative URLs only:** call `/v1/...`, never hard-coded hosts, so the embedded build works on any address.
- **Dependencies:** keep the list short. Ask before adding a UI kit, state library, or CSS framework.
- **Strict TypeScript:** `any` needs a comment explaining why.

## Commands

```bash
npm install
npm run dev         # :5173, proxies /v1 to the API on :8080 (run `make dev` first)
npm run typecheck
npm test
npm run build
```
