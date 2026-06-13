.PHONY: build web test clean run dev setup dist docker download-ort download-graphqlite tokenizers-lib e2e e2e-ui e2e-setup e2e-report

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

# Build the cloud HTTP server as a self-contained Docker image (CGO + bundled
# ONNX/graphqlite native libs; embedding models download on first start).
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
