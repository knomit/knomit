package web

import (
	_ "embed"
	"net/http"
)

//go:embed static/openapi.yaml
var openapiSpec []byte // embedded OpenAPI 3.x specification

//go:embed static/swagger.html
var swaggerHTML []byte // embedded Swagger UI HTML page

// handleOpenAPISpec serves the raw OpenAPI YAML spec at /api/v1/openapi.yaml.
func handleOpenAPISpec() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(openapiSpec)
	}
}

// handleSwaggerUI serves the Swagger UI HTML page at /docs.
func handleSwaggerUI() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(swaggerHTML)
	}
}
