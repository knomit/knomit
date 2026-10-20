package vocab

import (
	"knomit/internal/store/internal/sqlite"
)

// The unexported names below are the ones the vocab files used when they
// lived in package store, where sqlite_conn.go supplied them as one-line
// forwards (the parent keeps its own for the files that stayed). They are
// kept here under the same names so the moved files did not change; each is
// the sqlite child's definition, not a copy of it.
var (
	conn                = sqlite.Conn
	beginTxIfNeeded     = sqlite.BeginTxIfNeeded
	float32SliceToBytes = sqlite.Float32SliceToBytes
	bytesToFloat32Slice = sqlite.BytesToFloat32Slice
	usableKNNSimilarity = sqlite.UsableKNNSimilarity
	groupingKey         = sqlite.GroupingKey
	motifClusterKeyExpr = sqlite.MotifClusterKeyExpr
)
