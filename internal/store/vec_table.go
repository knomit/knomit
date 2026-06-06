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

// ensureFactsVecDefault creates facts_vec at defaultVecDim if it does not
// already exist, without consulting or writing the embed identity meta. Called
// from Open so the table is present from the moment the DB is migrated (the
// 000009 migration drops the old static table), independent of whether an
// embedder is ever configured. A no-op when the table already exists, so it
// never clobbers a table that ensureFactsVec created at a real model dim.
func (si *searchIndex) ensureFactsVecDefault(ctx context.Context) error {
	exists, err := si.factsVecExists(ctx)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := conn(ctx, si.rh.db).ExecContext(ctx,
		fmt.Sprintf(`CREATE VIRTUAL TABLE facts_vec USING vec0(embedding FLOAT[%d] distance_metric=cosine)`, defaultVecDim),
	); err != nil {
		return fmt.Errorf("create facts_vec[%d] (default): %w", defaultVecDim, err)
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
	exists, err := si.factsVecExists(ctx)
	if err != nil {
		return err
	}
	if exists && curID == modelID && curDim == dim {
		return nil
	}
	if exists {
		if err := si.dropFactsVec(ctx); err != nil {
			return err
		}
	}
	if _, err := conn(ctx, si.rh.db).ExecContext(ctx,
		fmt.Sprintf(`CREATE VIRTUAL TABLE facts_vec USING vec0(embedding FLOAT[%d] distance_metric=cosine)`, dim),
	); err != nil {
		return fmt.Errorf("create facts_vec[%d]: %w", dim, err)
	}
	return nil
}

func (si *searchIndex) factsVecExists(ctx context.Context) (bool, error) {
	var n int
	err := conn(ctx, si.rh.db).QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='facts_vec'`).Scan(&n)
	return n > 0, err
}

func (si *searchIndex) dropFactsVec(ctx context.Context) error {
	_, err := conn(ctx, si.rh.db).ExecContext(ctx, `DROP TABLE IF EXISTS facts_vec`)
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
