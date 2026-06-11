.PHONY: build web test clean run dev setup dist download-ort download-graphqlite tokenizers-lib e2e e2e-ui e2e-setup e2e-report tray tray-run

UNAME_S := $(shell uname -s)
TRAY_VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Native libraries (ONNX Runtime, graphqlite, libtokenizers.a) are fetched by
# the cross-platform Go tool tools/fetchlibs, which is the single source of
# truth for their versions and per-platform asset names (and works on Windows,
# unlike the old bash/uname scripts). The only platform bit Make still needs is
# the ORT library filename, for the `run` target's ORT_LIB_PATH.
ifeq ($(UNAME_S),Darwin)
  ORT_LIB_NAME := libonnxruntime.dylib
else ifeq ($(UNAME_S),Linux)
  ORT_LIB_NAME := libonnxruntime.so
else
  ORT_LIB_NAME := onnxruntime.dll
endif

setup:
	go run ./tools/fetchlibs dist/lib
	@echo "Setup complete. Run 'make run' to start the server."

download-ort:
	go run ./tools/fetchlibs -only ort dist/lib

download-graphqlite:
	go run ./tools/fetchlibs -only graphqlite dist/lib

tokenizers-lib:
	go run ./tools/fetchlibs -only tokenizers dist/lib

build: web tray
	CGO_ENABLED=1 go build $(GOFLAGS) -o dist/knomit .
	go build $(GOFLAGS) -o dist/knomit-bridge ./tools/bridge/

web:
	cd web && npm ci && npm run build

test: download-graphqlite
	CGO_ENABLED=1 go test $(GOFLAGS) ./...

dist: download-ort download-graphqlite tokenizers-lib build
	@echo "Distribution package ready in dist/"

CMD ?= serve
run: download-ort
	CGO_ENABLED=1 ORT_LIB_PATH=dist/lib/$(ORT_LIB_NAME) go run $(GOFLAGS) . $(CMD)

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

# ---- knomit-tray (macOS phase 1 + Linux phase 2) ----------------------------

tray:
ifeq ($(UNAME_S),Darwin)
	CGO_ENABLED=1 go build $(GOFLAGS) -ldflags "-X knomit/tools/tray/cmd.version=$(TRAY_VERSION)" -o dist/knomit-tray ./tools/tray
	@echo "Built dist/knomit-tray (macOS)"
else ifeq ($(UNAME_S),Linux)
  # webview_go hardcodes pkg-config webkit2gtk-4.0; Debian 13+ ships only 4.1.
  # If 4.0 is missing but 4.1 is present, create a shim .pc so CGO finds it.
	@if ! pkg-config --exists webkit2gtk-4.0 2>/dev/null && pkg-config --exists webkit2gtk-4.1 2>/dev/null; then \
		mkdir -p dist/.pc; \
		cp "$$(pkg-config --variable=pcfiledir webkit2gtk-4.1)/webkit2gtk-4.1.pc" dist/.pc/webkit2gtk-4.0.pc; \
		echo "Created webkit2gtk-4.0 shim (pointing to 4.1)"; \
	fi
	CGO_ENABLED=1 PKG_CONFIG_PATH="$(CURDIR)/dist/.pc:$$PKG_CONFIG_PATH" go build $(GOFLAGS) -ldflags "-X knomit/tools/tray/cmd.version=$(TRAY_VERSION)" -o dist/knomit-tray ./tools/tray
	@go run $(GOFLAGS) tools/tray/linux/genicon.go
	@sed -e 's|{{BINARY}}|$(CURDIR)/dist/knomit-tray|g' \
	     -e 's|{{ICON}}|$(CURDIR)/dist/knomit.png|g' \
	     tools/tray/linux/knomit.desktop.tmpl > dist/knomit.desktop
	@sed -e 's|{{BINARY}}|$(CURDIR)/dist/knomit-tray|g' \
	     -e 's|{{KNOMIT_BIN}}|$(CURDIR)/dist/knomit|g' \
	     tools/tray/linux/knomit-tray.service.tmpl > dist/knomit-tray.service
	@echo "Built dist/knomit-tray, dist/knomit.png, dist/knomit.desktop, dist/knomit-tray.service (Linux)"
else
	@echo "knomit-tray build is macOS/Linux only; current platform: $(UNAME_S)"
	@exit 1
endif

# Run the tray against the already-built main binary in dist/.
tray-run: build tray
	KNOMIT_BIN=$(PWD)/dist/knomit ./dist/knomit-tray
