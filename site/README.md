# site: the Einsora website

The project website, built with [Astro Starlight](https://starlight.astro.build): a landing page, the
thinking behind Einsora, the licence, and every document in this repository, with search, dark mode,
and diagrams. The repository's Markdown is the source; `scripts/sync-docs.mjs` turns it into pages at
build time.

```bash
make site-dev      # from the repository root: http://localhost:4321/
make site          # build into site/dist; fails on a broken link
```

**Hosting:** Cloudflare Pages builds and serves it at **https://einsora.com**, connected to this
repository: every push to `main` publishes it, and pull requests get preview URLs. Its settings
(Workers & Pages → the project → Settings → Build):

| Setting | Value |
|---|---|
| Root directory | `site` |
| Build command | `npm run build` |
| Build output directory | `dist` |
| Environment variable | `NODE_VERSION` = `24` |

`.github/workflows/site.yml` builds it on every change to the site or the docs, so a broken link fails
the pull request before it's published.

See [AGENTS.md](AGENTS.md) for the rules.
