# site: the Einsora website

The project website, built with [Astro Starlight](https://starlight.astro.build): a landing page, the
thinking behind Einsora, the licence, and every document in this repository, with search, dark mode,
and diagrams. The repository's Markdown is the source; `scripts/sync-docs.mjs` turns it into pages at
build time.

```bash
make site-dev      # from the repository root: http://localhost:4321/Einsora/
make site          # build into site/dist; fails on a broken link
```

It's published to GitHub Pages by `.github/workflows/site.yml` on every push to `main` that touches the
site or the docs. To publish at the project's own domain, set the repository variables `SITE_URL`
(for example `https://einsora.com`) and `BASE_PATH` (`/`), and add the domain under Settings → Pages.

See [AGENTS.md](AGENTS.md) for the rules.
