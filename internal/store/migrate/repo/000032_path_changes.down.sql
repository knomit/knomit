DROP TABLE IF EXISTS commit_log_stage;
DROP TABLE IF EXISTS commit_fp;
DROP TABLE IF EXISTS path_change_links;
DROP TABLE IF EXISTS path_changes;
DELETE FROM meta WHERE key = 'path_changes_version';
