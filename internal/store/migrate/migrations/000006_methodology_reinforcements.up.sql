-- Records every reinforcement signal: a transition during a review session
-- explained by an existing methodology fact. One row per
-- (methodology, transition) pair from the reflect step's `reinforce` array.
--
-- Why a separate table instead of bumping a counter on the methodology fact
-- file: facts are git-backed; metadata-only commits would churn history.
-- The table also preserves the full provenance graph (which transitions in
-- which session reinforced which methodology) so future retirement /
-- corpus-health tooling has a real signal to query.
CREATE TABLE IF NOT EXISTS methodology_reinforcements (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    branch            TEXT NOT NULL,
    methodology_path  TEXT NOT NULL,
    transition_path   TEXT NOT NULL,
    session_id        TEXT NOT NULL REFERENCES pipeline_sessions(id) ON DELETE CASCADE,
    rationale         TEXT NOT NULL,
    reinforced_at     TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_methodology_reinforcements_path
    ON methodology_reinforcements(branch, methodology_path);

CREATE INDEX IF NOT EXISTS idx_methodology_reinforcements_session
    ON methodology_reinforcements(session_id);
