-- Adds the description column that used to be bolted on by a runtime
-- ALTER TABLE in OpenLensRegistry (which swallowed "duplicate column name").
-- On any control.db that already has this column (every install that ran the
-- pre-migration code, whose lensSchema constant already included it), this
-- ALTER collides with the live schema and upWithRecovery's alreadyApplied
-- fallback marks the migration applied rather than failing — the versioned
-- equivalent of the swallowed error the old runtime code relied on.
ALTER TABLE lenses ADD COLUMN description TEXT NOT NULL DEFAULT '';
