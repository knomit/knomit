-- Why an archived repo was archived. One row per archived repo that was
-- archived with a reason, written in the same transaction as the state flip
-- (Registry.Archive) and removed by a SUCCESSFUL restore. An archived repo
-- with NO row here was archived before reasons were recorded, and is shown
-- as "no reason recorded" -- there is nothing to backfill.
--
-- source is user (DELETE /repos/{repo}) or system (a cancelled create, a
-- fleet removal, the boot auto-archive of a repo whose ontology is a
-- symlink). reason is the system's concrete why, note the user's free text,
-- condition the repo's observed state when it was archived (for one archived
-- while unavailable, its reason and detail).
--
-- A table rather than columns on repos because the control chain admits only
-- CREATE ... IF NOT EXISTS (TestControl_UpMigrationsAreIdempotentDDL).
-- Purge deletes the repos row, and the foreign key takes this one with it.
--
-- NOTE, the same one 000005 and 000013 carry: the idempotency guard splits
-- this file on the semicolon BEFORE it strips comments, so a semicolon
-- anywhere in this prose reads as a statement break. Do not put one here.
CREATE TABLE IF NOT EXISTS repo_archive_reasons (
    uid       TEXT PRIMARY KEY REFERENCES repos(uid) ON DELETE CASCADE,
    source    TEXT NOT NULL,
    reason    TEXT NOT NULL DEFAULT '',
    note      TEXT NOT NULL DEFAULT '',
    condition TEXT NOT NULL DEFAULT ''
);
