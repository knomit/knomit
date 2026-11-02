package httpx

import "net/http"

// Admits runs gate on r in front of a handler that only records that it was
// reached, and reports whether it was. A refusing gate has written its own
// answer (403, 401) to w, so the caller just returns. It is how a handler
// applies a gate that only some bodies need (a create that names an origin,
// a credential change that links a stored secret), which cannot sit on the
// route because only the body tells the cases apart.
func Admits(gate func(http.Handler) http.Handler, w http.ResponseWriter, r *http.Request) bool {
	admitted := false
	gate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { admitted = true })).ServeHTTP(w, r)
	return admitted
}

// Passes is Admits without an answer: it reports whether gate would admit r
// and writes nothing to the client. For a read that shows a field only to
// callers who pass the gate.
func Passes(gate func(http.Handler) http.Handler, r *http.Request) bool {
	return Admits(gate, discardWriter{h: http.Header{}}, r)
}

type discardWriter struct{ h http.Header }

func (d discardWriter) Header() http.Header         { return d.h }
func (d discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (d discardWriter) WriteHeader(int)             {}
