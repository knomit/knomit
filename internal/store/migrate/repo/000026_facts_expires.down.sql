DROP INDEX IF EXISTS facts_expires_at;
ALTER TABLE facts DROP COLUMN expires_at;
ALTER TABLE facts DROP COLUMN expires;
