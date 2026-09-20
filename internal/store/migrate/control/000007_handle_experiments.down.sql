-- Symmetric teardown. The up-chain admits no DROP (it is replayed wholesale),
-- but a DOWN migration is applied on its own by the versioned migrator, so the
-- drop belongs here and only here.
DROP TABLE IF EXISTS handle_experiments;
