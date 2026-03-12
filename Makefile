.PHONY: build web test clean run dev setup

setup:
	brew install onnxruntime
	@echo "Run 'make run' to start the server"

build: web
	CGO_ENABLED=1 go build -tags sqlite_fts5 -o dist/knomit ./cmd/knomit/

web:
	cd web && npm ci && npm run build

test:
	CGO_ENABLED=1 go test -tags sqlite_fts5 ./...

run:
	@brew list onnxruntime &>/dev/null || (echo "onnxruntime not installed — run: make setup" && exit 1)
	CGO_ENABLED=1 go run -tags sqlite_fts5 ./cmd/knomit/ serve

dev:
	cd web && npm run dev

clean:
	rm -rf dist/ web/dist/
