-- F09 PR 5: the instance's identity, persisted. Before this, the agent branch
-- (agent/<host>-<fp8>) was re-derived from the current key and hostname at
-- every boot, so a key rotation or a hostname change gave the instance a new
-- id and a new branch. Now the id is computed ONCE (first boot on a binary
-- with this table) and read on every later boot. The fp8 in it is inert
-- decoration nothing parses. Recreating control.db is a fresh start: the id
-- is computed again and the instance re-registers.
--
-- The fleet_* columns are this instance's fleet membership state machine
-- (standalone -> registering -> registered -> unregistering -> standalone).
-- WHICH repository is the fleet is not stored: it is the one mounted
-- repository whose ontology is the fleet preset.
CREATE TABLE IF NOT EXISTS instance_identity (
    id                  INTEGER PRIMARY KEY CHECK (id = 1),
    agent_id            TEXT    NOT NULL,
    branch              TEXT    NOT NULL,
    created_at          INTEGER NOT NULL,
    fleet_state         TEXT    NOT NULL DEFAULT 'standalone'
                        CHECK (fleet_state IN ('standalone','registering','registered','unregistering')),
    fleet_state_since   INTEGER,
    fleet_registered_at INTEGER,
    fleet_last_attempt  INTEGER,
    fleet_last_error    TEXT
);
