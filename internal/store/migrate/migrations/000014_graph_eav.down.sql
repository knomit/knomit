-- Irreversible in practice: the typed property tables dropped by the up
-- migration held data only the GraphQLite extension could populate, and the
-- graph is a rebuildable cache regenerated from git by Rebuild. Dropping the
-- graph tables is therefore the honest inverse — a downgrade re-creates them
-- via the old extension path and rebuilds.
DROP TABLE IF EXISTS edge_props_text;
DROP TABLE IF EXISTS node_props_text;
DROP TABLE IF EXISTS edges;
DROP TABLE IF EXISTS node_labels;
DROP TABLE IF EXISTS nodes;
DROP TABLE IF EXISTS property_keys;
