#!/bin/bash
# Regenerate the Homebrew cask from the installer that was actually built.
#
# The version, the file name and the checksum all come from the package in
# dist/, because a cask is a promise about a specific file: get the checksum
# wrong and every `brew install` fails with a mismatch, which is a worse failure
# than not shipping a cask at all. Nothing here is typed by hand.
#
# Run after `make pkg`, and again after notarizing — stapling the ticket
# rewrites the package, so its checksum changes.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="$REPO_ROOT/dist"
CASK="$REPO_ROOT/Casks/deadlocker.rb"

fail() { printf 'error: %s\n' "$1" >&2; exit 1; }

PKG="${PKG:-$(ls -t "$DIST"/Deadlocker-*.pkg 2>/dev/null | head -1)}"
[ -n "$PKG" ] && [ -f "$PKG" ] || fail "no installer in dist/. Build one first: make pkg"

base="$(basename "$PKG")"
version="${base#Deadlocker-}"
version="${version%.pkg}"
# The tag carries the leading v; the cask's version field should not repeat it.
bare="${version#v}"
sha="$(shasum -a 256 "$PKG" | cut -d' ' -f1)"

[ -f "$CASK" ] || fail "$CASK is missing"

# Only the two lines that describe the artefact are rewritten; everything else
# in the cask is prose and stanzas that a checksum has no business touching.
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
sed -e "s|^  version \".*\"$|  version \"$bare\"|" \
    -e "s|^  sha256 .*$|  sha256 \"$sha\"|" \
    "$CASK" >"$tmp"
mv "$tmp" "$CASK"

printf 'cask updated\n  package  %s\n  version  %s\n  sha256   %s\n' "$base" "$bare" "$sha"

if ! xcrun stapler validate "$PKG" >/dev/null 2>&1; then
    cat <<'EOF'

warning: this package has no stapled notarization ticket. Stapling changes the
file, so run this again afterwards or the checksum will be wrong.
EOF
fi
