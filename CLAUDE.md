# CLAUDE.md

@AGENTS.md

## Claude Code specifics

- **Skills** (`.claude/skills/`): `/verify`, `/new-adr`, `/add-api-resource`, `/add-platform-component`.
  Each wraps a playbook in `docs/playbooks/` so Claude, Codex, and humans follow the same steps.
- **Subagents** (`.claude/agents/`):
  - `tenancy-reviewer` — run after any change to `controlplane/internal/{api,service,store,cluster,controller,metering,access}`, agent or driver code, `deploy/`, or `ml/src/einsora_ml/validate.py`. It looks for:
    - cross-tenant leaks and broken isolation (the trust matrix)
    - weakened host protections
    - usage-accounting bugs
    - data leaving its region (rule 9) and hidden fallbacks (rule 10)
    - anything that adds pricing or paywalls (rule 12)
    - logs or exports that leak across tenants, leak secrets, or silently drop data (rule 14)
  - `platform-reviewer` — run after changes to `deploy/` or `infra/` (the Datacenter profile), or to packaging and service files. Checks pinning, secrets, ordering, cross-platform builds, and blast radius.
- **Permissions** (`.claude/settings.json`): secret files are denied, and cluster-mutating commands
  require explicit approval. Personal overrides go in `.claude/settings.local.json` (gitignored).
- **Component rules** load from nested `CLAUDE.md` files, which import the nested `AGENTS.md`.
- **Developing on a Mac** is expected: `make dev`, the tests, and `make build-all` all run on Apple Silicon.
- Personal, uncommitted notes: `CLAUDE.local.md` (gitignored).
