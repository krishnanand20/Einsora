#!/bin/sh
# Installs einsora from its GitHub release (ADR-0023), on an Apple Silicon Mac or 64-bit Linux:
#
#   curl -fsSL https://einsora.com/install.sh | sh
#
# It downloads the release's archive for this machine with the release's checksums, checks the
# archive's SHA-256 against them and, if cosign is installed, the checksums' signature (made by the
# release workflow, with no key that could leak), then installs the einsora binary. Nothing else
# changes: no service, no account, no settings. Optional settings:
#
#   EINSORA_VERSION      a release, such as v0.1.0 (default: the latest)
#   EINSORA_INSTALL_DIR  where einsora goes (default: /usr/local/bin, with sudo if you can't write there)
#   EINSORA_RELEASES     a URL serving a release's files, such as a mirror (default: GitHub)
set -eu

repo="krishnanand20/Einsora"
# Who signs releases: the release workflow, for a version tag, through GitHub's OIDC issuer.
signer="^https://github\\.com/${repo}/\\.github/workflows/release\\.yml@refs/tags/v[0-9][0-9A-Za-z.+-]*\$"
issuer="https://token.actions.githubusercontent.com"

say() { printf '%s\n' "$*"; }
fail() {
	printf 'einsora install: %s\n' "$*" >&2
	exit 1
}

# Everything runs from main, called on the last line: a download cut short runs nothing.
main() {
	platform=$(detect_platform)
	version=${EINSORA_VERSION:-latest}
	if [ "$version" != latest ] && ! printf '%s' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
		fail "EINSORA_VERSION must be a release such as v0.1.0, not '$version'"
	fi
	if [ -n "${EINSORA_RELEASES:-}" ]; then
		base=${EINSORA_RELEASES%/}
	elif [ "$version" = latest ]; then
		base="https://github.com/${repo}/releases/latest/download"
	else
		base="https://github.com/${repo}/releases/download/${version}"
	fi
	archive="einsora_${platform}.tar.gz"

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 1' INT TERM
	say "Downloading einsora ($version, $platform) from $base"
	download "$base/$archive" "$tmp/$archive"
	download "$base/checksums.txt" "$tmp/checksums.txt"

	verify_signature "$base" "$tmp"
	expected=$(awk -v name="$archive" '$2 == name { print $1 }' "$tmp/checksums.txt")
	[ -n "$expected" ] || fail "the release's checksums don't list $archive"
	actual=$(sha256 "$tmp/$archive")
	[ "$expected" = "$actual" ] || fail "$archive has SHA-256 $actual, but the release lists $expected: not installing it"
	say "Checked $archive against the release's SHA-256."

	mkdir "$tmp/unpacked"
	tar -xzf "$tmp/$archive" -C "$tmp/unpacked" einsora || fail "$archive has no einsora in it"
	binary="$tmp/unpacked/einsora"
	if [ -L "$binary" ] || [ ! -f "$binary" ]; then
		fail "einsora in $archive isn't a regular file"
	fi

	dir=${EINSORA_INSTALL_DIR:-/usr/local/bin}
	case "$dir" in
	/*) ;;
	*) fail "EINSORA_INSTALL_DIR must be an absolute path, not '$dir'" ;;
	esac
	install_binary "$binary" "$dir"
	say ""
	"$dir/einsora" version || fail "installed $dir/einsora, but it doesn't run on this machine"
	case ":$PATH:" in
	*":$dir:"*) ;;
	*) say "Note: $dir isn't on your PATH; add it, or run $dir/einsora." ;;
	esac
	say ""
	say "Next: run 'einsora up' to start Einsora on this machine and create its owner account."
	say "Then 'einsora service install' keeps it running in the background, starting at login."
	say "Guide: https://einsora.com/runbooks/00-quickstart/"
}

detect_platform() {
	os=$(uname -s)
	arch=$(uname -m)
	case "$os" in
	Darwin)
		# A shell running under Rosetta reports x86_64 on an Apple Silicon Mac.
		if [ "$arch" = x86_64 ] && [ "$(sysctl -n hw.optional.arm64 2>/dev/null || true)" = 1 ]; then
			arch=arm64
		fi
		[ "$arch" = arm64 ] || fail "Einsora needs an Apple Silicon Mac (M1 or newer); this one is $arch"
		echo darwin_arm64
		;;
	Linux)
		case "$arch" in
		x86_64 | amd64) echo linux_amd64 ;;
		aarch64 | arm64) echo linux_arm64 ;;
		*) fail "Einsora runs on 64-bit x86 and ARM Linux, not $arch" ;;
		esac
		;;
	*) fail "Einsora runs on macOS and Linux, not $os" ;;
	esac
}

# download fetches a URL to a file, over HTTPS only, redirects included (or a file: URL, for a local
# mirror). curl only: wget can't keep a redirect from leaving HTTPS, and the checksums travel the
# same way as the archive.
download() {
	command -v curl >/dev/null 2>&1 || fail "install curl first"
	curl -fsSL --proto '=https,file' --proto-redir '=https' --tlsv1.2 -o "$2" "$1" || fail "couldn't download $1"
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{ print $1 }'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{ print $1 }'
	else
		fail "install sha256sum or shasum first: the download must be checked"
	fi
}

# verify_signature checks the checksums' Sigstore signature when cosign is installed. Without it,
# the SHA-256 still has to match, which proves the archive is the release's, as long as GitHub
# serves the release unaltered; cosign proves the release workflow made it.
verify_signature() {
	if ! command -v cosign >/dev/null 2>&1; then
		say "cosign isn't installed, so the release's signature isn't checked (https://docs.sigstore.dev); its SHA-256 is."
		return
	fi
	download "$1/checksums.txt.sigstore.json" "$2/checksums.txt.sigstore.json"
	if ! cosign verify-blob --bundle "$2/checksums.txt.sigstore.json" \
		--certificate-identity-regexp "$signer" --certificate-oidc-issuer "$issuer" \
		"$2/checksums.txt" >"$2/cosign.log" 2>&1; then
		sed 's/^/  cosign: /' "$2/cosign.log" >&2
		fail "cosign couldn't verify that the release workflow signed these checksums: not installing (an old cosign can't read this signature: cosign 2.4 or later can)"
	fi
	say "Checked the release's signature with cosign."
}

install_binary() {
	if { [ -d "$2" ] && [ -w "$2" ]; } || { [ ! -e "$2" ] && mkdir -p "$2" 2>/dev/null; }; then
		install -m 0755 "$1" "$2/einsora"
	elif [ -z "${EINSORA_INSTALL_DIR:-}" ] && command -v sudo >/dev/null 2>&1; then
		say "Installing to $2 with sudo:"
		sudo mkdir -p "$2"
		sudo install -m 0755 "$1" "$2/einsora"
	else
		fail "can't write to $2: set EINSORA_INSTALL_DIR to a directory you can write to, such as \$HOME/.local/bin"
	fi
	say "Installed $2/einsora."
}

main "$@"
