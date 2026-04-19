.PHONY: build web test clean run dev setup dist download-ort download-graphqlite e2e e2e-ui e2e-setup e2e-report tray tray-run

ORT_VERSION := 1.24.3
UNAME_S := $(shell uname -s)
UNAME_M := $(shell uname -m)
TRAY_VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Detect platform for ORT download
ifeq ($(UNAME_S),Darwin)
  ifeq ($(UNAME_M),arm64)
    ORT_PLATFORM := osx-arm64
    ORT_LIB_NAME := libonnxruntime.dylib
    ORT_LIB_VERSIONED := libonnxruntime.$(ORT_VERSION).dylib
  else
    ORT_PLATFORM := osx-x86_64
    ORT_LIB_NAME := libonnxruntime.dylib
    ORT_LIB_VERSIONED := libonnxruntime.$(ORT_VERSION).dylib
  endif
else ifeq ($(UNAME_S),Linux)
  ORT_PLATFORM := linux-x64
  ORT_LIB_NAME := libonnxruntime.so
  ORT_LIB_VERSIONED := libonnxruntime.so.$(ORT_VERSION)
else
  ORT_PLATFORM := win-x64
  ORT_LIB_NAME := onnxruntime.dll
  ORT_LIB_VERSIONED := onnxruntime.dll
endif

ORT_URL := https://github.com/microsoft/onnxruntime/releases/download/v$(ORT_VERSION)/onnxruntime-$(ORT_PLATFORM)-$(ORT_VERSION).tgz

GRAPHQLITE_VERSION := 0.3.10
ifeq ($(UNAME_S),Darwin)
  ifeq ($(UNAME_M),arm64)
    GRAPHQLITE_ASSET := graphqlite-macos-arm64.dylib
    GRAPHQLITE_LIB := graphqlite.dylib
  else
    GRAPHQLITE_ASSET := graphqlite-macos-x86_64.dylib
    GRAPHQLITE_LIB := graphqlite.dylib
  endif
else ifeq ($(UNAME_S),Linux)
  ifeq ($(UNAME_M),aarch64)
    GRAPHQLITE_ASSET := graphqlite-linux-aarch64.so
    GRAPHQLITE_LIB := graphqlite.so
  else
    GRAPHQLITE_ASSET := graphqlite-linux-x86_64.so
    GRAPHQLITE_LIB := graphqlite.so
  endif
else
  GRAPHQLITE_ASSET := graphqlite-windows-x86_64.dll
  GRAPHQLITE_LIB := graphqlite.dll
endif
GRAPHQLITE_URL := https://github.com/colliery-io/graphqlite/releases/download/v$(GRAPHQLITE_VERSION)/$(GRAPHQLITE_ASSET)

setup: download-ort download-graphqlite
	@echo "Setup complete. Run 'make run' to start the server."

download-ort:
	@mkdir -p dist/lib
	@if [ ! -f dist/lib/$(ORT_LIB_NAME) ]; then \
		echo "Downloading onnxruntime $(ORT_VERSION) for $(ORT_PLATFORM)..."; \
		curl -sL $(ORT_URL) | tar xz -C /tmp; \
		cp /tmp/onnxruntime-$(ORT_PLATFORM)-$(ORT_VERSION)/lib/$(ORT_LIB_VERSIONED) dist/lib/$(ORT_LIB_NAME); \
		rm -rf /tmp/onnxruntime-$(ORT_PLATFORM)-$(ORT_VERSION); \
		echo "onnxruntime installed to dist/lib/"; \
	fi

download-graphqlite:
	@mkdir -p dist/lib
	@if [ ! -f dist/lib/$(GRAPHQLITE_LIB) ]; then \
		echo "Downloading graphqlite v$(GRAPHQLITE_VERSION)..."; \
		curl -sL $(GRAPHQLITE_URL) -o dist/lib/$(GRAPHQLITE_LIB); \
		if [ "$(UNAME_S)" = "Darwin" ]; then \
			codesign --sign - --force dist/lib/$(GRAPHQLITE_LIB); \
		fi; \
		echo "graphqlite installed to dist/lib/"; \
	fi

build: web
	CGO_ENABLED=1 go build -o dist/knomit .
	go build -o dist/knomit-remote ./tools/remote/

web:
	cd web && npm ci && npm run build

test: download-graphqlite
	CGO_ENABLED=1 go test ./...

dist: download-ort download-graphqlite build
	@echo "Distribution package ready in dist/"

CMD ?= serve
run: download-ort
	CGO_ENABLED=1 ORT_LIB_PATH=dist/lib/$(ORT_LIB_NAME) go run . $(CMD)

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
	CGO_ENABLED=1 go build -ldflags "-X knomit/tools/tray/cmd.version=$(TRAY_VERSION)" -o dist/knomit-tray ./tools/tray
	@echo "Built dist/knomit-tray (macOS)"
else ifeq ($(UNAME_S),Linux)
	CGO_ENABLED=1 go build -ldflags "-X knomit/tools/tray/cmd.version=$(TRAY_VERSION)" -o dist/knomit-tray ./tools/tray
	@go run tools/tray/linux/genicon.go
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
