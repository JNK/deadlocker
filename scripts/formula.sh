#!/bin/bash
# Point the Homebrew formula at a published tag.
#
# The formula builds from GitHub's source tarball for the tag, so what it needs
# is that tarball's checksum — and the checksum can only be taken after the tag
# exists on GitHub. Run it after `make release`, which does it for you.
#
# A formula rather than a cask on the .pkg: Homebrew installs packages with
# `sudo installer -target /`, and the .pkg is a per-user install that needs no
# administrator password. The .pkg is still published for double-clicking.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FORMULA="$REPO_ROOT/Formula/deadlocker.rb"

step() { printf '\n==> %s\n' "$1"; }
fail() { printf 'error: %s\n' "$1" >&2; exit 1; }

TAG="${1:-${TAG:-$(git -C "$REPO_ROOT" describe --tags --abbrev=0 2>/dev/null)}}"
[ -n "$TAG" ] || fail "no tag to point at. Tag a release first, or pass one: scripts/formula.sh v0.1.0"

# The formula's version field carries no v; the URL does.
version="${TAG#v}"
url="https://github.com/JNK/deadlocker/archive/refs/tags/$TAG.tar.gz"

step "Fetching $url"
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
curl -fsSL "$url" -o "$tmp" || fail "could not download $url — is the tag pushed?"
sha="$(shasum -a 256 "$tmp" | cut -d' ' -f1)"

[ -f "$FORMULA" ] || fail "$FORMULA is missing"

out="$(mktemp)"
sed -e "s|^  url \".*\"$|  url \"$url\"|" \
    -e "s|^  version \".*\"$|  version \"$version\"|" \
    -e "s|^  sha256 \".*\"$|  sha256 \"$sha\"|" \
    "$FORMULA" >"$out"
mv "$out" "$FORMULA"

printf '\nformula updated\n  tag      %s\n  version  %s\n  sha256   %s\n' "$TAG" "$version" "$sha"
