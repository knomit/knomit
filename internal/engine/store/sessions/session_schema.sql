-- Ephemeral session database schema.
--
-- This file is NOT a versioned migration. The session DB is a separate,
-- disposable SQLite file recreated empty on every Service.Open (see
-- initSchema in db.go), so there is no prior data to migrate and no
-- version history to honor — the whole file is exec'd wholesale against the
-- fresh DB. Schema changes are plain edits here; they take effect on next start.
--
-- It holds only in-flight, process-runtime session/work-queue state that is NOT
-- derivable from git and NOT meaningfully durable across restarts. Durable
-- progress (pipeline_watermarks) deliberately stays in the main DB.

-- Tool sessions: paginated browsing/explain/explore/query cursors.
CREATE TABLE tool_sessions (
    id           TEXT PRIMARY KEY,
    tool         TEXT NOT NULL,
    branch       TEXT NOT NULL,
    path_prefix  TEXT NOT NULL DEFAULT '',
    binding      TEXT NOT NULL DEFAULT '',   -- binding IDENTITY the cursor is frozen to: `repo:<uid>` or `lens:<uid>` (Binding.PinID, lenses RFC §7.3) — NOT the display name, so a repo/lens rename leaves an in-flight cursor resumable; resume through another binding is rejected
    read_set     TEXT NOT NULL DEFAULT '',   -- canonical read-set fingerprint (id12@branch,… sorted) the cursor is frozen to; resume against a re-pinned read set is rejected, indistinguishably from expiry (lenses RFC §7.3)
    owner        TEXT NOT NULL DEFAULT '',   -- WHO may resume it, indistinguishably from expiry for anyone else: for a query/explain/single-fact cursor the endpoint or route it was minted on plus the caller's principal (mcp.callerOwner); for a FactSet the principal ALONE (materialize.FactSetOwner), so one principal reaches it from REST and every MCP endpoint with the same binding
    last_commit  TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'active',
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    last_used_at TEXT NOT NULL              -- idle-reap heartbeat (bumped on every page)
);

CREATE TABLE tool_seen_paths (
    session_id TEXT NOT NULL REFERENCES tool_sessions(id) ON DELETE CASCADE,
    path       TEXT NOT NULL,
    PRIMARY KEY (session_id, path)
);

CREATE TABLE tool_queue (
    session_id  TEXT NOT NULL REFERENCES tool_sessions(id) ON DELETE CASCADE,
    path        TEXT NOT NULL,
    commit_hash TEXT NOT NULL,
    blob        TEXT NOT NULL DEFAULT '',     -- the version's blob hash when the producer knows it (knomit_query: the hit's blob)
    sort_key    INTEGER NOT NULL DEFAULT 0,   -- SQL-orderable consume order (was `depth`)
    state       TEXT NOT NULL DEFAULT '',     -- per-item JSON payload (query: frozen snippet)
    PRIMARY KEY (session_id, path, commit_hash)
);

CREATE INDEX tool_queue_session_sort ON tool_queue(session_id, sort_key);

-- A materialized search (internal/corpus/materialize FactSet): its FROZEN
-- inputs, written ONCE at creation in the same transaction as its
-- tool_sessions row (which carries owner, binding pin and read set, and is
-- what the idle reaper and an explicit close delete, cascading here). An
-- owner may hold any number of FactSets. JSON: the search, the
-- options, the anchor, the frozen tips of the reachable repos and the roots as
-- (repo, path, commit, blob). Never updated: the reading position travels in
-- the cursor token, and the walk's state is an in-memory cache rebuilt by
-- replay.
CREATE TABLE factsets (
    session_id TEXT PRIMARY KEY REFERENCES tool_sessions(id) ON DELETE CASCADE,
    frozen     BLOB NOT NULL
);
CREATE INDEX tool_sessions_last_used ON tool_sessions(last_used_at);

-- Pipeline work-stealing sessions (review/distill/reflect/hypothesize).
CREATE TABLE pipeline_sessions (
    id           TEXT PRIMARY KEY,
    tool         TEXT NOT NULL,
    branch       TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'active',
    phase        TEXT NOT NULL DEFAULT 'work',
    scoped       INTEGER NOT NULL DEFAULT 0, -- 1 when session was started with a scope filter
    -- WHO opened this session, as a CORRELATION HANDLE — emphatically NOT an
    -- identity and NOT authentication (knomit#123). Over MCP the value derives
    -- from the caller's own `Mcp-Session-Id` header, which this server does
    -- not verify: a fabricated id is accepted and runs. The MCP specification
    -- is explicit that a session id "is not evidence of who the caller is" and
    -- MUST NOT be treated as authentication. Read this column as "the opening
    -- call said it was this", never as "this caller was".
    --
    -- Scheme-prefixed so the KIND of claim is visible in the value itself:
    -- `mcp-session:<id>`, plus ` client:<name>/<version>` when the caller ran
    -- `initialize` against this process. Empty for in-process callers (tests,
    -- local tools) — honest absence, not "unknown".
    --
    -- Its worth is joinability: the same id is logged as `mcp_session` by the
    -- HTTP slow-request line, so within the idle-reap window
    -- (pipeline_idle_ttl, 1h) an unexpected session is one query rather than a
    -- forensic pass over checkpointed DB copies.
    created_by   TEXT NOT NULL DEFAULT '',
    -- What a start must match to RESUME this session instead of being refused:
    -- the opening call's effort and scope, canonicalised by the engine. Empty
    -- for sessions opened without a resume policy (in-process runs), which
    -- therefore never match.
    start_key    TEXT NOT NULL DEFAULT '',
    -- 1 from every create until its start has planned the work and rendered
    -- the first item. A session still planning is never resumed and never stale by the
    -- resume window: its queue is empty only because nothing is queued yet.
    planning     INTEGER NOT NULL DEFAULT 0,
    -- 1 from a won phase advance until the strategy's phase hook has queued
    -- the new phase's work. The next advance and completion wait on it: until
    -- then the new phase only LOOKS empty.
    advancing    INTEGER NOT NULL DEFAULT 0,
    -- Running work-item stat totals. They live on the row, not in memory, because
    -- the engine is per-call stateless: the MCP handler builds a fresh Reviewer
    -- for every continue call, so nothing accumulated on that struct survives.
    -- Each apply adds its counts here; completion reads them back as the summary.
    stat_pruned      INTEGER NOT NULL DEFAULT 0,
    stat_merged      INTEGER NOT NULL DEFAULT 0,
    stat_updated     INTEGER NOT NULL DEFAULT 0,
    stat_synthesized INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    last_used_at TEXT NOT NULL              -- idle-reap heartbeat (bumped on work-item access)
);

CREATE TABLE pipeline_work_items (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id  TEXT NOT NULL REFERENCES pipeline_sessions(id) ON DELETE CASCADE,
    step_type   TEXT NOT NULL,
    cluster_key TEXT NOT NULL,
    facts_json  TEXT NOT NULL,
    response    TEXT,
    -- 1 from the claim (response set) until the claimant's apply returns. An
    -- item being applied is still outstanding: its apply may enqueue follow-up
    -- items, so the session's phase must not advance past it.
    applying    INTEGER NOT NULL DEFAULT 0,
    priority    REAL NOT NULL,
    depth       INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL
);

CREATE INDEX pipeline_sessions_last_used ON pipeline_sessions(last_used_at);
-- The outstanding-item probes: the next unanswered item, the item being
-- applied, and the phase advance's NOT EXISTS guard.
CREATE INDEX pipeline_work_items_session_state ON pipeline_work_items(session_id, response, applying);
CREATE INDEX pipeline_sessions_slot ON pipeline_sessions(tool, branch, status);
