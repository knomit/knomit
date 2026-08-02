.PHONY: build backup-agent web desktop-ui test test-desktop clean run dev setup dist docker docker-amd64 desktop desktop-deps desktop-app-macos desktop-icons desktop-install desktop-run download-ort tokenizers-lib e2e e2e-ui e2e-setup e2e-report release release-server release-desktop desktop-notarize print-version print-semver

# All build artifacts are written under a per-platform directory,
# dist/<goos>-<goarch> (e.g. dist/darwin-arm64, dist/linux-arm64), so builds for
# different platforms — e.g. a native macOS build alongside a Linux build done
# in a container/VM — coexist without clobbering each other's binaries or native
# libs. Wails (CGO + the OS-native webview toolkit) cannot cross-compile, so each
# platform is built in its own native environment; this layout keeps the outputs
# separate. For consumers that need a stable, platform-independent path (e.g.
# .mcp.json, the e2e harness), `build`/`desktop` also drop top-level symlinks
# dist/<tool> -> <platform>/<tool>.
GOOS    := $(shell go env GOOS)
GOARCH  := $(shell go env GOARCH)
PLATFORM := $(GOOS)-$(GOARCH)
DIST    := dist/$(PLATFORM)
LIBDIR  := $(DIST)/lib

# Build version. BASE_VERSION is the Major.Minor.Patch semver of the last
# RELEASE and is the single source of truth — bump it here on release.
# GIT_COMMIT is the short SHA of the build. Both are injected into the
# internal/version package via -ldflags, so every binary (knomit,
# knomit-bridge, knomit-okf, knomit-backup, knomit-desktop) reports e.g.
# 0.5.0.2a7ae9d.
# A bare `go build` (no make) falls back to the package default "dev".
BASE_VERSION := 0.5.1
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
# BUILD_VERSION is the macOS CFBundleVersion, which macOS/LaunchServices use to
# order builds for upgrade detection — so it MUST increase monotonically across
# releases. Apple also requires only digits 0-9 and periods (≤3 integer
# components). We use the HEAD commit's committer epoch seconds: a single legal
# integer (~1.78e9, well under 2^32 until ~2106) that grows with every commit,
# is deterministic per commit, and survives shallow CI clones (unlike a commit
# count). Commit IDENTITY is NOT encoded here — it lives in GIT_COMMIT (the SHA
# in internal/version), while CFBundleShortVersionString carries the display
# version, $(VERSION), which differs per RELEASE_CHANNEL below.
# Falls back to 0 outside a git checkout.
BUILD_VERSION := $(shell git show -s --format=%ct HEAD 2>/dev/null || echo 0)

# RELEASE_CHANNEL selects what VERSION means, and nothing else in this file
# branches on it.
#
#   stable (default)  VERSION = BASE_VERSION, e.g. 0.5.1. Local builds, the
#                     stable release workflow, and the tag check in
#                     release-stable.yml all take this path — print-semver has
#                     to keep printing the bare BASE_VERSION or that check
#                     fails on every tag.
#   dev               VERSION = <next patch>-dev.<BUILD_VERSION>, e.g.
#                     0.5.2-dev.1785282494. Set by release.yml.
#
# WHY the next patch and not BASE_VERSION: semver orders a prerelease BELOW its
# own release, so 0.5.1-dev.N < 0.5.1 — a dev build cut after v0.5.1 shipped
# would sort behind the release it was built from. Bumping the patch first puts
# dev builds where they actually belong, between 0.5.1 and 0.5.2. If the next
# release turns out to be 0.6.0 rather than 0.5.2 the ordering still holds:
# 0.5.1 < 0.5.2-dev.N < 0.6.0.
#
# WHY BUILD_VERSION as the prerelease counter: it is already the monotonic,
# deterministic, shallow-clone-safe number this file uses for CFBundleVersion,
# and semver compares dot-separated numeric prerelease identifiers NUMERICALLY,
# so 0.5.2-dev.1785299001 > 0.5.2-dev.1785282494. Reusing it keeps one notion
# of "which build is newer" rather than two that can disagree. `dev-latest` is
# a ROLLING tag, so without this every dev build reported the same version as
# the last stable release and was indistinguishable from it.
#
# This is deliberately NOT wired to self-update: dev builds still ship with an
# empty UPDATE_PUBLIC_KEY (see below), which is what disables the updater in
# them. The versions are ordered so a dev channel COULD exist later, not
# because one does.
#
# awk -F'[.]' rather than -F. because a bare `.` is a regex to some awks, and
# brackets rather than parens because Make closes $(shell at the first bare
# `)` regardless of quoting.
RELEASE_CHANNEL ?= stable
ifeq ($(RELEASE_CHANNEL),dev)
  VERSION := $(shell echo $(BASE_VERSION) | awk -F'[.]' '{print $$1"."$$2"."$$3+1}')-dev.$(BUILD_VERSION)
else
  VERSION := $(BASE_VERSION)
endif

# FULL_VERSION is the semver plus the short SHA (e.g. 0.5.0.2a7ae9d) — the same
# string the binaries report via internal/version. Used as the Docker image tag.
FULL_VERSION := $(VERSION).$(GIT_COMMIT)
VERSION_PKG := knomit/internal/version
# UPDATE_PUBLIC_KEY is the base64 Ed25519 key that authenticates desktop update
# artifacts. Empty for local builds and for the dev release, which is what
# disables self-update in those binaries — see tools/desktop/update.go. The
# stable-release workflow supplies it from a repo VARIABLE (not a secret:
# Actions' secret masking would corrupt the value baked into the binary).
UPDATE_PUBLIC_KEY ?=
VERSION_LDFLAGS := -X $(VERSION_PKG).Version=$(VERSION) -X $(VERSION_PKG).Commit=$(GIT_COMMIT) -X $(VERSION_PKG).UpdatePublicKey=$(UPDATE_PUBLIC_KEY)

# ---- Apple code signing and notarization (macOS only) ----------------------
#
# CODESIGN_IDENTITY selects between two signing modes, applied to BOTH macOS
# release artifacts — the .app bundle and the loose binaries in the server
# tarball:
#
#   empty (default)  ad-hoc. What every local build and every fork PR gets.
#                    Enough to stop macOS calling a downloaded copy "damaged";
#                    NOT enough for Gatekeeper to open it without a prompt.
#   set              Developer ID. Hardened runtime + secure timestamp, which
#                    are both PREREQUISITES for notarization — Apple rejects a
#                    submission missing either.
#
# e.g. CODESIGN_IDENTITY="Developer ID Application: Knomit LLC (73YBS394G4)"
CODESIGN_IDENTITY ?=

# The flags every codesign call in this file uses, so the two modes cannot
# drift apart between the bundle and the tarball. --options runtime and
# --timestamp are deliberately absent from the ad-hoc form: both exist to
# satisfy notarization, and neither means anything without a real identity.
#
# --options runtime also turns on LIBRARY VALIDATION, which is why every dylib
# we dlopen must carry this same identity. internal/embeddings/embedder.go
# resolves libonnxruntime from <exe>/lib first and we sign that copy, so the
# shipped artifacts are fine; the /opt/homebrew fallback and the ORT_LIB_PATH
# override in that file will NOT load against a Developer ID build unless the
# dylib they point at is signed by the same Team ID. That is a developer-only
# path — no entitlements file is needed, and none exists.
ifeq ($(CODESIGN_IDENTITY),)
  CODESIGN_FLAGS := --force --sign -
else
  CODESIGN_FLAGS := --force --options runtime --timestamp --sign "$(CODESIGN_IDENTITY)"
endif

# The variant for the LOOSE binaries in the server tarball: identity, but no
# hardened runtime. Hardened runtime exists to satisfy notarization, and a
# .tar.gz can never be notarized — notarytool takes .zip/.pkg/.dmg, and
# stapler can only write a ticket into a bundle, so loose binaries have nowhere
# to keep one. Enabling it there would buy nothing and cost something real:
# library validation would break the ORT_LIB_PATH escape hatch that tarball
# users rely on to point knomit at their own onnxruntime build.
ifeq ($(CODESIGN_IDENTITY),)
  CODESIGN_FLAGS_LOOSE := --force --sign -
else
  CODESIGN_FLAGS_LOOSE := --force --timestamp --sign "$(CODESIGN_IDENTITY)"
endif

# Notarization credentials, used by release-desktop. Two forms, matching the
# two `notarytool` supports:
#
#   NOTARY_PROFILE   a keychain profile from `notarytool store-credentials`.
#                    Simplest locally; useless in CI, where there is no
#                    populated login keychain and the Apple ID is 2FA-bound.
#   NOTARY_KEY +     an App Store Connect API key (.p8 path, Key ID, Issuer
#   NOTARY_KEY_ID +  ID). The CI form — no interactive Apple ID involved.
#   NOTARY_ISSUER
#
# With neither set, release-desktop signs but does not notarize, and says so.
# That keeps `make release` working for anyone without Apple credentials.
NOTARY_PROFILE ?=
NOTARY_KEY     ?=
NOTARY_KEY_ID  ?=
NOTARY_ISSUER  ?=

# The auth flags handed to `notarytool`, or empty when notarization is off.
# Profile wins if both are somehow set, because it is the more explicit
# local-developer choice.
ifneq ($(NOTARY_PROFILE),)
  NOTARY_AUTH := --keychain-profile "$(NOTARY_PROFILE)"
else ifneq ($(NOTARY_KEY),)
  NOTARY_AUTH := --key "$(NOTARY_KEY)" --key-id "$(NOTARY_KEY_ID)" --issuer "$(NOTARY_ISSUER)"
else
  NOTARY_AUTH :=
endif

# Native libraries (ONNX Runtime, libtokenizers.a) are fetched by
# the cross-platform Go tool tools/fetchlibs, which is the single source of
# truth for their versions and per-platform asset names. The only platform bit
# Make still needs is the ORT library filename, for the `run` target.
ifeq ($(GOOS),darwin)
  ORT_LIB_NAME := libonnxruntime.dylib
else ifeq ($(GOOS),windows)
  ORT_LIB_NAME := onnxruntime.dll
else
  ORT_LIB_NAME := libonnxruntime.so
endif

# symlink_tool creates/refreshes a stable top-level symlink
# dist/<name> -> <platform>/<name>. The target is relative to dist/ so the link
# stays valid regardless of where the repo lives.
define symlink_tool
	ln -sfn "$(PLATFORM)/$(1)" "dist/$(1)"
endef

setup:
	go run ./tools/fetchlibs $(LIBDIR)
	@echo "Setup complete. Run 'make run' to start the server."

download-ort:
	go run ./tools/fetchlibs -only ort $(LIBDIR)

tokenizers-lib:
	go run ./tools/fetchlibs -only tokenizers $(LIBDIR)

# tokenizers-lib is linked statically at build time; ort is the
# shared libs the built binary dlopens at RUN time, so fetch them too — otherwise
# a fresh `make build && ./dist/<platform>/knomit serve` fails to load them.
build: web tokenizers-lib download-ort
	mkdir -p $(DIST)
	CGO_ENABLED=1 go build $(GOFLAGS) -ldflags "$(VERSION_LDFLAGS)" -o $(DIST)/knomit .
	go build $(GOFLAGS) -ldflags "$(VERSION_LDFLAGS)" -o $(DIST)/knomit-bridge ./tools/bridge/
	go build $(GOFLAGS) -ldflags "$(VERSION_LDFLAGS)" -o $(DIST)/knomit-okf ./tools/okf/
	# knomit-backup runs litestream in a CHILD process. knomit must not link
	# litestream at all — litestream's SQLite build and knomit's cgo one do not
	# see each other's file locks inside one process. knomit finds this binary
	# beside itself at startup, which is why it lands in the same directory.
	go build $(GOFLAGS) -ldflags "$(VERSION_LDFLAGS)" -o $(DIST)/knomit-backup ./tools/backup/
	$(call symlink_tool,knomit)
	$(call symlink_tool,knomit-bridge)
	$(call symlink_tool,knomit-okf)
	$(call symlink_tool,knomit-backup)

web:
	cd web && npm ci && npm run build

# The desktop-only UI (Settings, Logs), embedded by tools/desktop/ui and served
# under /desktop/. Separate from `web` because that bundle also ships inside the
# server binary, where these screens would be meaningless.
desktop-ui:
	cd tools/desktop/ui && npm ci && npm run build

test: tokenizers-lib
	CGO_ENABLED=1 go test $(GOFLAGS) ./...

# The same suite under `-tags desktop`. It is a separate pass because the tag
# changes BEHAVIOUR, not just which files compile: internal/config forces
# backup.enabled off under it (backup_desktop.go), so the guard behind that
# ruling only runs here. Whole tree rather than ./tools/desktop/... — the tag's
# reach is not confined to that directory, and scoping it there is exactly how
# the guard came to be executed by nobody. CI runs this same pattern.
test-desktop: tokenizers-lib
	CGO_ENABLED=1 go test $(GOFLAGS) -tags desktop ./...

dist: download-ort tokenizers-lib build
	@echo "Distribution package ready in $(DIST)/"

# Build the cloud HTTP server as a fully self-contained Docker image (CGO +
# bundled ONNX native libs + embedding model baked at build time;
# the running container performs no startup downloads).
docker:
	docker build -t knomit:$(FULL_VERSION) -t knomit:latest .

# Cross-build the same fully self-contained image for linux/amd64. The Dockerfile
# is multi-arch (fetchlibs pulls per-platform native libs; the runtime COPY globs
# dist/linux-*/lib), so only --platform differs. Tags carry an -amd64 suffix so a
# cross-build on a non-amd64 host does not clobber the native knomit:latest.
# Requires a buildx-capable Docker (Docker Desktop / OrbStack provide this).
docker-amd64:
	docker build --platform linux/amd64 -t knomit:$(FULL_VERSION)-amd64 -t knomit:latest-amd64 .

# backup-agent builds only the replication child, for targets that run knomit
# without producing a full dist tree.
backup-agent:
	mkdir -p $(DIST)
	go build $(GOFLAGS) -ldflags "$(VERSION_LDFLAGS)" -o $(DIST)/knomit-backup ./tools/backup/

CMD ?= serve
# KNOMIT_BACKUP_AGENT is set explicitly because `go run` puts the executable in
# a build-cache temp directory, so knomit's usual "look beside myself" search
# would find nothing and a backup-enabled run would refuse to start.
run: download-ort tokenizers-lib backup-agent
	CGO_ENABLED=1 \
	  ORT_LIB_PATH=$(LIBDIR)/$(ORT_LIB_NAME) \
	  KNOMIT_BACKUP_AGENT=$(abspath $(DIST)/knomit-backup) \
	  go run $(GOFLAGS) . $(CMD)

dev:
	cd web && npm run dev

clean:
	rm -rf dist/ web/dist/

e2e-setup:
	cd e2e && npm ci && npx playwright install

e2e: dist
	cd e2e && npx playwright test

e2e-ui: dist
	cd e2e && npx playwright test --headed

e2e-report:
	cd e2e && npx playwright show-report playwright-report

# ---- knomit-desktop (Wails v3) ----------------------------------------------
# Desktop shares the unified version scheme (VERSION.GIT_COMMIT via internal/version).
DESKTOP_BUILD = CGO_ENABLED=1 go build $(GOFLAGS) -tags desktop -ldflags "$(VERSION_LDFLAGS)"

# Install the OS deps the desktop app (Wails v3, CGO) needs to BUILD. macOS and
# Windows use system frameworks (Cocoa/WebKit, WebView2) — nothing to install;
# Linux needs the GTK4 + WebKitGTK 6.0 dev stack. The -dev packages pull in the
# matching runtime libs, so after this the built binary also runs. Run once per
# machine (needs sudo); not a prerequisite of `desktop` so normal builds and CI
# never shell out to a package manager.
desktop-deps:
ifeq ($(GOOS),darwin)
	@echo "macOS: no extra deps — the desktop app uses system Cocoa/WebKit frameworks."
else ifeq ($(GOOS),windows)
	@echo "Windows: install the WebView2 Evergreen runtime (preinstalled on Win11)."
else
	@command -v apt-get >/dev/null 2>&1 || { \
	  echo "Non-apt Linux: install these with your package manager, then re-run 'make desktop':"; \
	  echo "  gtk4 + webkitgtk-6.0 + libsoup-3.0 + glib2 (gio-unix-2.0) dev headers,"; \
	  echo "  a C/C++ toolchain, pkg-config, and sqlite3 dev headers."; \
	  exit 1; }
	sudo apt-get update
	sudo apt-get install -y --no-install-recommends \
	  build-essential pkg-config \
	  libgtk-4-dev libwebkitgtk-6.0-dev libsoup-3.0-dev \
	  libglib2.0-dev libsqlite3-dev
endif

# Build the native desktop app (Wails v3, CGO). Serves the UI in-process and
# runs the knomit server API-only on a looknomitck port (prefers 19278). Wails
# cannot cross-compile, so this builds for the host platform only.
#   - macOS:        ONLY a real $(DIST)/Knomit.app bundle (the binary is built
#                   straight into it — no loose executable left behind).
#   - Linux/Windows: the standalone $(DIST)/knomit-desktop binary (no bundle).
desktop: web desktop-ui download-ort tokenizers-lib
ifeq ($(GOOS),darwin)
	@$(MAKE) --no-print-directory desktop-app-macos
	@echo "Built $(APP) — launch with: open $(APP)"
else
	mkdir -p $(DIST)
	$(DESKTOP_BUILD) -o $(DIST)/knomit-desktop ./tools/desktop
	go build $(GOFLAGS) -ldflags "$(VERSION_LDFLAGS)" -o $(DIST)/knomit-bridge ./tools/bridge
	go build $(GOFLAGS) -ldflags "$(VERSION_LDFLAGS)" -o $(DIST)/knomit-okf ./tools/okf
	# No knomit-backup here, deliberately. Backup is a SERVER-build feature: the
	# desktop build forces backup.enabled off at config load
	# (internal/config/backup_desktop.go, a project-owner ruling), so the agent
	# could never be spawned and shipping it would only suggest otherwise.
	@echo "Built $(DIST)/knomit-desktop + knomit-bridge + knomit-okf"
endif

# Assemble the macOS .app bundle. The desktop binary is built DIRECTLY into the
# bundle (Contents/MacOS/) so no loose copy is left under $(DIST); the binary
# resolves the ONNX dylibs from <exe>/lib, i.e. Contents/MacOS/lib,
# so they are copied there. libtokenizers.a is linked statically (no runtime
# lib). The bundle lives only at $(APP) (no top-level symlink — nothing
# references it by a fixed path; launch it with `open $(APP)`). Assumes the
# desktop target's prerequisites (web build + native libs) have already run.
APP := $(DIST)/Knomit.app
desktop-app-macos:
	rm -rf $(APP)
	mkdir -p $(APP)/Contents/MacOS/lib $(APP)/Contents/Resources
	$(DESKTOP_BUILD) -o $(APP)/Contents/MacOS/knomit-desktop ./tools/desktop
	# knomit-bridge: the stdio↔HTTP MCP adapter stdio clients launch. Pure Go
	# (no CGO/dylibs), shipped next to the desktop binary; the app symlinks it
	# to <home>/bin on launch for a stable MCP command path.
	go build $(GOFLAGS) -ldflags "$(VERSION_LDFLAGS)" -o $(APP)/Contents/MacOS/knomit-bridge ./tools/bridge
	# knomit-okf: the OKF export CLI. Also pure Go, and also symlinked to
	# <home>/bin on launch — a CLI reachable only at
	# /Applications/Knomit.app/Contents/MacOS/knomit-okf is one nobody runs.
	go build $(GOFLAGS) -ldflags "$(VERSION_LDFLAGS)" -o $(APP)/Contents/MacOS/knomit-okf ./tools/okf
	# No knomit-backup in the bundle, deliberately — see the `desktop` target.
	# Backup is a server-build feature and the desktop build cannot switch it on.
	cp $(LIBDIR)/libonnxruntime.dylib $(APP)/Contents/MacOS/lib/
	# CFBundleShortVersionString gets the full $(VERSION), so a dev bundle reads
	# 0.5.2-dev.<epoch> in Finder's Get Info rather than impersonating the last
	# stable release — telling the two apart is the point of the dev channel
	# version. Apple documents this key as three period-separated integers, but
	# it is a DISPLAY string: upgrade ordering is CFBundleVersion's job, and
	# that stays the bare monotonic $(BUILD_VERSION) integer on both channels.
	sed -e 's/{{SHORT_VERSION}}/$(VERSION)/g' -e 's/{{BUILD_VERSION}}/$(BUILD_VERSION)/g' tools/desktop/macos/Info.plist > $(APP)/Contents/Info.plist
	@[ -f tools/desktop/macos/icon.icns ] && cp tools/desktop/macos/icon.icns $(APP)/Contents/Resources/icon.icns || echo "  (no icon.icns — using generic app icon)"
	# Ad-hoc sign the assembled bundle. LAST in this recipe, and inner code
	# before the bundle: the bundle's seal covers Contents/, so signing before
	# Info.plist and the icon land — or re-signing a nested binary afterwards —
	# seals a tree that no longer matches.
	#
	# Without this the bundle carries ONLY the Go linker's ad-hoc signature on
	# knomit-desktop (Identifier=a.out, flags=adhoc,linker-signed). That
	# signature declares a sealed resource envelope that does not exist —
	# there is no Contents/_CodeSignature — so `codesign --verify` fails with
	# "code has no resources but signature indicates they must be present" and
	# macOS reports a QUARANTINED copy as "damaged — move it to the Trash".
	# That dialog is a dead end: no Open Anyway, no right-click override.
	#
	# It only ever bites users who DOWNLOAD a release. A locally built or
	# self-updated bundle is never quarantined, so macOS never runs the strict
	# validation that trips on the missing envelope — which is exactly why
	# every test before the first published release missed it.
	#
	# With CODESIGN_IDENTITY empty this is ad-hoc (-), NOT Developer ID: not
	# notarization, and it does not remove the `xattr -cr` step in the release
	# notes. What it buys is (1) an unrecoverable "damaged" becoming the
	# ordinary unidentified-developer prompt, which Open Anyway can clear, and
	# (2) Info.plist bound into the signature, so the app's signed identity is
	# com.knomit.desktop rather than `a.out` — which is what
	# UNUserNotificationCenter reads.
	#
	# EVERY nested Mach-O must be listed explicitly, innermost first. Two traps
	# live here:
	#
	#   - A glob like Contents/MacOS/* expands to the `lib` DIRECTORY, which
	#     codesign rejects with "bundle format unrecognized, invalid, or
	#     unsuitable" — and, worse, never reaches the dylib inside it. That is
	#     how libonnxruntime.dylib shipped ad-hoc into a Developer ID bundle
	#     and failed notarization with "not signed with a valid Developer ID
	#     certificate". Sign lib/*.dylib as its own step.
	#   - Signing the bundle does NOT sign the code inside it. The bundle seal
	#     covers nested binaries by hash, so whatever signature they already
	#     carry is what gets sealed in.
	#
	# knomit-desktop is signed explicitly too, in BOTH modes. The Go linker
	# gives it an ad-hoc signature at build time (Identifier=a.out,
	# flags=adhoc,linker-signed), so leaving it out means sealing that in — the
	# chosen identity under Developer ID, and the wrong bundle identifier under
	# ad-hoc. This is the one way the default path differs from before.
	@echo "  codesign: $(if $(CODESIGN_IDENTITY),$(CODESIGN_IDENTITY),ad-hoc)"
	codesign $(CODESIGN_FLAGS) $(APP)/Contents/MacOS/lib/libonnxruntime.dylib
	codesign $(CODESIGN_FLAGS) $(APP)/Contents/MacOS/knomit-bridge
	codesign $(CODESIGN_FLAGS) $(APP)/Contents/MacOS/knomit-okf
	codesign $(CODESIGN_FLAGS) $(APP)/Contents/MacOS/knomit-desktop
	codesign $(CODESIGN_FLAGS) $(APP)
ifneq ($(CODESIGN_IDENTITY),)
	# Assert what notarization will check, here where it is a build failure
	# rather than a 20-minute round trip to Apple. Both of these were real
	# rejection reasons on submission d7e41665.
	@for f in $(APP)/Contents/MacOS/lib/libonnxruntime.dylib \
	          $(APP)/Contents/MacOS/knomit-bridge \
	          $(APP)/Contents/MacOS/knomit-okf \
	          $(APP)/Contents/MacOS/knomit-desktop \
	          $(APP); do \
	  codesign -dvvv "$$f" 2>&1 | grep -q "^Authority=Developer ID Application" \
	    || { echo "$$f: not signed with a Developer ID certificate"; exit 1; }; \
	  codesign -dvvv "$$f" 2>&1 | grep -q "^Timestamp=" \
	    || { echo "$$f: signature has no secure timestamp"; exit 1; }; \
	done
	@echo "  all nested code carries Developer ID + secure timestamp"
endif
	# Assert the seal rather than trusting the exit code above: a bundle that
	# signs cleanly can still fail validation, and this is the exact check the
	# user's machine runs on first launch.
	@codesign --verify --deep --strict $(APP) \
	  || { echo "$(APP): bundle signature does not validate"; exit 1; }

# Regenerate every desktop icon asset from the canonical logos. Requires
# rsvg-convert + iconutil (macOS). The outputs are committed (the Go binary
# //go:embeds them), so this only needs rerunning when a logo changes.
#   - icon.png             64px colored tray icon (Linux/Windows tray)
#   - appicon.png          256px colored app/window icon (Options.Icon; Linux
#                          window/taskbar + .desktop launcher)
#   - icon-tray-light.png  64px tray icon for a LIGHT macOS menu bar (dark glyph)
#   - icon-tray-dark.png   64px tray icon for a DARK macOS menu bar (light glyph)
#                          rendered from icon-tray-{light,dark}.svg; the app
#                          swaps between them on theme change
#   - macos/icon.icns      the .app bundle icon
desktop-icons:
	rsvg-convert -w 64 -h 64 web/public/logo.svg -o tools/desktop/icon.png
	rsvg-convert -w 256 -h 256 web/public/logo.svg -o tools/desktop/appicon.png
	rsvg-convert -w 64 -h 64 tools/desktop/icon-tray-light.svg -o tools/desktop/icon-tray-light.png
	rsvg-convert -w 64 -h 64 tools/desktop/icon-tray-dark.svg -o tools/desktop/icon-tray-dark.png
	rm -rf /tmp/knomit.iconset && mkdir -p /tmp/knomit.iconset
	for sz in 16 32 128 256 512; do \
	  rsvg-convert -w $$sz -h $$sz web/public/logo.svg -o /tmp/knomit.iconset/icon_$${sz}x$${sz}.png; \
	  rsvg-convert -w $$((sz*2)) -h $$((sz*2)) web/public/logo.svg -o /tmp/knomit.iconset/icon_$${sz}x$${sz}@2x.png; \
	done
	iconutil -c icns /tmp/knomit.iconset -o tools/desktop/macos/icon.icns
	@echo "Regenerated tools/desktop/{icon,appicon,icon-tray-light,icon-tray-dark}.png and tools/desktop/macos/icon.icns"

# Install the Linux desktop launcher: copies the built binary, a hicolor app
# icon, and a .desktop entry into the user's XDG dirs so Knomit appears in the
# GNOME/KDE app grid with its icon. Run after `make desktop`. macOS/Windows do
# not need this (the .app bundle / .exe carry their own icon). Honors XDG_*
# overrides; falls back to the standard ~/.local paths.
XDG_BIN  ?= $(HOME)/.local/bin
XDG_DATA ?= $(if $(XDG_DATA_HOME),$(XDG_DATA_HOME),$(HOME)/.local/share)
HICOLOR  := $(XDG_DATA)/icons/hicolor/256x256/apps
desktop-install:
ifeq ($(GOOS),darwin)
	@echo "macOS: nothing to install — launch the bundle with 'open $(APP)'."
else
	@test -f $(DIST)/knomit-desktop || { echo "build it first: make desktop"; exit 1; }
	mkdir -p $(XDG_BIN) $(HICOLOR) $(XDG_DATA)/applications
	install -m 0755 $(DIST)/knomit-desktop $(XDG_BIN)/knomit-desktop
	install -m 0644 tools/desktop/appicon.png $(HICOLOR)/knomit-desktop.png
	sed 's#{{EXEC}}#$(XDG_BIN)/knomit-desktop#' tools/desktop/linux/knomit-desktop.desktop \
	  > $(XDG_DATA)/applications/knomit-desktop.desktop
	-update-desktop-database $(XDG_DATA)/applications 2>/dev/null
	-gtk-update-icon-cache -f -t $(XDG_DATA)/icons/hicolor 2>/dev/null
	@echo "Installed knomit-desktop launcher + icon under $(XDG_DATA)."
endif

desktop-run: desktop
ifeq ($(GOOS),darwin)
	open $(APP)
else
	$(DIST)/knomit-desktop
endif

# ---- release packaging ------------------------------------------------------
# Assemble downloadable release artifacts under dist/release/. Wails cannot
# cross-compile, so each runner packages ONLY its own platform's artifacts;
# the GitHub release job collects the per-platform outputs. Filenames carry the
# BARE semver + PLATFORM — PLATFORM is what keeps one runner's outputs from
# colliding with another's. Every target builds its prerequisite (`build` /
# `desktop`) first, so `make release` on a clean checkout produces that
# platform's downloads end to end.
#
# No SHA in the filename. A stable release is immutable and already identified
# by its tag, so semver.sha here only made the published names disagree with
# the ones the release notes and the appcast feed talk about. Commit identity
# is NOT lost — it moved out of the filename, not out of the build. Every
# binary still reports semver.sha from internal/version: `knomit version`,
# `knomit-bridge version`, `knomit-okf version`, `knomit-desktop --version`,
# the desktop startup log line, and GET /api/v1/version (as `full`).
#
# Consequence for the ROLLING dev-latest pre-release: successive dev builds at
# the same semver now produce identically named assets, replacing each other on
# every run. That is what "rolling" already meant — the tag moves too — and the
# release notes still name the exact FULL_VERSION and SHA the assets came from.
RELEASE_DIR       := dist/release
SERVER_PKG        := knomit-$(VERSION)-$(PLATFORM)
DESKTOP_MAC_ZIP   := Knomit-$(VERSION)-$(PLATFORM).app.zip
# Linux desktop ships as one self-contained AppImage rather than the directory
# tarball it used to be: a single file needs no install.sh, and the AppImage
# runtime handles desktop-menu integration itself.
DESKTOP_APPIMAGE  := Knomit-$(VERSION)-$(PLATFORM).AppImage
APPDIR            := $(DIST)/Knomit.AppDir
# appimagetool names architectures the way uname does, not the way Go does, and
# it REQUIRES $ARCH — it cannot infer one from the AppDir. Hardcoding x86_64
# would silently mislabel an arm64 build rather than fail it.
ifeq ($(GOARCH),amd64)
  APPIMAGE_ARCH := x86_64
else ifeq ($(GOARCH),arm64)
  APPIMAGE_ARCH := aarch64
else
  APPIMAGE_ARCH := $(GOARCH)
endif

release: release-server release-desktop
	@echo "Release artifacts for $(PLATFORM):"
	@ls -1 $(RELEASE_DIR)

# Print the full build version (semver.sha) — the single source of truth the
# release workflow uses to tag the Docker image and name the GitHub release.
print-version:
	@echo $(FULL_VERSION)

# Print the BARE semver — no SHA, no `v` prefix. The stable-release workflow
# compares the pushed tag against this and refuses to publish on a mismatch, so
# VERSION above stays the single source of truth. A tag that disagreed with it
# would publish binaries reporting a different version than the update feed
# advertises, and the updater would then re-offer the same update forever
# because the installed version never catches up.
print-semver:
	@echo $(VERSION)

# Server tarball. The per-platform dist dir already IS the runtime layout —
# knomit + knomit-bridge resolve their ONNX libs from <exe>/lib
# (internal/embeddings/embedder.go, internal/store/vec.go) — so we just stage
# those things under a versioned top-level dir and tar it. libtokenizers.a
# is a build-time STATIC lib (never dlopen'd at runtime), so it is dropped.
# knomit-okf ships too: it is the only OKF export path, and a tool nobody can
# install is a tool nobody uses. Pure Go, so it needs nothing from lib/.
# knomit-backup is NOT optional: knomit locates it beside its own executable and
# REFUSES TO START when backup.enabled is true and it is missing, so a tarball
# without it is a tarball that cannot run with replication on.
release-server: build
	mkdir -p $(RELEASE_DIR)
	rm -rf $(DIST)/$(SERVER_PKG)
	mkdir -p $(DIST)/$(SERVER_PKG)/lib
	cp $(DIST)/knomit $(DIST)/knomit-bridge $(DIST)/knomit-okf $(DIST)/knomit-backup $(DIST)/$(SERVER_PKG)/
	cp -R $(LIBDIR)/. $(DIST)/$(SERVER_PKG)/lib/
	rm -f $(DIST)/$(SERVER_PKG)/lib/*.a
ifeq ($(GOOS),darwin)
	# Sign the STAGED copies, so `make release CODESIGN_IDENTITY=…` cannot
	# produce a signed .app beside an unsigned tarball carrying the same three
	# executables. A downloaded .tar.gz is quarantined like anything else, and
	# an unsigned Mach-O out of quarantine is the "damaged" dialog again.
	#
	# Mach-O signatures live in the binary (LC_CODE_SIGNATURE), not in xattrs,
	# so tar carries them through intact.
	#
	# The lib/ glob is safe where the bundle's was not: this directory holds
	# only files, never a nested dir, and the .a is already gone by here.
	codesign $(CODESIGN_FLAGS_LOOSE) $(DIST)/$(SERVER_PKG)/lib/*.dylib
	codesign $(CODESIGN_FLAGS_LOOSE) $(DIST)/$(SERVER_PKG)/knomit
	codesign $(CODESIGN_FLAGS_LOOSE) $(DIST)/$(SERVER_PKG)/knomit-bridge
	codesign $(CODESIGN_FLAGS_LOOSE) $(DIST)/$(SERVER_PKG)/knomit-okf
	@echo "  signed: $(if $(CODESIGN_IDENTITY),$(CODESIGN_IDENTITY),ad-hoc) (not notarized — see CODESIGN_FLAGS_LOOSE)"
endif
	tar -C $(DIST) -czf $(RELEASE_DIR)/$(SERVER_PKG).tar.gz $(SERVER_PKG)
	rm -rf $(DIST)/$(SERVER_PKG)
	@echo "Packaged $(RELEASE_DIR)/$(SERVER_PKG).tar.gz"

# Notarize the assembled .app and staple the ticket INTO it.
#
# Ordering is the whole point of this target existing separately: the ticket is
# stapled to the .app, never to a zip, and release-desktop zips whatever is on
# disk. Notarize after `desktop` and before the ditto, or the published zip
# contains an unstapled bundle — which still works online (Gatekeeper can ask
# Apple) and fails on a machine that is offline or behind a filtering proxy.
#
# The zip submitted here is a throwaway: Apple only reads it, and the artifact
# that ships is re-zipped from the stapled bundle afterwards. It still uses the
# SAME ditto flags as the shipping one — in particular no --sequesterRsrc, see
# release-desktop below for what that flag cost us — so that nobody reading the
# two invocations has to work out whether the difference was deliberate.
desktop-notarize:
ifeq ($(NOTARY_AUTH),)
	@echo "Notarization skipped (no NOTARY_PROFILE or NOTARY_KEY set)."
	@echo "  The bundle is signed but has no ticket: Gatekeeper will still"
	@echo "  prompt, and the release notes' xattr step remains necessary."
else
	@[ "$(GOOS)" = darwin ] || { \
	  echo "desktop-notarize: only macOS can notarize (GOOS=$(GOOS))."; exit 1; }
	@[ -d "$(APP)" ] || { \
	  echo "desktop-notarize: no bundle at $(APP) — run 'make desktop' first."; \
	  exit 1; }
	@[ -n "$(CODESIGN_IDENTITY)" ] || { \
	  echo "desktop-notarize: CODESIGN_IDENTITY is empty."; \
	  echo "  Apple rejects ad-hoc signatures — sign with Developer ID first."; \
	  exit 1; }
	rm -f $(DIST)/notarize.zip
	ditto -c -k --keepParent $(APP) $(DIST)/notarize.zip
	# Silent on purpose: make would otherwise echo the expanded NOTARY_AUTH,
	# putting the API key path, Key ID and Issuer ID into public CI logs.
	@echo "  submitting to Apple and waiting on their queue (minutes, not seconds)"
	@xcrun notarytool submit $(DIST)/notarize.zip $(NOTARY_AUTH) --wait
	rm -f $(DIST)/notarize.zip
	xcrun stapler staple $(APP)
	# Two checks, and they are not redundant. stapler validate reads the
	# bundle: it proves the ticket is EMBEDDED, which is the only thing that
	# helps a Mac that is offline or behind a filtering proxy. spctl reports
	# Gatekeeper's verdict, which on a connected build machine it can reach by
	# asking Apple — so spctl alone would pass on an unstapled bundle and tell
	# us nothing about the case this target exists for.
	@xcrun stapler validate $(APP) \
	  || { echo "$(APP): no ticket stapled into the bundle — offline launches"; \
	       echo "  will be blocked even though Gatekeeper may pass online"; \
	       exit 1; }
	# "Notarized Developer ID" is the only source string that means the
	# download opens without a prompt; "Unnotarized Developer ID" means the
	# ticket was never issued.
	@spctl -a -vvv -t exec $(APP) 2>&1 | grep -q "source=Notarized Developer ID" \
	  || { echo "$(APP): stapled ticket not recognised by Gatekeeper"; \
	       spctl -a -vvv -t exec $(APP); exit 1; }
	@echo "  notarized and stapled: source=Notarized Developer ID"
endif

# Desktop bundle/AppImage.
#   - macOS: ditto-zip the .app (preserves the bundle's symlinks + attrs; a
#            plain `zip` corrupts it). Signed, and notarized when credentials
#            are present — see desktop-notarize above; install notes still
#            cover Gatekeeper for unnotarized builds.
#            The single top-level entry is Knomit.app, which is exactly what
#            the self-updater's one-path swap requires — pkg/updater/extract.go
#            ReadDir's the extraction scratch dir and refuses anything but one
#            entry. NO --sequesterRsrc: it hoists HFS metadata (here just
#            com.apple.provenance, applied by the OS to every downloaded file)
#            into a SECOND top-level __MACOSX/ directory, and the updater then
#            rejects the archive with "archive must contain exactly one
#            top-level entry, got 2" — after the download and after the
#            signature verified, so nothing upstream catches it. The bundle
#            carries no resource forks worth sequestering, and the extracted
#            tree is byte-identical without the flag.
#   - Linux: one self-contained .AppImage. Native libs go under usr/bin/lib/,
#            NOT usr/lib/ — knomit resolves them from <exe>/lib, so keeping
#            them beside the binary preserves that lookup and spares AppRun an
#            LD_LIBRARY_PATH dance. GTK 4 and WebKitGTK 6.0 stay host
#            requirements, as the tarball always required.
release-desktop: desktop
	mkdir -p $(RELEASE_DIR)
ifeq ($(GOOS),darwin)
	@$(MAKE) --no-print-directory desktop-notarize
	rm -f $(RELEASE_DIR)/$(DESKTOP_MAC_ZIP)
	ditto -c -k --keepParent $(APP) $(RELEASE_DIR)/$(DESKTOP_MAC_ZIP)
	# Guard, not decoration. A zip with a second top-level entry verifies,
	# downloads and THEN fails to install, so the break surfaces only on an
	# already-published release that every client re-attempts forever. Assert
	# the shape here, where it is still a build failure.
	@n=$$(unzip -Z1 $(RELEASE_DIR)/$(DESKTOP_MAC_ZIP) | cut -d/ -f1 | sort -u | wc -l | tr -d ' '); \
	  [ "$$n" = 1 ] || { \
	    echo "$(DESKTOP_MAC_ZIP): $$n top-level entries, the updater requires exactly 1:"; \
	    unzip -Z1 $(RELEASE_DIR)/$(DESKTOP_MAC_ZIP) | cut -d/ -f1 | sort -u | sed 's/^/  /'; \
	    exit 1; }
	@echo "Packaged $(RELEASE_DIR)/$(DESKTOP_MAC_ZIP)"
else
	rm -rf $(APPDIR)
	mkdir -p $(APPDIR)/usr/bin/lib
	cp $(DIST)/knomit-desktop $(DIST)/knomit-bridge $(DIST)/knomit-okf $(APPDIR)/usr/bin/
	cp -R $(LIBDIR)/. $(APPDIR)/usr/bin/lib/
	rm -f $(APPDIR)/usr/bin/lib/*.a
	install -m 0755 tools/desktop/linux/AppRun $(APPDIR)/AppRun
	# The .desktop template carries Exec={{EXEC}} (substituted by
	# `make desktop-install` for local installs) and Icon=knomit-desktop.
	# AppImage needs a real Exec relative to the AppDir, and an icon file at the
	# AppDir root whose basename MATCHES the Icon= key — hence appicon.png being
	# renamed. A mismatch fails appimagetool with "could not find icon file".
	sed -e 's|{{EXEC}}|knomit-desktop|' \
	  tools/desktop/linux/knomit-desktop.desktop > $(APPDIR)/knomit-desktop.desktop
	cp tools/desktop/appicon.png $(APPDIR)/knomit-desktop.png
	# --appimage-extract-and-run: CI runners and containers routinely lack FUSE,
	# which appimagetool itself needs to self-mount.
	rm -f $(RELEASE_DIR)/$(DESKTOP_APPIMAGE)
	ARCH=$(APPIMAGE_ARCH) appimagetool --appimage-extract-and-run $(APPDIR) $(RELEASE_DIR)/$(DESKTOP_APPIMAGE)
	rm -rf $(APPDIR)
	@echo "Packaged $(RELEASE_DIR)/$(DESKTOP_APPIMAGE)"
endif
