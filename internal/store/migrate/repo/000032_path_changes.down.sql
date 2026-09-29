DROP TABLE IF EXISTS path_change_commits;
DROP TABLE IF EXISTS path_change_links;
DROP INDEX IF EXISTS path_changes_entry_blob;
DROP TABLE IF EXISTS path_changes;
DELETE FROM meta WHERE key = 'path_changes_version';
