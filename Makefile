.PHONY: build web test clean run dev setup dist docker docker-amd64 desktop desktop-deps desktop-app-macos desktop-icons desktop-install desktop-run download-ort tokenizers-lib e2e e2e-ui e2e-setup e2e-report release release-server release-desktop print-version print-semver

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

# Build version. VERSION is the Major.Minor.Patch semver and is the single
# source of truth — bump it here on release. GIT_COMMIT is the short SHA of the
# build. Both are injected into the internal/version package via -ldflags, so
# every binary (knomit, knomit-bridge, knomit-okf, knomit-desktop) reports e.g. 0.5.0.2a7ae9d.
# A bare `go build` (no make) falls back to the package default "dev".
VERSION    := 0.5.0
GIT_COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
# BUILD_VERSION is the macOS CFBundleVersion, which macOS/LaunchServices use to
# order builds for upgrade detection — so it MUST increase monotonically across
# releases. Apple also requires only digits 0-9 and periods (≤3 integer
# components). We use the HEAD commit's committer epoch seconds: a single legal
# integer (~1.78e9, well under 2^32 until ~2106) that grows with every commit,
# is deterministic per commit, and survives shallow CI clones (unlike a commit
# count). Commit IDENTITY is NOT encoded here — it lives in GIT_COMMIT (the SHA
# in internal/version / CFBundleShortVersionString stays the marketing semver).
# Falls back to 0 outside a git checkout.
BUILD_VERSION := $(shell git show -s --format=%ct HEAD 2>/dev/null || echo 0)
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
	$(call symlink_tool,knomit)
	$(call symlink_tool,knomit-bridge)
	$(call symlink_tool,knomit-okf)

web:
	cd web && npm ci && npm run build

test: tokenizers-lib
	CGO_ENABLED=1 go test $(GOFLAGS) ./...

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

CMD ?= serve
run: download-ort tokenizers-lib
	CGO_ENABLED=1 \
	  ORT_LIB_PATH=$(LIBDIR)/$(ORT_LIB_NAME) \
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
desktop: web download-ort tokenizers-lib
ifeq ($(GOOS),darwin)
	@$(MAKE) --no-print-directory desktop-app-macos
	@echo "Built $(APP) — launch with: open $(APP)"
else
	mkdir -p $(DIST)
	$(DESKTOP_BUILD) -o $(DIST)/knomit-desktop ./tools/desktop
	go build $(GOFLAGS) -ldflags "$(VERSION_LDFLAGS)" -o $(DIST)/knomit-bridge ./tools/bridge
	go build $(GOFLAGS) -ldflags "$(VERSION_LDFLAGS)" -o $(DIST)/knomit-okf ./tools/okf
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
	cp $(LIBDIR)/libonnxruntime.dylib $(APP)/Contents/MacOS/lib/
	sed -e 's/{{SHORT_VERSION}}/$(VERSION)/g' -e 's/{{BUILD_VERSION}}/$(BUILD_VERSION)/g' tools/desktop/macos/Info.plist > $(APP)/Contents/Info.plist
	@[ -f tools/desktop/macos/icon.icns ] && cp tools/desktop/macos/icon.icns $(APP)/Contents/Resources/icon.icns || echo "  (no icon.icns — using generic app icon)"

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
# the GitHub release job collects the per-platform outputs. Filenames carry
# FULL_VERSION (semver.sha) + PLATFORM so a single rolling release holds builds
# from several runners without collision. Every target builds its prerequisite
# (`build` / `desktop`) first, so `make release` on a clean checkout produces
# that platform's downloads end to end.
RELEASE_DIR       := dist/release
SERVER_PKG        := knomit-$(FULL_VERSION)-$(PLATFORM)
DESKTOP_MAC_ZIP   := Knomit-$(FULL_VERSION)-$(PLATFORM).app.zip
# Linux desktop ships as one self-contained AppImage rather than the directory
# tarball it used to be: a single file needs no install.sh, and the AppImage
# runtime handles desktop-menu integration itself.
DESKTOP_APPIMAGE  := Knomit-$(FULL_VERSION)-$(PLATFORM).AppImage
APPDIR            := $(DIST)/Knomit.AppDir

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
release-server: build
	mkdir -p $(RELEASE_DIR)
	rm -rf $(DIST)/$(SERVER_PKG)
	mkdir -p $(DIST)/$(SERVER_PKG)/lib
	cp $(DIST)/knomit $(DIST)/knomit-bridge $(DIST)/knomit-okf $(DIST)/$(SERVER_PKG)/
	cp -R $(LIBDIR)/. $(DIST)/$(SERVER_PKG)/lib/
	rm -f $(DIST)/$(SERVER_PKG)/lib/*.a
	tar -C $(DIST) -czf $(RELEASE_DIR)/$(SERVER_PKG).tar.gz $(SERVER_PKG)
	rm -rf $(DIST)/$(SERVER_PKG)
	@echo "Packaged $(RELEASE_DIR)/$(SERVER_PKG).tar.gz"

# Desktop bundle/AppImage.
#   - macOS: ditto-zip the .app (preserves the bundle's symlinks + attrs; a
#            plain `zip` corrupts it). Unsigned — install notes cover Gatekeeper.
#            The single top-level entry is Knomit.app, which is exactly what
#            the self-updater's one-path swap requires.
#   - Linux: one self-contained .AppImage. Native libs go under usr/bin/lib/,
#            NOT usr/lib/ — knomit resolves them from <exe>/lib, so keeping
#            them beside the binary preserves that lookup and spares AppRun an
#            LD_LIBRARY_PATH dance. GTK 4 and WebKitGTK 6.0 stay host
#            requirements, as the tarball always required.
release-desktop: desktop
	mkdir -p $(RELEASE_DIR)
ifeq ($(GOOS),darwin)
	rm -f $(RELEASE_DIR)/$(DESKTOP_MAC_ZIP)
	ditto -c -k --sequesterRsrc --keepParent $(APP) $(RELEASE_DIR)/$(DESKTOP_MAC_ZIP)
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
	ARCH=x86_64 appimagetool --appimage-extract-and-run $(APPDIR) $(RELEASE_DIR)/$(DESKTOP_APPIMAGE)
	rm -rf $(APPDIR)
	@echo "Packaged $(RELEASE_DIR)/$(DESKTOP_APPIMAGE)"
endif
