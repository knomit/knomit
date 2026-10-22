-- Own the property-graph schema outright.
--
-- These tables were previously created as a side effect of the GraphQLite
-- extension's first cypher() call (migration 000003 was literally
-- `SELECT cypher('RETURN 1')`). With the extension gone nothing would create
-- them, so the DDL is declared here, matching the layout the extension used so
-- existing databases are unaffected.
--
-- IF NOT EXISTS throughout: on an already-migrated database the extension
-- created these long ago and this is a no-op; on a fresh database this is what
-- actually builds the graph.

CREATE TABLE IF NOT EXISTS property_keys (
    id  INTEGER PRIMARY KEY AUTOINCREMENT,
    key TEXT UNIQUE NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_property_keys_key ON property_keys(key);

CREATE TABLE IF NOT EXISTS nodes (
    id INTEGER PRIMARY KEY AUTOINCREMENT
);

CREATE TABLE IF NOT EXISTS node_labels (
    node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    label   TEXT NOT NULL,
    PRIMARY KEY (node_id, label)
);
CREATE INDEX IF NOT EXISTS idx_node_labels_label ON node_labels(label, node_id);

CREATE TABLE IF NOT EXISTS edges (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    source_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    target_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    type      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_edges_source ON edges(source_id, type);
CREATE INDEX IF NOT EXISTS idx_edges_target ON edges(target_id, type);
CREATE INDEX IF NOT EXISTS idx_edges_type   ON edges(type);

CREATE TABLE IF NOT EXISTS node_props_text (
    node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    key_id  INTEGER NOT NULL REFERENCES property_keys(id),
    value   TEXT NOT NULL,
    PRIMARY KEY (node_id, key_id)
);
CREATE INDEX IF NOT EXISTS idx_node_props_text_key_value ON node_props_text(key_id, value, node_id);

CREATE TABLE IF NOT EXISTS edge_props_text (
    edge_id INTEGER NOT NULL REFERENCES edges(id) ON DELETE CASCADE,
    key_id  INTEGER NOT NULL REFERENCES property_keys(id),
    value   TEXT NOT NULL,
    PRIMARY KEY (edge_id, key_id)
);
CREATE INDEX IF NOT EXISTS idx_edge_props_text_key_value ON edge_props_text(key_id, value, edge_id);

-- Carry `deleted` across the layout change BEFORE dropping the table it lives
-- in. On a v4 database the extension stored it as INTEGER 0/1 in
-- node_props_bool, and it is the only key ever written there. The direct-SQL
-- readers only see node_props_text, so dropping without converting would make
-- every retracted fact read as live until a Rebuild regenerated the graph —
-- and that Rebuild runs later, in the background, and can fail. Converting
-- here keeps liveness correct for the whole window: this migration is one
-- transaction, so the graph is never in a state where its tombstones are gone.
--
-- The shim exists only so the SELECT below parses on a fresh database that
-- never had the typed tables; there it copies zero rows.
CREATE TABLE IF NOT EXISTS node_props_bool (
    node_id INTEGER NOT NULL,
    key_id  INTEGER NOT NULL,
    value   INTEGER NOT NULL,
    PRIMARY KEY (node_id, key_id)
);
INSERT OR IGNORE INTO node_props_text(node_id, key_id, value)
SELECT node_id, key_id,
       CASE WHEN value IN (1, '1', 'true') THEN 'true' ELSE 'false' END
FROM node_props_bool;

-- Drop the typed property tables. The extension routed values into these by
-- SQLite storage class; knomit stores every property as TEXT because no graph
-- read does a numeric or range comparison — confidence and sources are written
-- but never read back, and `deleted` (converted above) is compared against
-- 'true'/'false'. graph_schema_version is bumped in the same change, so the
-- next Rebuild regenerates the graph from git regardless; the conversion is
-- what keeps reads correct until it does.
DROP TABLE IF EXISTS node_props_int;
DROP TABLE IF EXISTS node_props_real;
DROP TABLE IF EXISTS node_props_bool;
DROP TABLE IF EXISTS node_props_json;
DROP TABLE IF EXISTS edge_props_int;
DROP TABLE IF EXISTS edge_props_real;
DROP TABLE IF EXISTS edge_props_bool;
DROP TABLE IF EXISTS edge_props_json;
