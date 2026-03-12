package web

import (
	_ "embed"
	"net/http"
)

//go:embed static/openapi.yaml
var openapiSpec []byte

//go:embed static/swagger.html
var swaggerHTML []byte

func handleOpenAPISpec() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(openapiSpec)
	}
}

func handleSwaggerUI() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(swaggerHTML)
	}
}
