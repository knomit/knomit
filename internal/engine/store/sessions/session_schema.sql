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
    owner        TEXT NOT NULL DEFAULT '',   -- WHO may resume it: the MCP endpoint it was minted on plus the caller's principal (mcp.callerOwner); resume by another owner is rejected, indistinguishably from expiry
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

-- Book cursors (materialize a search as a book). A book is a tool_sessions
-- row (tool 'materialize-book:<limit>', owner = endpoint + principal) plus
-- its frozen structure here, on disk: every appearance of every fact in book
-- order, with its unit, section number and which appearance is written.
-- Bodies are never stored; a page reads them at each row's frozen commit.
-- Both tables cascade from tool_sessions, so the idle reaper and the
-- one-book-per-owner replacement delete a book in one statement.
CREATE TABLE book_sessions (
    session_id TEXT PRIMARY KEY REFERENCES tool_sessions(id) ON DELETE CASCADE,
    env        TEXT NOT NULL,              -- JSON envelope (title, search, options, counts)
    units      INTEGER NOT NULL DEFAULT 0,
    rows       INTEGER NOT NULL DEFAULT 0,
    next_seq   INTEGER NOT NULL DEFAULT 1, -- the cursor: the row the next page starts at
    ready      INTEGER NOT NULL DEFAULT 0  -- 1 once the structure is complete and placed
);

CREATE TABLE book_rows (
    session_id TEXT NOT NULL REFERENCES tool_sessions(id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,           -- book order, 1..rows
    unit       INTEGER NOT NULL,           -- 1..units (= the result's position)
    n          INTEGER NOT NULL,           -- ordinal inside the unit's own walk
    path       TEXT NOT NULL,              -- wire path: same path = same fact
    room       INTEGER NOT NULL,           -- sections in this appearance's walk subtree
    result     INTEGER NOT NULL DEFAULT 0, -- 1 for a unit root (a search result)
    written    INTEGER NOT NULL DEFAULT 0, -- 1 for the one appearance that carries the body
    number     TEXT NOT NULL,              -- "5", "5.1.2"; "" for a frame ancestor
    tree_depth INTEGER NOT NULL,
    title      TEXT NOT NULL,
    short_id   TEXT NOT NULL,              -- the fact's id8
    anchor     TEXT NOT NULL,              -- this appearance's in-document anchor
    commit_hash TEXT NOT NULL,
    blob       TEXT NOT NULL DEFAULT '',
    state      TEXT NOT NULL,              -- JSON node without body
    PRIMARY KEY (session_id, seq)
);

CREATE INDEX book_rows_unit ON book_rows(session_id, unit, n);
CREATE INDEX book_rows_path ON book_rows(session_id, path, written);
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
