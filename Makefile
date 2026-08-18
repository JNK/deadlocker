# Makefile for deadlocker
#
# `make pkg` is the one that matters: one command, and a signed installer lands
# in dist/. See INSTALLER.md for the certificates it wants and what it does
# without them.

# Default installation prefix — the same place the installer writes to.
PREFIX ?= $(HOME)/.local

help:
	@echo "Usage: make <target>"
	@echo ""
	@echo "Build targets:"
	@echo "  build              Build deadlocker into bin/"
	@echo ""
	@echo "Installation targets:"
	@echo "  install            Build and install deadlocker to $(PREFIX)/bin"
	@echo "  uninstall          Remove deadlocker from $(PREFIX)/bin"
	@echo ""
	@echo "Packaging:"
	@echo "  pkg                Build a signed, notarizable installer into dist/"
	@echo "                     (NOTARY_PROFILE=<notarytool profile> also notarizes and staples)"
	@echo "  release            Publish the newest dist/ installer as a GitHub release"
	@echo "  cask               Point the Homebrew cask at the newest dist/ installer"
	@echo ""
	@echo "Checks:"
	@echo "  test               Go tests and the browser-free JS suites"
	@echo "  verify             Run every scenario against a real MySQL (needs Docker)"
	@echo ""
	@echo "Cleanup targets:"
	@echo "  clean              Remove bin/ and dist/"
	@echo ""
	@echo "Options:"
	@echo "  PREFIX             Installation prefix (default: ~/.local)"
	@echo "  VERSION            Override the version stamped into the package"

# -trimpath because a plain `go build` records the absolute path of every source
# file in the binary, which on this machine means a few hundred copies of the
# home directory, printed at anyone who ever sees a stack trace. The installer
# builds with it too.
GOFLAGS_BUILD := -trimpath

build:
	@echo "Building deadlocker..."
	go build $(GOFLAGS_BUILD) -o bin/deadlocker ./cmd/deadlocker

install: build
	@echo "Installing deadlocker to $(PREFIX)/bin..."
	mkdir -p $(PREFIX)/bin
	install -m 755 bin/deadlocker $(PREFIX)/bin/deadlocker

uninstall:
	@echo "Uninstalling deadlocker from $(PREFIX)/bin..."
	rm -f $(PREFIX)/bin/deadlocker

# Everything that does not need Docker. The scenario library is checked by
# `make verify`, which does.
test:
	go vet ./...
	go test ./...
	node hack/yaml_test.js
	node hack/palette_test.js
	node hack/deadlock_test.js
	node hack/library_test.js
	node hack/css_test.js

# The scenario library is a set of claims about MySQL; this is what checks them.
verify:
	go run ./cmd/deadlocker run -seed -cases $(or $(CASES),cases)

# Universal binary, Developer ID signature, per-user .pkg; notarizes and staples
# when NOTARY_PROFILE is set.
pkg:
	@chmod +x scripts/pkg.sh
	@./scripts/pkg.sh

release:
	@chmod +x scripts/release.sh
	@./scripts/release.sh

# The cask points at one exact file, so it is generated from that file rather
# than edited. `make release` does this for you; this is for regenerating after
# a re-staple.
cask:
	@chmod +x scripts/cask.sh
	@./scripts/cask.sh

clean:
	@echo "Cleaning up..."
	rm -rf bin dist

.PHONY: help build install uninstall test verify pkg release cask clean
