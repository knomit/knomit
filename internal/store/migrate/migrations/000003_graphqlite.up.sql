-- Historical no-op.
--
-- This migration was `SELECT cypher('RETURN 1')`, whose only purpose was to
-- trigger the GraphQLite extension into lazily creating the property-graph
-- tables. The extension is gone and those tables are now declared explicitly in
-- 000014_graph_eav. The file is retained (rather than deleted) because
-- golang-migrate tracks applied versions by number: removing it would leave
-- already-migrated databases pointing at a version that no longer exists.
SELECT 1;
