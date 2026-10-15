-- WHICH repository is this instance's fleet repository, recorded by the fleet
-- code when it mounts one (RegisterFleet). Boot admits an ontology with id
-- fleet ONLY for this repository: any other registered repo whose ontology
-- is or becomes the fleet's (an upstream switch that arrived by sync, a home
-- from an older build) is refused at Identify with the reserved-id error and
-- listed unavailable, and stays archivable and purgeable. It extends the
-- fleet state machine in instance_identity, whose 000012 comment said the
-- fleet repository was NOT stored. The repository's ontology id is still how
-- a MOUNTED repo is recognised as the fleet (fleetRepo).
--
-- A separate table rather than a column on instance_identity because the
-- control chain admits only CREATE ... IF NOT EXISTS
-- (TestControl_UpMigrationsAreIdempotentDDL). Purging the repository deletes
-- its repos row, and the foreign key takes this one with it.
--
-- NOTE, the same one 000005 and 000013 carry: the idempotency guard splits
-- this file on the semicolon BEFORE it strips comments, so a semicolon
-- anywhere in this prose reads as a statement break. Do not put one here.
CREATE TABLE IF NOT EXISTS instance_fleet_repo (
    id       INTEGER PRIMARY KEY CHECK (id = 1),
    repo_uid TEXT    NOT NULL REFERENCES repos(uid) ON DELETE CASCADE
);
