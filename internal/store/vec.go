// Vector embedding utilities: binary encoding/decoding of float32 vectors,
// pairwise cosine distance computation via sqlite-vec, and one-time
// registration of the sqlite-vec extension.
package store

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"sync"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	sqlite3 "github.com/mattn/go-sqlite3"
)

var vecOnce sync.Once

// registerVec loads the sqlite-vec extension exactly once per process and
// registers a custom "sqlite3_knomit" driver carrying knomit's SQL functions
// and per-connection pragmas.
func registerVec() {
	vecOnce.Do(func() {
		sqlite_vec.Auto()
		sql.Register("sqlite3_knomit", &sqlite3.SQLiteDriver{
			ConnectHook: func(conn *sqlite3.SQLiteConn) error {
				if err := registerSQLFuncs(conn); err != nil {
					return err
				}
				// Apply per-connection performance pragmas. These must run in
				// ConnectHook so every connection in the pool is configured —
				// db.Exec() only reaches one connection.
				for _, p := range []string{
					"PRAGMA synchronous = NORMAL",
					"PRAGMA cache_size = -65536",   // 64 MB page cache
					"PRAGMA mmap_size = 268435456", // 256 MB memory-mapped I/O
					"PRAGMA temp_store = MEMORY",
				} {
					if _, err := conn.Exec(p, nil); err != nil {
						return err
					}
				}
				return nil
			},
		})
	})
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

// usableKNNSimilarity unwraps a similarity/distance column from a KNN row that
// may be NULL. sqlite-vec returns a NULL distance — hence a NULL similarity —
// for a neighbor with a degenerate, zero-norm embedding, which has no
// meaningful similarity to anything. Because NULL sorts FIRST under
// "ORDER BY distance ASC", callers must `continue` past such rows rather than
// `break` (which would drop every remaining valid hit). Centralizing the
// invariant here keeps all KNN scan sites consistent. Returns the value and
// whether it is usable.
func usableKNNSimilarity(sim sql.NullFloat64) (float64, bool) {
	return sim.Float64, sim.Valid
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

// CosineSim is the cosine similarity of two vectors, and the single definition
// of that formula in this package: the SQL function knomit_cosine_sim decodes
// its blobs and calls straight through to it, so a change here cannot leave the
// two disagreeing.
//
// Returns 0 for mismatched lengths or a degenerate (zero-norm) vector, which
// has no meaningful similarity to anything.
func CosineSim(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		va, vb := float64(a[i]), float64(b[i])
		dot += va * vb
		normA += va * va
		normB += vb * vb
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}
