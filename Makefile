.PHONY: build web test clean run dev setup dist docker desktop desktop-deps desktop-app-macos desktop-icons desktop-install desktop-run download-ort download-graphqlite tokenizers-lib e2e e2e-ui e2e-setup e2e-report

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

# Native libraries (ONNX Runtime, graphqlite, libtokenizers.a) are fetched by
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

download-graphqlite:
	go run ./tools/fetchlibs -only graphqlite $(LIBDIR)

tokenizers-lib:
	go run ./tools/fetchlibs -only tokenizers $(LIBDIR)

# tokenizers-lib is linked statically at build time; ort + graphqlite are the
# shared libs the built binary dlopens at RUN time, so fetch them too — otherwise
# a fresh `make build && ./dist/<platform>/knomit serve` fails to load them.
build: web tokenizers-lib download-ort download-graphqlite
	mkdir -p $(DIST)
	CGO_ENABLED=1 go build $(GOFLAGS) -o $(DIST)/knomit .
	go build $(GOFLAGS) -o $(DIST)/knomit-bridge ./tools/bridge/
	$(call symlink_tool,knomit)
	$(call symlink_tool,knomit-bridge)

web:
	cd web && npm ci && npm run build

test: download-graphqlite tokenizers-lib
	CGO_ENABLED=1 go test $(GOFLAGS) ./...

dist: download-ort download-graphqlite tokenizers-lib build
	@echo "Distribution package ready in $(DIST)/"

# Build the cloud HTTP server as a fully self-contained Docker image (CGO +
# bundled ONNX/graphqlite native libs + embedding model baked at build time;
# the running container performs no startup downloads).
docker:
	docker build -t knomit:latest .

CMD ?= serve
run: download-ort download-graphqlite tokenizers-lib
	CGO_ENABLED=1 \
	  ORT_LIB_PATH=$(LIBDIR)/$(ORT_LIB_NAME) \
	  GRAPHQLITE_LIB_PATH=$(LIBDIR)/graphqlite \
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
DESKTOP_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
DESKTOP_BUILD = CGO_ENABLED=1 go build $(GOFLAGS) -tags desktop -ldflags "-X main.version=$(DESKTOP_VERSION)"

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
desktop: web download-ort download-graphqlite tokenizers-lib
ifeq ($(GOOS),darwin)
	@$(MAKE) --no-print-directory desktop-app-macos
	@echo "Built $(APP) — launch with: open $(APP)"
else
	mkdir -p $(DIST)
	$(DESKTOP_BUILD) -o $(DIST)/knomit-desktop ./tools/desktop
	@echo "Built $(DIST)/knomit-desktop"
endif

# Assemble the macOS .app bundle. The desktop binary is built DIRECTLY into the
# bundle (Contents/MacOS/) so no loose copy is left under $(DIST); the binary
# resolves the ONNX/graphqlite dylibs from <exe>/lib, i.e. Contents/MacOS/lib,
# so they are copied there. libtokenizers.a is linked statically (no runtime
# lib). The bundle lives only at $(APP) (no top-level symlink — nothing
# references it by a fixed path; launch it with `open $(APP)`). Assumes the
# desktop target's prerequisites (web build + native libs) have already run.
APP := $(DIST)/Knomit.app
desktop-app-macos:
	rm -rf $(APP)
	mkdir -p $(APP)/Contents/MacOS/lib $(APP)/Contents/Resources
	$(DESKTOP_BUILD) -o $(APP)/Contents/MacOS/knomit-desktop ./tools/desktop
	cp $(LIBDIR)/libonnxruntime.dylib $(APP)/Contents/MacOS/lib/
	cp $(LIBDIR)/graphqlite.dylib $(APP)/Contents/MacOS/lib/
	sed 's/{{VERSION}}/$(DESKTOP_VERSION)/g' tools/desktop/macos/Info.plist > $(APP)/Contents/Info.plist
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
