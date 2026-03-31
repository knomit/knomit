package store

import (
	"context"
	"testing"
)

func TestConn_ReturnsDBWithoutTx(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	ctx := context.Background()
	c := conn(ctx, idx.db)
	if c != idx.db {
		t.Error("expected conn to return db when no tx in context")
	}
}

func TestConn_ReturnsTxFromContext(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	tx, err := idx.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	ctx := WithTx(context.Background(), tx)
	c := conn(ctx, idx.db)
	if c != tx {
		t.Error("expected conn to return tx from context")
	}
}

func TestBeginTxIfNeeded_CreatesNewTx(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	ctx := context.Background()
	ctx2, tx, ownTx, err := beginTxIfNeeded(ctx, idx.db)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	if !ownTx {
		t.Error("expected ownTx=true when no tx in context")
	}
	if TxFromContext(ctx2) != tx {
		t.Error("expected tx to be stored in returned context")
	}
}

func TestBeginTxIfNeeded_ReusesExistingTx(t *testing.T) {
	idx, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	tx, err := idx.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	ctx := WithTx(context.Background(), tx)
	_, tx2, ownTx, err := beginTxIfNeeded(ctx, idx.db)
	if err != nil {
		t.Fatal(err)
	}
	if ownTx {
		t.Error("expected ownTx=false when tx already in context")
	}
	if tx2 != tx {
		t.Error("expected same tx to be returned")
	}
}
