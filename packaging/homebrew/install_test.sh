#!/bin/sh
# Builds, installs, and tests Einsora's Homebrew formula from an archive of this commit, as
# `brew install krishnanand20/tap/einsora` does from a release. CI's macOS job runs it. It changes
# the machine's Homebrew (a local tap, einsora, and Go and Node to build with), so it runs only in CI.
set -eu

[ "${CI:-}" = true ] || { echo "install_test.sh changes this machine's Homebrew, so it runs only in CI" >&2; exit 2; }

here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# A release's source archive is `git archive` of its tag, as GoReleaser makes it (.goreleaser.yaml).
archive="$tmp/einsora-0.0.0.tar.gz"
git -C "$here/../.." archive --format=tar.gz --prefix=einsora-0.0.0/ -o "$archive" HEAD
sha=$(shasum -a 256 "$archive" | cut -d ' ' -f 1)

brew tap-new --no-git einsora/test
sh "$here/formula.sh" "file://$archive" "$sha" >"$(brew --repository einsora/test)/Formula/einsora.rb"
brew style einsora/test/einsora
brew install --build-from-source einsora/test/einsora
brew test einsora/test/einsora
echo "homebrew formula: built, installed, and tested"
