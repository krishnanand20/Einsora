#!/bin/sh
# Checks formula.sh without Homebrew: it fills in a release's archive and SHA-256, the result is
# valid Ruby, and it refuses anything else. `make test-packaging` runs it; install_test.sh (CI only)
# builds and installs the formula.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

url=https://github.com/krishnanand20/Einsora/releases/download/v0.1.0/einsora-0.1.0.tar.gz
sha=$(printf '%064d' 0)

sh "$here/formula.sh" "$url" "$sha" >"$tmp/einsora.rb"
grep -q "^  url \"$url\"\$" "$tmp/einsora.rb" || { echo "formula.sh: url not filled in" >&2; exit 1; }
grep -q "^  sha256 \"$sha\"\$" "$tmp/einsora.rb" || { echo "formula.sh: sha256 not filled in" >&2; exit 1; }
if grep -q '@URL@\|@SHA256@' "$tmp/einsora.rb"; then echo "formula.sh: a placeholder is left" >&2; exit 1; fi
if command -v ruby >/dev/null 2>&1; then
  ruby -c "$tmp/einsora.rb" >/dev/null
else
  echo "formula.sh: ruby isn't installed, so the Ruby syntax check was skipped" >&2
fi

refuses() {
  if sh "$here/formula.sh" "$1" "$2" >/dev/null 2>&1; then
    echo "formula.sh accepted: $1 $2" >&2
    exit 1
  fi
}
refuses https://example.com/einsora-0.1.0.tar.gz "$sha"
refuses http://github.com/krishnanand20/Einsora/releases/download/v0.1.0/einsora-0.1.0.tar.gz "$sha"
refuses "$url\" do system \"true" "$sha"
refuses "$url" "$(printf '%063d' 0)"
refuses "$url" "$(printf '%064d' 0 | tr 0 g)"
refuses "$url" ""
echo "formula.sh: all checks passed"
