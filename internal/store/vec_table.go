package store

import (
	"context"
	"database/sql"
	"fmt"
)

// defaultVecDim is the dimension facts_vec is created at when no embedder is
// configured yet (e.g. immediately after Open, before SetEmbedder, or on a
// deployment running with embeddings disabled). The table must exist so the
// facts_after_delete trigger and the upsert/query paths that reference
// facts_vec keep working even with no embedder. It is created EMPTY and is
// recreated at the real model dim by ensureFactsVec once an embedder is
// configured and a rebuild runs. 768 matches the historical static-migration
// dimension.
const defaultVecDim = 768

// The two code-managed vec0 tables. Both are keyed by facts.id and both are
// created at the ACTIVE model's dimension, which is why neither can be a static
// migration: a vec0 table fixes its width at CREATE and cannot be ALTERed.
//
//   - factsVecTable holds the blended (title+body) document vector every fact
//     is indexed with.
//   - titlesVecTable holds the ABSTRACTION AXIS: the title embedded alone. It
//     is filled lazily by the review pipeline, never by the write path.
const (
	factsVecTable  = "facts_vec"
	titlesVecTable = "fact_titles_vec"
)

// ensureFactsVecDefault creates facts_vec at defaultVecDim if it does not
// already exist, without consulting or writing the embed identity meta. Called
// from Open so the table is present from the moment the DB is migrated (the
// 000009 migration drops the old static table), independent of whether an
// embedder is ever configured. A no-op when the table already exists, so it
// never clobbers a table that ensureFactsVec created at a real model dim.
func (si *searchIndex) ensureFactsVecDefault(ctx context.Context) error {
	for _, table := range []string{factsVecTable, titlesVecTable} {
		exists, err := si.vecTableExists(ctx, table)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if err := si.createVecTable(ctx, table, defaultVecDim); err != nil {
			return err
		}
	}
	return nil
}

// createVecTable creates one code-managed vec0 table at exactly `dim`. It is
// the single source of their DDL, shared by ensureFactsVecDefault and
// ensureFactsVec so the column spec (width, distance metric) can never drift
// between the create-on-Open and recreate-on-rebuild paths. `table` is one of
// the constants above and `dim` comes from the trusted model registry (or
// defaultVecDim), so formatting both into DDL is safe.
func (si *searchIndex) createVecTable(ctx context.Context, table string, dim int) error {
	if _, err := conn(ctx, si.rh.db).ExecContext(ctx,
		fmt.Sprintf(`CREATE VIRTUAL TABLE %s USING vec0(embedding FLOAT[%d] distance_metric=cosine)`, table, dim),
	); err != nil {
		return fmt.Errorf("create %s[%d]: %w", table, dim, err)
	}
	return nil
}

// ensureFactsVec guarantees the facts_vec vec0 table exists at exactly `dim`
// for embedding model `modelID`. If the persisted embedding identity differs
// (different model id OR dim) it drops and recreates the table EMPTY, so the
// subsequent rebuild re-embeds the whole corpus. `dim` comes from the trusted
// model registry, so formatting it into DDL is safe. Meta identity
// (embed_model_id/embed_dim) is persisted by Rebuild on success, not here.
func (si *searchIndex) ensureFactsVec(ctx context.Context, modelID string, dim int) error {
	curID, err := si.persistedEmbedModelID(ctx)
	if err != nil {
		return err
	}
	curDim, err := si.persistedEmbedDim(ctx)
	if err != nil {
		return err
	}
	exists, err := si.vecTableExists(ctx, factsVecTable)
	if err != nil {
		return err
	}
	if exists && curID == modelID && curDim == dim {
		return nil
	}
	// The abstraction axis is embedded by the same model at the same dimension,
	// so it drifts with facts_vec and is recreated by the same mechanism. The
	// restatement caches are derived from the axis — title cosines computed
	// under another model are not comparable — so they go with it.
	if err := si.recreateVecTable(ctx, factsVecTable, dim); err != nil {
		return err
	}
	if err := si.recreateVecTable(ctx, titlesVecTable, dim); err != nil {
		return err
	}
	for _, table := range []string{"restatement_pairs", "restatement_cache_state"} {
		if _, err := conn(ctx, si.rh.db).ExecContext(ctx, `DELETE FROM `+table); err != nil {
			return fmt.Errorf("clear %s on embed-identity change: %w", table, err)
		}
	}
	return nil
}

// recreateVecTable drops table if present and creates it empty at dim.
func (si *searchIndex) recreateVecTable(ctx context.Context, table string, dim int) error {
	exists, err := si.vecTableExists(ctx, table)
	if err != nil {
		return err
	}
	if exists {
		if err := si.dropVecTable(ctx, table); err != nil {
			return err
		}
	}
	return si.createVecTable(ctx, table, dim)
}

func (si *searchIndex) vecTableExists(ctx context.Context, table string) (bool, error) {
	var n int
	err := conn(ctx, si.rh.db).QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name = ?`, table).Scan(&n)
	return n > 0, err
}

func (si *searchIndex) dropVecTable(ctx context.Context, table string) error {
	_, err := conn(ctx, si.rh.db).ExecContext(ctx, `DROP TABLE IF EXISTS `+table)
	return err
}

// persistedEmbedModelID returns meta.embed_model_id ("" if unset).
func (si *searchIndex) persistedEmbedModelID(ctx context.Context) (string, error) {
	var v string
	err := conn(ctx, si.rh.db).QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'embed_model_id'`).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// persistedEmbedDim returns meta.embed_dim (0 if unset).
func (si *searchIndex) persistedEmbedDim(ctx context.Context) (int, error) {
	var v int
	err := conn(ctx, si.rh.db).QueryRowContext(ctx,
		`SELECT value FROM meta WHERE key = 'embed_dim'`).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return v, err
}
