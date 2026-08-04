-- The authoritative repo registry. Before this table the registry WAS the
-- filesystem (Manager.Start globbed repos/*.db), which returns nothing on an
-- empty disk — so a restored server could not know what repos to recover.
--
-- archive_id replaces repos/archive/<id>.json: an archive record is
-- control-plane state, not a sidecar file with its own failure modes.
CREATE TABLE IF NOT EXISTS repos (
    name          TEXT PRIMARY KEY,
    origin_url    TEXT NOT NULL DEFAULT '',
    origin_branch TEXT NOT NULL DEFAULT '',
    state         TEXT NOT NULL DEFAULT 'active',
    archive_id    TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL DEFAULT 0,
    archived_at   INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_repos_state ON repos(state);
