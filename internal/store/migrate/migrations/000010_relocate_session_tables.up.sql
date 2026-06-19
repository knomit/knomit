-- Relocate all in-flight session/work-queue state to a separate, ephemeral
-- session database (see internal/store/session_schema.sql). These tables hold
-- only process-runtime state that is not derivable from git and not meaningfully
-- durable across restarts, so they no longer belong in the main, git-derived DB.
--
-- pipeline_watermarks is intentionally NOT dropped: it records how far
-- review/hypothesize has processed git history (durable progress), and must
-- survive restarts.
DROP TABLE IF EXISTS tool_seen_paths;
DROP TABLE IF EXISTS tool_queue;
DROP TABLE IF EXISTS tool_sessions;
DROP TABLE IF EXISTS pipeline_work_items;
DROP TABLE IF EXISTS pipeline_sessions;
