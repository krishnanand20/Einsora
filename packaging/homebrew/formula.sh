#!/bin/sh
# Prints Einsora's Homebrew formula for one release: formula.sh <source archive URL> <its SHA-256>.
# The release workflow publishes it to krishnanand20/homebrew-tap for each full release (ADR-0023);
# CI builds it from an archive of the commit (install_test.sh).
set -eu

[ $# -eq 2 ] || { echo "usage: formula.sh <source archive URL> <SHA-256>" >&2; exit 2; }
url=$1 sha=$2

case $url in
  https://github.com/krishnanand20/Einsora/releases/download/* | file:///*) ;;
  *) echo "formula.sh: not an Einsora release or a local file: $url" >&2; exit 2 ;;
esac
case $url in
  *[!A-Za-z0-9._~:/+-]*) echo "formula.sh: unexpected characters in $url" >&2; exit 2 ;;
esac
case $sha in
  *[!0-9a-f]* | "") echo "formula.sh: not a SHA-256: $sha" >&2; exit 2 ;;
esac
[ ${#sha} -eq 64 ] || { echo "formula.sh: not a SHA-256: $sha" >&2; exit 2; }

sed -e "s|@URL@|$url|" -e "s|@SHA256@|$sha|" "$(dirname "$0")/einsora.rb"
