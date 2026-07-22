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

-- Drop the typed property tables. The extension routed values into these by
-- SQLite storage class; knomit stores every property as TEXT because no graph
-- read does a numeric or range comparison — confidence and sources are written
-- but never read back, and `deleted` is compared against 'true'/'false'. Any
-- rows here are stale and unreachable; graph_schema_version is bumped in the
-- same change, so the next Rebuild regenerates the graph from git regardless.
DROP TABLE IF EXISTS node_props_int;
DROP TABLE IF EXISTS node_props_real;
DROP TABLE IF EXISTS node_props_bool;
DROP TABLE IF EXISTS node_props_json;
DROP TABLE IF EXISTS edge_props_int;
DROP TABLE IF EXISTS edge_props_real;
DROP TABLE IF EXISTS edge_props_bool;
DROP TABLE IF EXISTS edge_props_json;
