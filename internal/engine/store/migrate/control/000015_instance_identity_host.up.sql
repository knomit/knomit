-- The instance's host label, persisted with its identity (D18). It is the
-- sanitized hostname the identity was created with, stored ONCE when the
-- instance_identity row is written and read ever after: the trigger
-- placeholder {host} reads it, and nothing recovers a host by trimming the
-- fingerprint off the agent branch any more.
--
-- A separate table rather than a column on instance_identity because the
-- control chain admits only CREATE ... IF NOT EXISTS
-- (TestControl_UpMigrationsAreIdempotentDDL). An identity created before
-- this table has no row here: its host is unknown (empty), and a trigger
-- naming {host} is invalid on that instance. There is nothing to backfill:
-- the host is never parsed back out of a name.
--
-- NOTE, the same one 000005 and 000013 carry: the idempotency guard splits
-- this file on the semicolon BEFORE it strips comments, so a semicolon
-- anywhere in this prose reads as a statement break. Do not put one here.
CREATE TABLE IF NOT EXISTS instance_identity_host (
    id   INTEGER PRIMARY KEY CHECK (id = 1),
    host TEXT    NOT NULL
);
