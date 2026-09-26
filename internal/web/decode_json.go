package web

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"strconv"

	"knomit/internal/web/hal"
)

// decodeJSON is how an API handler reads a JSON request body. It refuses,
// writing the problem document itself, and returns false when:
//
//   - the Content-Type is absent or is not application/json (parameters such
//     as charset, and letter case, are tolerated): 415, and nothing is read.
//     A cross-site page cannot send application/json without a CORS
//     preflight, which corsMiddleware refuses for an origin it does not
//     allow, so this rule makes every body-carrying mutation a non-simple
//     request. It is a second layer behind the loopback Origin check, for a
//     browser that sends no Origin at all. It does NOT cover a mutation with
//     no body: a request with no body has no Content-Type to judge.
//   - maxBytes > 0 and the body is longer: 413. Zero means no limit; each
//     caller passes the limit it already had, and none is invented here.
//   - the body is not one JSON value of v's shape: 400 "Invalid request
//     body", naming the byte offset (and the field, for a type mismatch).
//
// v must be a non-nil pointer. It is written only on success: the body is
// decoded into a fresh value and copied, so a half-decoded body never leaves a
// caller holding partial input. Unknown fields are accepted, as they always
// were; rejecting them is not a CSRF control and would break a client for
// sending more than the server reads.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any, maxBytes int64) bool {
	if !requireJSONContentType(w, r) {
		return false
	}
	return decodeBodyInto(w, r, r.Body, v, maxBytes)
}

// decodeOptionalJSON is decodeJSON for a route whose body may be absent: an
// empty body passes, whatever its Content-Type, and leaves v untouched. A
// non-empty body is held to decodeJSON's rules. A body of unknown length
// (chunked) is judged by peeking one byte, which is put back before decoding.
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, v any, maxBytes int64) bool {
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return true
	}
	body := io.Reader(r.Body)
	if r.ContentLength < 0 {
		br := bufio.NewReaderSize(r.Body, 16)
		if _, err := br.Peek(1); errors.Is(err, io.EOF) {
			return true
		}
		body = br
	}
	if !requireJSONContentType(w, r) {
		return false
	}
	return decodeBodyInto(w, r, body, v, maxBytes)
}

// requireJSONContentType writes the 415 and reports false unless r declares
// application/json.
func requireJSONContentType(w http.ResponseWriter, r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	if mt, _, err := mime.ParseMediaType(ct); err == nil && mt == "application/json" {
		return true
	}
	got := "none"
	if ct != "" {
		got = strconv.Quote(ct)
	}
	hal.WriteProblem(w, http.StatusUnsupportedMediaType, "Unsupported Media Type",
		"the request body must be sent with Content-Type: application/json; got "+got, r.URL.Path)
	return false
}

func decodeBodyInto(w http.ResponseWriter, r *http.Request, body io.Reader, v any, maxBytes int64) bool {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		panic("decodeJSON: v must be a non-nil pointer")
	}
	if maxBytes > 0 {
		body = http.MaxBytesReader(w, io.NopCloser(body), maxBytes)
	}
	fresh := reflect.New(rv.Elem().Type())
	if err := json.NewDecoder(body).Decode(fresh.Interface()); err != nil {
		writeDecodeProblem(w, r, err)
		return false
	}
	rv.Elem().Set(fresh.Elem())
	return true
}

func writeDecodeProblem(w http.ResponseWriter, r *http.Request, err error) {
	var (
		tooLarge  *http.MaxBytesError
		syntax    *json.SyntaxError
		wrongType *json.UnmarshalTypeError
		detail    string
	)
	switch {
	case errors.As(err, &tooLarge):
		hal.WriteProblem(w, http.StatusRequestEntityTooLarge, "Request body too large",
			fmt.Sprintf("the request body exceeds %d bytes", tooLarge.Limit), r.URL.Path)
		return
	case errors.As(err, &syntax):
		detail = fmt.Sprintf("the request body is not valid JSON at byte %d: %v", syntax.Offset, err)
	case errors.As(err, &wrongType):
		detail = fmt.Sprintf("the request body field %q has the wrong type at byte %d: want %s, got JSON %s",
			wrongType.Field, wrongType.Offset, wrongType.Type, wrongType.Value)
	case errors.Is(err, io.EOF):
		detail = "the request body is empty; a JSON value is required"
	case errors.Is(err, io.ErrUnexpectedEOF):
		detail = "the request body ends before its JSON value does"
	default:
		detail = err.Error()
	}
	hal.WriteProblem(w, http.StatusBadRequest, "Invalid request body", detail, r.URL.Path)
}
