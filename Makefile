.PHONY: build web test clean run dev setup dist download-ort download-graphqlite

ORT_VERSION := 1.24.3
UNAME_S := $(shell uname -s)
UNAME_M := $(shell uname -m)

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

GRAPHQLITE_VERSION := 0.3.7
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
		echo "graphqlite installed to dist/lib/"; \
	fi

build: web
	CGO_ENABLED=1 go build -o dist/knomit .
	go build -o dist/knomit-mcp-remote ./tools/mcp-remote/

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
