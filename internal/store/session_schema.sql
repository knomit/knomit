-- Ephemeral session database schema.
--
-- This file is NOT a versioned migration. The session DB is a separate,
-- disposable SQLite file recreated empty on every Service.Open (see
-- initSessionSchema in service.go), so there is no prior data to migrate and no
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
    sort_key    INTEGER NOT NULL DEFAULT 0,   -- SQL-orderable consume order (was `depth`)
    state       TEXT NOT NULL DEFAULT '',     -- per-item JSON payload (query: frozen snippet)
    PRIMARY KEY (session_id, path, commit_hash)
);

CREATE INDEX tool_queue_session_sort ON tool_queue(session_id, sort_key);
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
    priority    REAL NOT NULL,
    depth       INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL
);

CREATE INDEX pipeline_sessions_last_used ON pipeline_sessions(last_used_at);
