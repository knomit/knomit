// Vector embedding utilities: binary encoding/decoding of float32 vectors,
// pairwise cosine distance computation via sqlite-vec, and one-time
// registration of the sqlite-vec extension.
package store

import (
	"encoding/binary"
	"fmt"
	"math"
	"sync"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
)

var vecOnce sync.Once

// registerVec loads the sqlite-vec extension exactly once per process.
func registerVec() {
	vecOnce.Do(func() { sqlite_vec.Auto() })
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

// PairwiseDistances computes the cosine distance matrix for the given paths
// using SQLite vec_distance_cosine. Returns an NxN matrix where dist[i][j] is
// the cosine distance between paths[i] and paths[j]. Paths without embeddings
// are excluded; the returned paths slice contains only those with embeddings,
// and the matrix indices correspond to the returned paths.
func (idx *Index) PairwiseDistances(paths []string) (retPaths []string, dist [][]float64, err error) {
	// Load rowids for paths that have embeddings.
	type entry struct {
		path  string
		rowid int64
	}
	var entries []entry
	for _, p := range paths {
		var rowid int64
		err := idx.db.QueryRow(
			`SELECT f.rowid FROM facts f JOIN facts_vec fv ON fv.rowid = f.rowid WHERE f.path = ?`,
			p,
		).Scan(&rowid)
		if err != nil {
			continue // no embedding for this path
		}
		entries = append(entries, entry{p, rowid})
	}

	n := len(entries)
	if n == 0 {
		return nil, nil, nil
	}

	// Load embedding blobs once, keyed by rowid.
	blobs := make(map[int64][]byte, n)
	for _, e := range entries {
		var blob []byte
		if err := idx.db.QueryRow(
			`SELECT embedding FROM facts_vec WHERE rowid = ?`, e.rowid,
		).Scan(&blob); err != nil {
			continue
		}
		blobs[e.rowid] = blob
	}

	// Filter to only entries with blobs.
	var filtered []entry
	for _, e := range entries {
		if _, ok := blobs[e.rowid]; ok {
			filtered = append(filtered, e)
		}
	}
	entries = filtered
	n = len(entries)
	if n == 0 {
		return nil, nil, nil
	}

	// Compute pairwise distances via vec_distance_cosine.
	retPaths = make([]string, n)
	for i, e := range entries {
		retPaths[i] = e.path
	}

	dist = make([][]float64, n)
	for i := range dist {
		dist[i] = make([]float64, n)
	}

	stmt, err := idx.db.Prepare(`SELECT vec_distance_cosine(?, ?)`)
	if err != nil {
		return nil, nil, fmt.Errorf("prepare vec_distance_cosine: %w", err)
	}
	defer stmt.Close()

	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			var d float64
			if err := stmt.QueryRow(blobs[entries[i].rowid], blobs[entries[j].rowid]).Scan(&d); err != nil {
				return nil, nil, fmt.Errorf("vec_distance_cosine(%s, %s): %w", entries[i].path, entries[j].path, err)
			}
			dist[i][j] = d
			dist[j][i] = d
		}
	}

	return retPaths, dist, nil
}
