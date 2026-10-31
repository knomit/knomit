-- ref_edges: a fact VERSION's references as one flat row per ref, keyed on
-- the citing BLOB, in the order the author wrote them. It carries exactly what
-- a reference walk reads, so one level of a walk is ONE covering range scan
-- per citing blob (primary key (source_blob, ord), WITHOUT ROWID) instead of
-- the EAV join over edges, node_props_text and edge_props_text.
--
-- Derived state, a mirror of the per-blob DERIVED_FROM edges (PR #693): a
-- blob's rows are written in the SAME transaction as its refs_commit claim and
-- its edges (index.applyBlobRefs), replaced with them when the claim moves or
-- a void claim is re-claimed, checked against them by Verify (category
-- ref-edges) and repaired by a rebuild.
--
--   source_blob    the citing version's blob hash (node identity = blob)
--   ord            the ref's 0-based position in the version's frontmatter
--                  refs list (facts.refs / the parsed blob), counting EVERY
--                  ref (URLs and src:// included), so ord equals
--                  json_each(facts.refs).key. Gaps are normal; a target cited
--                  twice keeps its first position. NOT the edge id order.
--   target_repo    '' for a ref into this repo; the other repo's 12-hex id
--                  for a cross-KB ref (kb://<repo-id>/<path>)
--   target_path    repo-relative path of the cited fact
--   target_blob    the pinned target version's blob (the DERIVED_FROM edge's
--                  target node). NULL for a cross-KB ref: it is resolved at
--                  read time against that repo's frozen tip, never here.
--   target_commit  the edge's target_commit (NULL for a cross-KB ref)
--
-- A local row exists only when its DERIVED_FROM edge exists, so only when the
-- source and target blobs are both in `objects` (the real-blob invariant). A
-- local ref that did not resolve has no row, as it has no edge.
--
-- No data backfill here: a row's order needs the parsed blob, which SQL cannot
-- read. index.BackfillRefEdges fills an existing store once (meta
-- ref_edges_backfilled), from the edges and the blobs in `objects`. Until the
-- sentinel is set, outgoing reads fall back to the edges (GraphStore). A store
-- with no Fact node yet (a new store) has nothing to backfill: its sentinel is
-- set here, so its reads use the table from the start.
CREATE TABLE IF NOT EXISTS ref_edges (
    source_blob   TEXT    NOT NULL,
    ord           INTEGER NOT NULL,
    target_repo   TEXT    NOT NULL DEFAULT '',
    target_path   TEXT    NOT NULL,
    target_blob   TEXT,
    target_commit TEXT,
    PRIMARY KEY (source_blob, ord),
    -- A cross-KB row pins no version: the incoming read and the reverse walk
    -- probe idx_ref_edges_target with no target_repo filter, relying on only
    -- local rows carrying a target_blob (PR #694 review R3-N2).
    CHECK (target_repo = '' OR (target_blob IS NULL AND target_commit IS NULL))
) WITHOUT ROWID;

-- The incoming direction (IncomingAtCommit, BlastRadius) probes by the CITED
-- blob: "which blobs cite this version". Anchored by blob alone, as the edge
-- read was (a version is a blob; the path is not part of the match), so the
-- index is (target_blob); being a WITHOUT ROWID table, each entry also carries
-- (source_blob, ord), so the probe never touches the table. Partial: cross-KB
-- rows (target_blob NULL) are never probed this way.
CREATE INDEX IF NOT EXISTS idx_ref_edges_target ON ref_edges(target_blob) WHERE target_blob IS NOT NULL;

INSERT OR IGNORE INTO meta(key, value)
SELECT 'ref_edges_backfilled', '1'
 WHERE NOT EXISTS (SELECT 1 FROM node_labels WHERE label = 'Fact');
