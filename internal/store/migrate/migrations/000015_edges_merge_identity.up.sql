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
-- Creating this fails loudly if a database already holds duplicates. That is
-- intended — it is a one-off audit of every existing deployment.
CREATE UNIQUE INDEX IF NOT EXISTS ux_edges_merge_identity
    ON edges(source_id, target_id, type)
    WHERE type <> 'DERIVED_FROM';
