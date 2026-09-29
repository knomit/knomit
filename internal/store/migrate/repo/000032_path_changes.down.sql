DROP TABLE IF EXISTS commit_log_stage;
DROP TABLE IF EXISTS commit_fp;
DROP TABLE IF EXISTS path_change_links;
DROP TABLE IF EXISTS path_changes;
DROP TABLE IF EXISTS path_changes_next;
DROP TABLE IF EXISTS path_change_links_next;
DROP TABLE IF EXISTS commit_fp_next;
DELETE FROM meta WHERE key IN ('path_changes_version', 'path_changes_shadow_version');
