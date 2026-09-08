-- A row here means the repo is a SUBSCRIPTION: it follows its origin's
-- upstream branch read-only, has no agent branch, and never pushes.
-- Presence table rather than a column on repo_origins because the control
-- chain must stay replayable with IF NOT EXISTS only (migrate-registry runs
-- the whole schema text inside its own transaction).
CREATE TABLE IF NOT EXISTS repo_subscriptions (
    repo_uid   TEXT PRIMARY KEY REFERENCES repos(uid) ON DELETE CASCADE,
    created_at INTEGER NOT NULL
);
