-- Make MERGE semantics a schema guarantee rather than a convention.
--
-- graphMergeEdge is idempotent only because it is ONE statement
-- (INSERT ... WHERE NOT EXISTS). Nothing in the schema enforced that: splitting
-- it back into a SELECT then an INSERT — which is exactly how it was written
-- originally — silently reintroduces duplicate edges under concurrent writers,
-- and the only thing that would catch it is a test someone has to remember to
-- keep. This index makes the bug impossible to reintroduce.
--
-- DERIVED_FROM is excluded by the partial predicate: it is deliberately a
-- multi-edge, one per ref-event, distinguished only by its source_commit /
-- target_commit edge properties, and it carries its own property-aware dedup
-- guard in derived_from.go.
--
-- Collapse any pre-existing duplicates first, so creating the index cannot fail
-- on real data. Failing loudly here would not be an audit: a migration error
-- leaves schema_migrations.dirty = 1, which knomit never clears, so the repo is
-- dropped from the API behind a single WARN (#33) and the graph_schema_version
-- 4 -> 5 Rebuild that would have regenerated a clean graph can never run — it
-- needs a store that opened. The failure is also deterministic, not a torn
-- write, so #33's re-run-the-migration recovery would re-fail every boot.
--
-- Deleting is safe: these edges are derived data that the version-5 Rebuild
-- regenerates from git wholesale, and only DERIVED_FROM carries edge properties
-- (graphSetEdgeProps has exactly one caller, in derived_from.go), so keeping the
-- lowest id of each (source_id, target_id, type) group loses nothing.
DELETE FROM edges
WHERE type <> 'DERIVED_FROM'
  AND id NOT IN (
      SELECT MIN(id) FROM edges
      WHERE type <> 'DERIVED_FROM'
      GROUP BY source_id, target_id, type
  );

CREATE UNIQUE INDEX IF NOT EXISTS ux_edges_merge_identity
    ON edges(source_id, target_id, type)
    WHERE type <> 'DERIVED_FROM';
