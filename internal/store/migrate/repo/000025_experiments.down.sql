-- Symmetric teardown. The indexes go with the table in SQLite, but naming
-- them keeps the down body readable and re-runnable on a partially applied up.
DROP INDEX IF EXISTS experiments_activity;
DROP INDEX IF EXISTS experiments_parent;
DROP TABLE IF EXISTS experiments;
