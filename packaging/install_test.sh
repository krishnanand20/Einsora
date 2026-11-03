#!/bin/sh
# Tests install.sh against fake releases on disk (EINSORA_RELEASES=file://...): no network, and
# nothing is installed outside a temporary directory. Run it with `make test-packaging`.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' INT TERM

if command -v cosign >/dev/null 2>&1; then
	echo "skipped: cosign is installed, and these fake releases aren't signed"
	exit 0
fi

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{ print $1 }'; else shasum -a 256 "$1" | awk '{ print $1 }'; fi
}

# release makes a release directory with an archive for every platform install.sh supports.
# $2 changes it: "tampered" alters an archive after its checksum; "unlisted" leaves the checksums
# empty; "symlink" ships einsora as a link.
release() {
	dir="$tmp/$1"
	mkdir -p "$dir/files"
	if [ "$2" = symlink ]; then
		ln -s /bin/sh "$dir/files/einsora"
	else
		printf '#!/bin/sh\necho "einsora 9.9.9 (test build)"\n' >"$dir/files/einsora"
		chmod 0755 "$dir/files/einsora"
	fi
	: >"$dir/checksums.txt"
	for platform in darwin_arm64 linux_amd64 linux_arm64; do
		tar -czf "$dir/einsora_$platform.tar.gz" -C "$dir/files" einsora
		if [ "$2" != unlisted ]; then
			printf '%s  %s\n' "$(sha256 "$dir/einsora_$platform.tar.gz")" "einsora_$platform.tar.gz" >>"$dir/checksums.txt"
		fi
		if [ "$2" = tampered ]; then
			printf 'x' >>"$dir/einsora_$platform.tar.gz"
		fi
	done
	echo "file://$dir"
}

# expect runs install.sh with a release and an install directory, checks its outcome, and keeps
# its output in $tmp/out. Failures are recorded in $tmp/failed.
expect() {
	name=$1 outcome=$2 releases=$3 into=$4
	shift 4
	if env EINSORA_RELEASES="$releases" EINSORA_INSTALL_DIR="$into" "$@" sh "$here/install.sh" >"$tmp/out" 2>&1; then got=installs; else got=refuses; fi
	if [ "$got" != "$outcome" ]; then
		fail "$name: install.sh $got, want $outcome" "$(sed 's/^/    /' "$tmp/out")"
	elif [ "$outcome" = refuses ] && [ -e "$into/einsora" ]; then
		fail "$name: refused, but installed einsora anyway"
	else
		echo "ok   $name"
	fi
}

# fail records a failure: its description, then any output that explains it.
fail() {
	printf 'FAIL %s\n' "$1"
	shift
	[ $# -eq 0 ] || printf '%s\n' "$@"
	echo x >>"$tmp/failed"
}

good=$(release good ok)
expect "installs a checked release" installs "$good" "$tmp/bin"
grep -q "einsora 9.9.9 (test build)" "$tmp/out" || fail "the install didn't run einsora version"
grep -q "Next: run 'einsora up'" "$tmp/out" || fail "the install didn't say what's next"
[ -x "$tmp/bin/einsora" ] || fail "einsora isn't executable"

expect "refuses an archive that doesn't match its checksum" refuses "$(release tampered tampered)" "$tmp/tampered"
expect "refuses an archive the checksums don't list" refuses "$(release unlisted unlisted)" "$tmp/unlisted"
expect "refuses einsora as a symlink" refuses "$(release symlink symlink)" "$tmp/symlink"
expect "refuses a version that isn't a release" refuses "$good" "$tmp/version" EINSORA_VERSION='v1;rm -rf /'
expect "refuses a relative install directory" refuses "$good" "relative/bin"
expect "refuses plain http" refuses "http://127.0.0.1:9/releases" "$tmp/http"

if [ -e "$tmp/failed" ]; then
	echo "$(wc -l <"$tmp/failed" | tr -d ' ') failed"
	exit 1
fi
echo "install.sh: all checks passed"
