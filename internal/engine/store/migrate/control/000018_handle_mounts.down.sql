-- Symmetric teardown. The up-chain admits no DROP (it is replayed wholesale),
-- but a DOWN migration is applied on its own by the versioned migrator.
DROP TABLE IF EXISTS handle_mounts;
