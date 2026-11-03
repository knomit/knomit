package index

// The vec0 KNN reads that join facts_vec to other tables. Each is written so
// the vec0 virtual table is the OUTER loop: `facts_vec fv CROSS JOIN …`, which
// SQLite treats as a fixed join order (left table outer).
//
// Written as a plain JOIN, SQLite planned these as `SCAN f USING COVERING
// INDEX …` outer and re-ran the whole vec0 KNN once per facts row (#725). On
// graphBuildSimilarityEdges, which runs for every indexed fact, that made each
// write cost (facts rows) × one KNN, and a batch of n facts quadratic: 501
// facts in one commit took 9 min on Windows, against about 6 s with vec0 outer.
//
// The join order changes the plan only, never the rows: vec0 applies its k
// window before the join and before every WHERE filter either way (gotcha
// fbb0d36d), so k and the overfetch stay exactly as they were.
//
// TestVecKNN_JoinOrderKeepsVec0Outer (store package) EXPLAINs each of these
// exact strings, through VecKNNQueriesForTest.

// similarityKNNSQL is the neighbour read of graphBuildSimilarityEdges and
// rebuildGraph: the knnK+1 nearest fact versions to one embedding (self
// included, skipped by the caller). Args: vecBlob, k.
const similarityKNNSQL = `SELECT f.path, f.blob_hash, (1.0 - fv.distance) as similarity
	 FROM facts_vec fv
	 CROSS JOIN facts f ON f.id = fv.rowid
	 WHERE fv.embedding MATCH ? AND fv.k = ?
	 ORDER BY fv.distance ASC`

// searchByPathKNNSQL is search's QueryByPath read: the KNN of the stored vector
// of (branch, path), resolved by the scalar subquery in MATCH, filtered to the
// branch's live facts. Args: branchID, branchID, path, k.
const searchByPathKNNSQL = `SELECT f.path, (1.0 - fv.distance) as similarity
	 FROM facts_vec fv
	 CROSS JOIN facts f ON f.id = fv.rowid
	 CROSS JOIN branch_facts bf ON bf.fact_id = f.id AND bf.branch_id = ?
	 WHERE fv.embedding MATCH (
	     SELECT fv2.embedding
	     FROM branch_facts bf2
	     JOIN facts_vec fv2 ON fv2.rowid = bf2.fact_id
	     WHERE bf2.branch_id = ? AND bf2.path = ?
	 ) AND fv.k = ?
	 ORDER BY fv.distance ASC`

// searchKNNSQL is search's vector read: the KNN of a query vector, filtered to
// the branch's live facts. Args: branchID, vecBlob, k.
const searchKNNSQL = `SELECT f.path, (1.0 - fv.distance) as similarity
	 FROM facts_vec fv
	 CROSS JOIN facts f ON f.id = fv.rowid
	 CROSS JOIN branch_facts bf ON bf.fact_id = f.id AND bf.branch_id = ?
	 WHERE fv.embedding MATCH ? AND fv.k = ?
	 ORDER BY fv.distance ASC`

// methodologyKNNSQL is RelevantMethodologyForFact's read: every methodology
// fact on the branch at or above a vector floor (k is the whole table, see
// there). Args: branchID, vecBlob, k, minVec.
const methodologyKNNSQL = `SELECT f.id, (1.0 - fv.distance) as similarity
	 FROM facts_vec fv
	 CROSS JOIN facts f ON f.id = fv.rowid
	 CROSS JOIN branch_facts bf ON bf.fact_id = f.id AND bf.branch_id = ?
	 WHERE fv.embedding MATCH ? AND fv.k = ? AND f.type = 'methodology'
	   AND (1.0 - fv.distance) >= ?`
