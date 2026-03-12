.PHONY: build web test clean run

build: web
	CGO_ENABLED=1 go build -tags sqlite_fts5 -o dist/knomit ./cmd/knomit/

web:
	cd web && npm ci && npm run build

test:
	CGO_ENABLED=1 go test -tags sqlite_fts5 ./...

run:
	CGO_ENABLED=1 go run -tags sqlite_fts5 ./cmd/knomit/ serve

clean:
	rm -rf dist/ web/dist/
