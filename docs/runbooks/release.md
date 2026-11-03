# Cut a release

How a maintainer publishes a version of Einsora: archives for macOS (Apple Silicon) and Linux,
`.deb` and `.rpm` packages, the source, signed checksums, and SBOMs on GitHub, and the Homebrew
formula in the tap ([ADR-0023](../adr/0023-releases-and-packaging.md)).
`.github/workflows/release.yml` does the work when a version tag is pushed; nothing is built on a
laptop.

> **Status:** written; no release has been cut yet. The pipeline is checked on every pull request
> by a snapshot build (the `packaging` job in CI), which publishes nothing, and the formula is
> built and installed from every commit (the `macos` job).

macOS builds aren't notarized: Einsora doesn't pay for an Apple Developer Program membership. A Mac
runs them when they're installed with `install.sh` or Homebrew, but not when they're downloaded with
a browser, and the release notes say so. Never work around this by removing the quarantine flag.

## Once: the Homebrew tap

The release works without it; the formula is published once it's set up.

1. Create a public repository `krishnanand20/homebrew-tap` with a `main` branch (an empty README is
   enough).
2. Create a [fine-grained GitHub token](https://github.com/settings/personal-access-tokens/new) with
   access to that repository only, and **Contents: Read and write**. Give it an expiry, and note it
   so you can renew it.
3. Add it to the Einsora repository's Actions secrets
   (**Settings → Secrets and variables → Actions**) as `HOMEBREW_TAP_TOKEN`.

Users then install with `brew install krishnanand20/tap/einsora`, which builds from source.

## Each release

1. Make sure `main` is green in CI, including the `packaging` job.
2. Pick the version: `v0.MINOR.PATCH` while Einsora is pre-1.0. A tag with a suffix, such as
   `v0.2.0-rc.1`, is published as a pre-release, and the formula isn't updated. Make the first release
   of all a pre-release (`v0.1.0-rc.1`): the release notes and signing run only for a real tag, so
   that's their first test. `latest` and `install.sh` skip pre-releases; try it with
   `EINSORA_VERSION=v0.1.0-rc.1`.
3. Tag the commit on `main` and push the tag:

   ```bash
   git checkout main && git pull
   git tag -a v0.1.0 -m "Einsora v0.1.0"
   git push origin v0.1.0
   ```

   The workflow refuses a tag on a commit that isn't on `main`.
4. Watch the **release** workflow in the Actions tab. For a full release, its `homebrew` job
   publishes the formula, or says it couldn't because `HOMEBREW_TAP_TOKEN` isn't set.
5. Check the release on GitHub. Expect these files:
   - three archives, `einsora_darwin_arm64.tar.gz`, `einsora_linux_amd64.tar.gz`, and
     `einsora_linux_arm64.tar.gz`, each with an `.sbom.json`;
   - two `.deb` and two `.rpm` packages;
   - the source, `einsora-0.1.0.tar.gz`;
   - `checksums.txt` and `checksums.txt.sigstore.json`.

## Check it

On a Mac and on a Linux machine:

```bash
curl -fsSL https://einsora.com/install.sh | sh     # says it checked the SHA-256
einsora version                                     # einsora 0.1.0 (commit …)
```

With [cosign](https://docs.sigstore.dev/cosign/system_config/installation/) installed, the script
checks the signature too, and says so. To check by hand, follow the release notes' **Verify**
section. For a full release with the tap set up, also try `brew install krishnanand20/tap/einsora`
(it builds for a few minutes) and `brew test einsora`.

## If something's wrong

- **The workflow failed before publishing:** fix it on `main`, delete the tag
  (`git push --delete origin v0.1.0`, then `git tag -d v0.1.0`), and tag again.
- **A published release is broken:** don't replace its files, which people may have already
  verified. Mark it as a pre-release on GitHub, so `latest` points elsewhere, and release a new patch
  version, which also replaces the formula.
- **The `homebrew` job failed** (an expired token, say): fix it, then re-run that job from the
  Actions tab. The release itself is already published.
