// Vector embedding utilities: binary encoding/decoding of float32 vectors,
// pairwise cosine distance computation via sqlite-vec, and one-time
// registration of the sqlite-vec extension.
package store

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	sqlite3 "github.com/mattn/go-sqlite3"
)

var vecOnce sync.Once

// registerVec loads the sqlite-vec extension exactly once per process and
// registers a custom "sqlite3_knomit" driver that also loads GraphQLite.
func registerVec() {
	vecOnce.Do(func() {
		sqlite_vec.Auto()
		sql.Register("sqlite3_knomit", &sqlite3.SQLiteDriver{
			Extensions: []string{graphqliteLibPath()},
		})
	})
}

// graphqliteLibPath returns the path to the GraphQLite shared library
// without the file extension. The mattn/go-sqlite3 driver appends the
// platform extension (.so, .dylib, .dll) automatically.
//
// Resolution order:
//  1. GRAPHQLITE_LIB_PATH env var (explicit override)
//  2. Path relative to the running executable (production / installed binary)
//  3. Path relative to the source tree (test binaries via runtime.Caller)
func graphqliteLibPath() string {
	if v := os.Getenv("GRAPHQLITE_LIB_PATH"); v != "" {
		return v
	}

	ext := ".so"
	switch runtime.GOOS {
	case "darwin":
		ext = ".dylib"
	case "windows":
		ext = ".dll"
	}
	// The file on disk has the extension, but we return the path without it
	// because the mattn driver appends it automatically.
	fileName := "graphqlite" + ext
	baseName := "graphqlite"

	// Try exe-relative path first (production binaries: dist/lib/).
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		if _, err := os.Stat(filepath.Join(dir, "lib", fileName)); err == nil {
			return filepath.Join(dir, "lib", baseName)
		}
	}

	// Fall back to source-tree-relative path (go test puts the binary in a
	// temp dir, so we use runtime.Caller to find the source file location).
	_, file, _, ok := runtime.Caller(0)
	if ok {
		srcDir := filepath.Join(filepath.Dir(file), "..", "..")
		if _, err := os.Stat(filepath.Join(srcDir, "dist", "lib", fileName)); err == nil {
			return filepath.Join(srcDir, "dist", "lib", baseName)
		}
	}

	// Return a best-effort path; the driver will fail at connection time with
	// a clear error if the library is truly missing.
	exe, _ := os.Executable()
	return filepath.Join(filepath.Dir(exe), "lib", baseName)
}

// float32SliceToBytes encodes a []float32 as little-endian bytes
// (4 bytes per element), the format expected by sqlite-vec.
func float32SliceToBytes(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// bytesToFloat32Slice decodes little-endian bytes back into a []float32.
func bytesToFloat32Slice(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("embedding blob length %d is not a multiple of 4", len(b))
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v, nil
}

