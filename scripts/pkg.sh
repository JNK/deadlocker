#!/bin/bash
# Build a distributable deadlocker installer: universal binary -> Developer ID
# signature with hardened runtime -> per-user .pkg -> notarization -> stapled
# ticket.
#
# The installer writes to ~/.local/bin, the same place as `make install`, and
# needs no administrator password: the distribution enables only the
# current-user-home domain.
#
# Two certificates are involved, and they are NOT interchangeable:
#   Developer ID Application - signs the binary
#   Developer ID Installer   - signs the .pkg
# Create whichever is missing in Xcode (Settings > Accounts > Manage
# Certificates > +) or at developer.apple.com.
#
# Notarization credentials are read from a notarytool keychain profile, created
# once with an app-specific password from appleid.apple.com:
#
#   xcrun notarytool store-credentials jnk-deadlocker \
#     --apple-id <apple-id> --team-id <team-id> --password <app-specific-password>
#
# NOTARY_PROFILE= (empty) stops after signing, for a build that never leaves
# this machine and should not wait on Apple.
#
# Anything missing degrades gracefully: the script builds as far as it can and
# prints the commands needed to finish.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PACKAGING="$REPO_ROOT/packaging"
DIST="$REPO_ROOT/dist"

APP_IDENTITY="${APP_IDENTITY:-Developer ID Application}"
INSTALLER_IDENTITY="${INSTALLER_IDENTITY:-Developer ID Installer}"
NOTARY_PROFILE="${NOTARY_PROFILE-jnk-deadlocker}"
IDENTIFIER="${IDENTIFIER:-io.jnk.deadlocker}"

# A tag when there is one, otherwise the build's own timestamp — this tree is
# not always a checkout, and two builds on one day still want two filenames.
VERSION="${VERSION:-$(git -C "$REPO_ROOT" describe --tags --always 2>/dev/null || date -u +%Y.%m.%d.%H%M)}"

# The build takes the working tree, but the version claims a tag — so an
# installer built with uncommitted changes is stamped with a version whose
# source does not contain them. Anyone who later builds that tag gets something
# different from what was published, and nothing says so.
if [ -n "$(git -C "$REPO_ROOT" status --porcelain 2>/dev/null)" ]; then
    if [ "${ALLOW_DIRTY:-}" = "1" ]; then
        printf 'warning: building %s from a dirty working tree because ALLOW_DIRTY=1\n' "$VERSION" >&2
    else
        cat >&2 <<EOF
error: the working tree has uncommitted changes.

This would package them under version $VERSION, whose tag does not contain
them. Commit or stash first:

$(git -C "$REPO_ROOT" status --short | sed 's/^/  /')

To build a throwaway package anyway, set ALLOW_DIRTY=1.
EOF
        exit 1
    fi
fi

# A tag may legally contain a slash; a filename and a pkg version string may not.
VERSION_SAFE="${VERSION//\//-}"
PKG="$DIST/Deadlocker-$VERSION_SAFE.pkg"

# One entry per installed binary, and the single source of truth for what the
# installer offers — the choice pane in distribution.xml is generated from this.
# Fields: <binary name>|<package to build>|<title>|<description>
TOOLS=(
    "deadlocker|./cmd/deadlocker|Deadlocker|Provoke and read MySQL locks, step by step"
)

step() { printf '\n==> %s\n' "$1"; }
fail() { printf 'error: %s\n' "$1" >&2; exit 1; }

# `find-identity -v` filters by the codesigning policy, which installer
# certificates are not part of — it never lists them, valid or not. The
# unfiltered list is the only one that sees both kinds.
has_identity() { security find-identity 2>/dev/null | grep -q "$1"; }

[ "$(uname -s)" = "Darwin" ] || fail "installers can only be built on macOS"
command -v lipo >/dev/null || fail "lipo not found (install the Xcode command line tools)"
has_identity "$APP_IDENTITY" || fail "no signing identity matching '$APP_IDENTITY'. List them with: security find-identity -v"

# Checked here rather than at the end, so a profile that is missing or expired
# is known before a minute of building rather than after it. notarytool keeps
# its credentials in the data-protection keychain, which `security` cannot
# search, so asking notarytool itself is the only way — it costs one round trip
# to Apple. A failure downgrades the run to signed-but-not-notarized instead of
# throwing the build away; the closing message says how to finish by hand.
if [ -n "$NOTARY_PROFILE" ]; then
    step "Checking the notarization profile"
    if xcrun notarytool history --keychain-profile "$NOTARY_PROFILE" >/dev/null 2>&1; then
        echo "$NOTARY_PROFILE: ok"
    else
        cat >&2 <<EOF
warning: notarytool profile "$NOTARY_PROFILE" is not usable — building without
notarization. Create it once with an app-specific password from
appleid.apple.com:

  xcrun notarytool store-credentials $NOTARY_PROFILE \\
    --apple-id <apple-id> --team-id <team-id> --password <app-specific-password>
EOF
        NOTARY_PROFILE=""
    fi
fi

stage="$(mktemp -d)"
build="$(mktemp -d)"
trap 'rm -rf "$stage" "$build"' EXIT

mkdir -p "$stage/flat"
: >"$stage/outline.xml"
: >"$stage/choices.xml"

# Every tool gets its own component package so the choice pane can include or
# exclude it independently. Each payload is laid out relative to the home
# directory, which is what the current-user-home domain in distribution.xml
# maps onto.
for tool in "${TOOLS[@]}"; do
    IFS='|' read -r name pkgpath title desc <<<"$tool"

    step "Building $name ($VERSION)"
    # CGO off keeps this a static binary that runs on any macOS of the same
    # architecture; the scenario library and the whole UI are embedded, so the
    # installed file is the entire application.
    for arch in arm64 amd64; do
        ( cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" \
            go build -trimpath -o "$build/$name-$arch" "$pkgpath" )
    done
    lipo -create -output "$build/$name" "$build/$name-arm64" "$build/$name-amd64"
    lipo -info "$build/$name"

    # Hardened runtime and a secure timestamp are both required for notarization.
    codesign --force --options runtime --timestamp --sign "$APP_IDENTITY" "$build/$name"
    codesign --verify --strict --verbose=2 "$build/$name"

    root="$stage/root/$name"
    mkdir -p "$root/.local/bin"
    install -m 755 "$build/$name" "$root/.local/bin/$name"

    pkgbuild \
        --root "$root" \
        --identifier "$IDENTIFIER.$name" \
        --version "$VERSION_SAFE" \
        --install-location "/" \
        "$stage/flat/$name-component.pkg"

    printf '        <line choice="%s"/>\n' "$name" >>"$stage/outline.xml"
    cat >>"$stage/choices.xml" <<EOF
    <choice id="$name" title="$title" description="$desc" start_selected="true">
        <pkg-ref id="$IDENTIFIER.$name"/>
    </choice>
    <pkg-ref id="$IDENTIFIER.$name" version="$VERSION_SAFE">$name-component.pkg</pkg-ref>
EOF
done

step "Staged payload"
find "$stage/root" -mindepth 1 -type f

# `sed -e '/x/r file' -e '/x/d'` is the portable read-then-delete idiom: it
# splices a multi-line block in where a single-line placeholder was. The
# patterns are anchored so that the template's own comment can name a
# placeholder without having the block spliced into the middle of it.
step "Composing distribution"
sed -e "s|@VERSION@|$VERSION_SAFE|g" \
    -e "/^@OUTLINE@$/r $stage/outline.xml" -e "/^@OUTLINE@$/d" \
    -e "/^@CHOICES@$/r $stage/choices.xml" -e "/^@CHOICES@$/d" \
    "$PACKAGING/distribution.xml" >"$stage/distribution.xml"
grep -q '@OUTLINE@\|@CHOICES@' "$stage/distribution.xml" && fail "placeholder left unexpanded in distribution.xml"
xmllint --noout "$stage/distribution.xml" || fail "generated distribution.xml is not well-formed"

mkdir -p "$DIST"
rm -f "$PKG"

step "Building installer"
if has_identity "$INSTALLER_IDENTITY"; then
    productbuild \
        --distribution "$stage/distribution.xml" \
        --package-path "$stage/flat" \
        --resources "$PACKAGING/resources" \
        --sign "$INSTALLER_IDENTITY" \
        --timestamp \
        "$PKG"
    pkgutil --check-signature "$PKG" | head -4
else
    productbuild \
        --distribution "$stage/distribution.xml" \
        --package-path "$stage/flat" \
        --resources "$PACKAGING/resources" \
        "$PKG"
    step "Unsigned installer"
    cat <<EOF
$PKG

No '$INSTALLER_IDENTITY' certificate is installed, so the package could not be
signed and cannot be notarized. A Developer ID Application certificate does not
work here; the installer needs its own certificate.

Create one, then re-run:
  Xcode > Settings > Accounts > Manage Certificates > + > Developer ID Installer
  (or developer.apple.com > Certificates, Identifiers & Profiles > Certificates)
EOF
    exit 0
fi

if [ -z "$NOTARY_PROFILE" ]; then
    step "Signed, not notarized"
    cat <<EOF
$PKG

The installer was not submitted to Apple. Until it is notarized and stapled,
Gatekeeper will warn on machines that download it.

To finish:
  xcrun notarytool submit "$PKG" --keychain-profile jnk-deadlocker --wait
  xcrun stapler staple "$PKG"
EOF
    exit 0
fi

step "Notarizing (this waits for Apple)"
xcrun notarytool submit "$PKG" --keychain-profile "$NOTARY_PROFILE" --wait

step "Stapling the ticket"
xcrun stapler staple "$PKG"
xcrun stapler validate "$PKG"

# Assess exactly as a downloading Mac would. Installers use the install policy.
step "Verifying Gatekeeper acceptance"
spctl --assess --type install -v "$PKG"

step "Done"
echo "$PKG"
