-- F09 PR 5: verification happens once, at the gate that advances main
-- (store.CheckRange). No instance folds history any more, so the per-commit
-- policy cache that fold kept is gone. verify_accepted stays: E4 reads it.
DROP TABLE IF EXISTS verify_context;
