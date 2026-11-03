# 0023. Release with GoReleaser, sign with Sigstore, and run in the background as the user

- **Status:** Proposed (roadmap step 11; built, no release cut yet)
- **Date:** 2026-11-03
- **Deciders:** Krishnanand
- **Builds on:** ADR-0003 (one Go binary), ADR-0012 (Solo runs as one process), ADR-0013 (Apple
  Silicon and Linux)

## Context

Phase 1 is done when one command installs Einsora on a fresh Mac or Linux box (roadmap). Until
now, it had to be built from source. A release needs:

- **Binaries for every platform:** macOS on Apple Silicon, and Linux on amd64 and arm64, each with
  the dashboard built in, and a version it can report.
- **A way to tell that a download is ours:** the binary gets the GPU, the user's models, and their
  tokens.
- **Installs people expect:** a script for any Linux or Mac, Debian and RPM packages, and Homebrew.
- **Running in the background:** starting at login, and again after a crash, without the user
  writing launchd or systemd files.

macOS adds two constraints:

- **Notarization.** Gatekeeper refuses to run a downloaded binary that Apple hasn't notarized, if
  it carries the quarantine flag that browsers and Homebrew casks set. Notarizing needs an Apple
  Developer Program membership, at $99 a year. Einsora's publisher has chosen not to pay for one.
  Einsora is free for its users either way (ADR-0011).
- **Homebrew.** Homebrew installs prebuilt programs as casks. Since September 2026 it supports only
  casks that pass Gatekeeper, and it has removed `--no-quarantine`. GoReleaser has deprecated
  generating formulae that install prebuilt binaries (v2.10). Formulae that build from source, as
  Homebrew's own do, need no notarization: Homebrew doesn't quarantine what it builds.

## Decision

**GoReleaser builds every release** (`.goreleaser.yaml`). The release workflow runs it for a
`v*.*.*` tag on a commit that's already on `main`, with GoReleaser's version and every action pinned
by commit. It builds:

- binaries for darwin/arm64, linux/amd64, and linux/arm64, CGO-free, with the dashboard embedded and
  the version stamped in (`einsora version`). File times come from the commit, so builds repeat.
- an archive per platform, named without its version (`einsora_linux_amd64.tar.gz`), so
  `releases/latest/download/<name>` always finds the latest;
- `.deb` and `.rpm` packages, which install `/usr/bin/einsora` and its licence, and nothing else;
- the source (`einsora-0.1.0.tar.gz`, `git archive` of the tag), for the Homebrew formula;
- `checksums.txt` with every file's SHA-256, and an SBOM (syft) for each archive.

CI builds a snapshot from the same file, publishing nothing, so a broken release config fails a pull
request rather than a tag.

**Releases are signed with Sigstore, keyless.** cosign signs `checksums.txt` with the release
workflow's own GitHub identity, so there's no long-lived key to keep or leak. The signature bundle
is published beside it (`checksums.txt.sigstore.json`), and the release notes say how to verify.

**`install.sh` is the one-line install**: `curl -fsSL https://einsora.com/install.sh | sh`. Its
source is `packaging/install.sh`, which the website build publishes as is. It:

- downloads over HTTPS only, redirects included, the archive for this machine and the checksums.
  It needs curl: wget can't stop a redirect from leaving HTTPS, and the checksums come the same way;
- refuses an archive whose SHA-256 doesn't match, and checks the signature when cosign is installed
  (it says so when it isn't);
- installs only the binary, a regular file, to `/usr/local/bin` (with sudo if needed) or
  `EINSORA_INSTALL_DIR`;
- starts nothing, and creates no account.

**macOS builds aren't notarized.** They're signed ad hoc, as Go signs every Apple Silicon binary.
They run when installed by the script or Homebrew, which set no quarantine flag, but not from a
browser download, and the release notes say so. Einsora never removes the quarantine flag to get
past Gatekeeper.

**Homebrew gets a formula that builds from source**, not a cask: `brew install
krishnanand20/tap/einsora`, on a Mac or Linux. Its source is `packaging/homebrew/einsora.rb`. For
each full release, not a pre-release, the release workflow fills in the source archive and its
SHA-256, taken from the signed `checksums.txt`, and pushes it to `krishnanand20/homebrew-tap`. That
happens in a job of its own, holding only the tap's token (`HOMEBREW_TAP_TOKEN`). CI builds,
installs, and tests the formula on macOS from every commit.

**`einsora service install` runs `einsora up` in the background, as the user**, on every install
method:

- **macOS:** a launch agent (`~/Library/LaunchAgents/com.einsora.up.plist`), in the user's session,
  where Metal is. launchd starts it at login, and again if it exits with an error, but not after a
  clean stop. Its output goes to the data directory.
- **Linux:** a systemd user service (`~/.config/systemd/user/einsora.service`), restarted on
  failure, with output to the journal. Keeping it running after logout (`loginctl enable-linger`)
  is the user's choice, and the command says how.

The definition records the PATH of the shell it's installed from (launchd and systemd start with
less, which would hide `uv` and `docker`) and the data directory, named explicitly so it can't
differ from the terminal's. It records other variables only when the user names them with `-env`,
such as `HF_TOKEN`. The file is readable by its owner only. `einsora service uninstall` and `status`
undo and describe it. Packages and the script install no service: running in the background is an
explicit choice. The definitions are built by a pure function and tested as data
(`internal/autostart`).

## Consequences

- **Installing takes one command once a release exists.** Cutting one is a manual step
  ([release runbook](../runbooks/release.md)): push a tag. The tap repository and its token are set
  up once, by hand.
- **Verification is opt-in for users.** The SHA-256 check always runs. The signature check needs
  cosign, which most machines don't have, and the script says when it skipped it.
- **A Mac can't run a copy downloaded with a browser.** Users install with the script or Homebrew.
- **Installing with Homebrew takes a few minutes:** it installs Go and Node to build with, and
  builds. Bottles (Homebrew's prebuilt formulae, which it doesn't quarantine) could cut that later,
  built by the tap's own CI.
- **Upgrades go through whatever installed it:** re-run the script, `apt`/`dnf`, or
  `brew upgrade`. There are no automatic updates. A service records the binary's path, so
  replacing the binary in place keeps it working; `einsora service install` again picks up a new
  PATH.
- **Native workloads keep running when the service stops**, as when `einsora up` stops: they're in
  their own process groups on macOS, and Docker owns them on Linux.
- **Windows isn't covered** (Phase 5).

## Alternatives considered

- **Release scripts of our own:** GoReleaser is the standard for Go, and covers cross-builds,
  packages, signing, SBOMs, and source archives in one reviewed file.
- **Notarizing, with an Apple Developer Program membership:** $99 a year. It would make browser
  downloads run, and allow a cask. Adding it later is GoReleaser's `notarize` section, Apple's
  credentials as secrets, and a cask.
- **A cask without notarization:** Homebrew no longer supports casks that fail Gatekeeper. Making
  one run by removing the quarantine flag in the cask is the bypass Homebrew removed
  `--no-quarantine` to stop.
- **A formula that installs the prebuilt binary:** GoReleaser has deprecated generating these, and
  Homebrew expects prebuilt binaries to be casks. Building from source is how Homebrew's own
  formulae work.
- **`brew services`:** the formula could define one, but it would help only Homebrew users;
  `einsora service` works on every install method.
- **Signing with a long-lived key (GPG or a cosign key pair):** a secret to guard and rotate forever.
  Keyless signing ties each release to the workflow that built it.
- **A system-wide service with its own user:** Solo runs as its owner: data in their home, Metal in
  their session, and Docker through their group. A system service would need a second set of paths
  and permissions.
- **Packages that install and start a service:** that would change the machine more than the user
  asked for. `einsora service install` is one explicit step.
