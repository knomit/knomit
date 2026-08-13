-- Record the git author NAME alongside the already-stored author_email.
-- The committer differs from the author on PR merges (committer = GitHub) and
-- drops the +operation subaddress for agents, so the AUTHOR is the faithful
-- "who made this commit". See kb/decisions/web/history-api-exposes-author.
ALTER TABLE commit_log ADD COLUMN author_name TEXT NOT NULL DEFAULT '';

-- Best-effort backfill for rows that predate this column. Agent commits encode
-- the agent-id as the local-part before the '+' subaddress
-- (e.g. "agent-7+learn@agents.knomit.io" → name "agent-7"), which equals the
-- author name verbatim. Human / PR-merge commits have no '+' subaddress and
-- their name was never stored, so they stay '' (unrecoverable from email).
UPDATE commit_log
   SET author_name = substr(author_email, 1, instr(author_email, '+') - 1)
 WHERE instr(author_email, '+') > 1;
