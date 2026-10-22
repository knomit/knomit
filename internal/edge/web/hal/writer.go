package hal

import (
	"encoding/json"
	"net/http"
)

// ContentType is the media type for HAL responses.
const ContentType = "application/hal+json"

// WriteHAL encodes v as application/hal+json with the given status. The value
// is expected to already contain a `_links` map with at least a self link.
func WriteHAL(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", ContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteHALCreated writes a 201 Created response with Location and
// Content-Location headers set to the same URI. Used after POST-to-collection.
func WriteHALCreated(w http.ResponseWriter, location string, v any) {
	w.Header().Set("Location", location)
	w.Header().Set("Content-Location", location)
	WriteHAL(w, http.StatusCreated, v)
}
