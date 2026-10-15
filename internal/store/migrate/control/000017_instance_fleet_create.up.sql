-- A fleet create that is under way (repos/fleet_create.go). The create
-- writes this row BEFORE its first write (the root key) and deletes it in
-- the same transaction that sets the fleet state, after the last step. A row
-- that is still here when nothing is creating (at boot, or at the next
-- create or registration) is a create that died part way, and it is rolled
-- back as a whole: the repository it names, the forge branch it pushed, the
-- trust material it installed, its root directory. The rollback deletes the
-- row last, so a rollback that fails part way is retried.
--
-- repo_name is the fleet repository's local name, free when the create
-- began. root_id is the fingerprint (pki.RootID) of the root the create
-- minted, set once the root exists. tls_dir_existed records whether [tls].dir
-- existed before the create, so that the rollback removes the directory only
-- if the create made it.
--
-- A separate table because the control chain admits only CREATE ... IF NOT
-- EXISTS (TestControl_UpMigrationsAreIdempotentDDL), and instance_identity's
-- fleet_state CHECK cannot gain a state without rebuilding that table.
--
-- NOTE, the same one 000005, 000013 and 000016 carry: the idempotency guard
-- splits this file on the semicolon BEFORE it strips comments, so a semicolon
-- anywhere in this prose reads as a statement break. Do not put one here.
CREATE TABLE IF NOT EXISTS instance_fleet_create (
    id              INTEGER PRIMARY KEY CHECK (id = 1),
    repo_name       TEXT    NOT NULL,
    started_at      INTEGER NOT NULL,
    root_id         TEXT    NOT NULL DEFAULT '',
    tls_dir_existed INTEGER NOT NULL
);
