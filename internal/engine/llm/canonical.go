package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// CanonicalJSON re-encodes raw so the same value always yields the same bytes.
//
// It decodes with UseNumber, so numbers keep their literal spelling instead of
// passing through float64, and re-encodes through encoding/json, which writes
// map keys in sorted order: that sort is the whole canonicalisation, because a
// value decoded into map[string]any has lost its original key order anyway.
// HTML escaping is off, so "<" stays "<" rather than becoming <. The
// trailing newline the encoder adds is dropped.
//
// Why it matters: tool schemas sit in the cached prompt prefix, and Go's map
// iteration order is random, so a schema marshalled from a map without this
// step can change bytes between runs and silently miss the cache.
func CanonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("canonical json: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("canonical json: trailing data after the value")
	}
	return encodeCanonical(v)
}

// encodeCanonical marshals v with HTML escaping off and no trailing newline.
func encodeCanonical(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))), nil
}
