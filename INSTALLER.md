# Building the installer

`make pkg` produces a signed, notarized macOS installer in `dist/`. One command,
and the result is a `.pkg` anyone can double-click.

```sh
make pkg          # sign, notarize, staple
make release      # publish it as a GitHub release, and point the cask at it
```

`make release` regenerates `Casks/deadlocker.rb` from the package it published,
so the Homebrew cask always describes a file that exists with the checksum it
actually has. Commit that file afterwards — the tap is this repository:

```sh
brew tap jnk/deadlocker https://github.com/JNK/deadlocker
brew install --cask deadlocker
```

Stapling a notarization ticket rewrites the package, so its checksum changes.
If you staple by hand after the fact, run `make cask` again.

`dist/` is not in the repository. A 15MB binary has no business in git history,
so a built installer becomes downloadable by being attached to a release.

## What it produces

A universal binary — Apple Silicon and Intel in one file — signed with a
Developer ID Application certificate and the hardened runtime, wrapped in a
per-user `.pkg` signed with a Developer ID Installer certificate, notarized by
Apple and stapled.

It installs one file, `~/.local/bin/deadlocker`, which is the whole application:
the web interface, the scenario library and the MySQL wire-protocol decoder are
all embedded in the binary. Installation needs no administrator password,
because the distribution enables only the current-user-home domain — the
installer cannot be pointed at `/usr/local` or another volume, so there is a
single install location and it matches `make install`.

## What it needs

**Xcode command line tools**, for `lipo`, `codesign`, `pkgbuild` and
`productbuild`.

**Two certificates, which are not interchangeable:**

| Certificate | Signs |
|---|---|
| Developer ID Application | the binary |
| Developer ID Installer | the `.pkg` |

Create whichever is missing in Xcode (Settings → Accounts → Manage
Certificates → +) or at developer.apple.com.

**A notarytool keychain profile**, created once with an app-specific password
from appleid.apple.com:

```sh
xcrun notarytool store-credentials jnk-deadlocker \
  --apple-id <apple-id> --team-id <team-id> --password <app-specific-password>
```

The profile lives in the keychain; nothing secret is stored in this repository.

## Without them

Everything degrades rather than failing. The script builds as far as it can and
prints the commands needed to finish by hand:

- No **Developer ID Installer** certificate → an unsigned `.pkg`, which cannot
  be notarized. A Developer ID Application certificate does not substitute.
- No usable **notary profile** → a signed but un-notarized `.pkg`. Gatekeeper
  will warn on any machine that downloads it. The profile is checked *before*
  the build rather than after, so a missing one costs a second rather than a
  minute.
- No **Developer ID Application** certificate → this is the one hard stop.
  There is nothing worth producing without it.

`make release` refuses to publish an installer that is unsigned or has no
stapled ticket, because the cost of that mistake is paid by everyone who
downloads it. `ALLOW_UNNOTARIZED=1` overrides it for a pre-release nobody will
download.

## Options

| Variable | Default | Meaning |
|---|---|---|
| `VERSION` | `git describe --tags --always` | Stamped into the filename and the package |
| `NOTARY_PROFILE` | `jnk-deadlocker` | Set it empty to stop after signing |
| `APP_IDENTITY` | `Developer ID Application` | Binary signing identity |
| `INSTALLER_IDENTITY` | `Developer ID Installer` | Package signing identity |
| `IDENTIFIER` | `io.jnk.deadlocker` | Package identifier prefix |
| `TAG` | `v<version>` | Release tag for `make release` |
| `PKG` | newest in `dist/` | Which installer `make release` publishes |

## Adding another binary

`scripts/pkg.sh` has a `TOOLS` array, and it is the single source of truth: the
choice pane in `packaging/distribution.xml` is generated from it. One line adds
a tool, and nothing else needs editing.

```
TOOLS=(
    "deadlocker|./cmd/deadlocker|Deadlocker|Provoke and read MySQL locks, step by step"
)
```
