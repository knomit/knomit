DROP TABLE IF EXISTS commit_fp;
DROP TABLE IF EXISTS path_change_links;
DROP INDEX IF EXISTS path_changes_depth;
DROP TABLE IF EXISTS path_changes;
DELETE FROM meta WHERE key = 'path_changes_version';
