.PHONY: build web test clean run dev setup dist docker desktop desktop-run download-ort download-graphqlite tokenizers-lib e2e e2e-ui e2e-setup e2e-report

UNAME_S := $(shell uname -s)

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

build: web
	CGO_ENABLED=1 go build $(GOFLAGS) -o dist/knomit .
	go build $(GOFLAGS) -o dist/knomit-bridge ./tools/bridge/

web:
	cd web && npm ci && npm run build

test: download-graphqlite
	CGO_ENABLED=1 go test $(GOFLAGS) ./...

dist: download-ort download-graphqlite tokenizers-lib build
	@echo "Distribution package ready in dist/"

# Build the cloud HTTP server as a fully self-contained Docker image (CGO +
# bundled ONNX/graphqlite native libs + embedding model baked at build time;
# the running container performs no startup downloads).
docker:
	docker build -t knomit:latest .

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

# ---- knomit-desktop (Wails v3, cross-platform) ------------------------------
DESKTOP_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Build the native desktop app (Wails v3, CGO). Serves the UI in-process and
# runs the knomit server API-only on a looknomitck port (prefers 19278). Requires
# the native libs in dist/lib (run `make setup` first).
desktop: web
	CGO_ENABLED=1 go build $(GOFLAGS) -tags desktop \
	  -ldflags "-X main.version=$(DESKTOP_VERSION)" \
	  -o dist/knomit-desktop ./tools/desktop
	@echo "Built dist/knomit-desktop"

desktop-run: dist desktop
	./dist/knomit-desktop
