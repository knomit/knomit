.PHONY: build web test clean run dev setup dist docker desktop desktop-app-macos desktop-run download-ort download-graphqlite tokenizers-lib e2e e2e-ui e2e-setup e2e-report

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
# runs the knomit server API-only on a looknomitck port (prefers 19278). On macOS
# this produces a real dist/Knomit.app bundle (double-clickable, no terminal).
desktop: web download-ort download-graphqlite tokenizers-lib
	CGO_ENABLED=1 go build $(GOFLAGS) -tags desktop \
	  -ldflags "-X main.version=$(DESKTOP_VERSION)" \
	  -o dist/knomit-desktop ./tools/desktop
ifeq ($(UNAME_S),Darwin)
	@$(MAKE) --no-print-directory desktop-app-macos
	@echo "Built $(APP) — launch with: open $(APP)"
else
	@echo "Built dist/knomit-desktop"
endif

# Assemble the macOS .app bundle. The binary resolves the ONNX/graphqlite
# dylibs from <exe>/lib, i.e. Knomit.app/Contents/MacOS/lib, so the native
# libs are copied there. libtokenizers.a is linked statically (no runtime lib).
APP := dist/Knomit.app
desktop-app-macos:
	rm -rf $(APP)
	mkdir -p $(APP)/Contents/MacOS/lib $(APP)/Contents/Resources
	cp dist/knomit-desktop $(APP)/Contents/MacOS/knomit-desktop
	cp dist/lib/libonnxruntime.dylib $(APP)/Contents/MacOS/lib/
	cp dist/lib/graphqlite.dylib $(APP)/Contents/MacOS/lib/
	sed 's/{{VERSION}}/$(DESKTOP_VERSION)/g' tools/desktop/macos/Info.plist > $(APP)/Contents/Info.plist
	@[ -f tools/desktop/macos/icon.icns ] && cp tools/desktop/macos/icon.icns $(APP)/Contents/Resources/icon.icns || echo "  (no icon.icns — using generic app icon)"

desktop-run: desktop
ifeq ($(UNAME_S),Darwin)
	open $(APP)
else
	./dist/knomit-desktop
endif
