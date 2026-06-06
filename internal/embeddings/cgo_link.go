// Package embeddings links the daulet/tokenizers static library. The upstream
// package declares -ltokenizers/-ldl but not a search path; this adds the
// project-local dist/lib directory so libtokenizers.a is found at build time.
package embeddings

// #cgo LDFLAGS: -L${SRCDIR}/../../dist/lib
import "C"
