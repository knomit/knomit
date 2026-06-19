// Package embeddings links the daulet/tokenizers static library. The upstream
// package declares -ltokenizers/-ldl but not a search path; these per-platform
// files add the project's per-platform native-lib dir (dist/<goos>-<goarch>/lib)
// so libtokenizers.a is found at build time. CGO -L paths cannot expand Make or
// env vars, so the path is spelled out literally, one file per supported
// GOOS/GOARCH (the filename suffix constrains the build).
package embeddings

// #cgo LDFLAGS: -L${SRCDIR}/../../dist/darwin-arm64/lib
import "C"
