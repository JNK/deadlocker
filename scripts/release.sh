#!/bin/bash
# Publish the newest installer in dist/ as a GitHub release.
#
# dist/ is not in the repository — a 15MB binary has no business in git history
# — so the release is where a built installer becomes something anyone can
# download. This uploads what `make pkg` produced; it never builds, so what is
# published is exactly the file that was signed and notarized.
#
# The tag comes from the package's own name, which came from `git describe`, so
# a release always corresponds to a tag that already existed at build time.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="$REPO_ROOT/dist"

step() { printf '\n==> %s\n' "$1"; }
fail() { printf 'error: %s\n' "$1" >&2; exit 1; }

command -v gh >/dev/null || fail "gh not found. Install it with: brew install gh"
gh auth status >/dev/null 2>&1 || fail "gh is not logged in. Run: gh auth login"

# Newest by modification time: the one just built.
PKG="${PKG:-$(ls -t "$DIST"/Deadlocker-*.pkg 2>/dev/null | head -1)}"
[ -n "$PKG" ] && [ -f "$PKG" ] || fail "no installer in dist/. Build one first: make pkg"

base="$(basename "$PKG")"
version="${base#Deadlocker-}"
version="${version%.pkg}"
# The version comes from `git describe`, so with the usual tagging convention it
# already starts with a v. Prefixing unconditionally produced "vv0.1.0" and a
# release under a tag nobody had ever created.
case "$version" in
    v*) TAG="${TAG:-$version}" ;;
    *)  TAG="${TAG:-v$version}" ;;
esac

step "Checking the installer"
echo "$PKG"

# Publishing an unsigned or un-notarized installer is worse than publishing
# nothing: everyone who downloads it meets a Gatekeeper warning, and the fix is
# to re-release. Checked here rather than trusted.
if ! pkgutil --check-signature "$PKG" >/dev/null 2>&1; then
    fail "this package is not signed. Build it on a machine with a Developer ID Installer certificate."
fi
pkgutil --check-signature "$PKG" | head -3

if ! xcrun stapler validate "$PKG" >/dev/null 2>&1; then
    cat >&2 <<EOF

error: this package has no stapled notarization ticket.

Anyone downloading it will be told it cannot be checked for malicious software.
Finish notarizing it first:

  xcrun notarytool submit "$PKG" --keychain-profile jnk-deadlocker --wait
  xcrun stapler staple "$PKG"

Then run this again. To publish anyway — for a pre-release nobody will download
— set ALLOW_UNNOTARIZED=1.
EOF
    [ "${ALLOW_UNNOTARIZED:-}" = "1" ] || exit 1
    printf '\nwarning: publishing without a notarization ticket because ALLOW_UNNOTARIZED=1\n'
fi

step "Publishing $TAG"
if gh release view "$TAG" >/dev/null 2>&1; then
    # Re-uploading the same asset is the normal way to correct a bad build, so
    # it is allowed rather than refused.
    echo "release $TAG already exists; uploading the installer to it"
    gh release upload "$TAG" "$PKG" --clobber
else
    notes="$(mktemp)"
    trap 'rm -f "$notes"' EXIT
    cat >"$notes" <<EOF
Signed, notarized installer for macOS (universal — Apple Silicon and Intel).

Installs \`deadlocker\` into \`~/.local/bin\`. No administrator password is
needed, and nothing is written outside your home directory.

Deadlocker starts its own MySQL containers, so **Docker must be running**.

\`\`\`
deadlocker            # serves the UI on http://127.0.0.1:8899
deadlocker run        # runs every scenario headless, non-zero on a mismatch
deadlocker help       # everything else
\`\`\`
EOF
    gh release create "$TAG" "$PKG" \
        --title "Deadlocker $version" \
        --notes-file "$notes"
fi

# The formula builds from the tag's source tarball, whose checksum can only be
# taken once the tag is on GitHub — which it now is.
step "Updating the Homebrew formula"
"$REPO_ROOT/scripts/formula.sh" "$TAG"
cat <<EOF

The formula now points at $TAG. Commit it so \`brew install\` finds it:

  git add Formula/deadlocker.rb && git commit -m "Point the formula at $TAG" && git push

EOF

step "Done"
gh release view "$TAG" --json url --jq .url
