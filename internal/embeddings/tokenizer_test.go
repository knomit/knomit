package embeddings_test

import (
	"testing"

	"knomit/internal/embeddings"
)

// testVocab returns a minimal BERT-compatible vocab for testing.
func testVocab() map[string]int32 {
	return map[string]int32{
		"[CLS]": 101,
		"[SEP]": 102,
		"[UNK]": 100,
		"hello": 7592,
		"world": 2088,
		",":     1010,
		"cafe":  29295,
	}
}

func mustTokenizer(t *testing.T) *embeddings.Tokenizer {
	t.Helper()
	tok, err := embeddings.NewTokenizer(testVocab(), 512)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestTokenize(t *testing.T) {
	tok := mustTokenizer(t)
	ids, mask, typeIDs := tok.Encode("hello world")
	// Expected: [101, 7592, 2088, 102]
	if ids[0] != 101 {
		t.Fatalf("expected [CLS]=101, got %d", ids[0])
	}
	if ids[1] != 7592 {
		t.Fatalf("expected hello=7592, got %d", ids[1])
	}
	if ids[2] != 2088 {
		t.Fatalf("expected world=2088, got %d", ids[2])
	}
	if ids[3] != 102 {
		t.Fatalf("expected [SEP]=102, got %d", ids[3])
	}
	if len(ids) != len(mask) || len(ids) != len(typeIDs) {
		t.Fatal("lengths don't match")
	}
	for _, m := range mask {
		if m != 1 {
			t.Fatalf("attention_mask: expected 1, got %d", m)
		}
	}
	for _, ty := range typeIDs {
		if ty != 0 {
			t.Fatalf("token_type_ids: expected 0, got %d", ty)
		}
	}
}

func TestWordPieceTruncation(t *testing.T) {
	tok := mustTokenizer(t)
	// Build a very long text to trigger truncation
	long := ""
	for i := 0; i < 600; i++ {
		long += "hello "
	}
	ids, mask, typeIDs := tok.Encode(long)
	if len(ids) > 512 {
		t.Fatalf("expected ids to be truncated to 512, got %d", len(ids))
	}
	if ids[len(ids)-1] != 102 {
		t.Fatalf("expected last token to be [SEP]=102, got %d", ids[len(ids)-1])
	}
	if len(ids) != len(mask) || len(ids) != len(typeIDs) {
		t.Fatal("lengths don't match after truncation")
	}
}

func TestWordPiecePunctuation(t *testing.T) {
	tok := mustTokenizer(t)
	// "hello, world" should split "hello" and "," and "world" separately
	ids, _, _ := tok.Encode("hello, world")
	if ids[0] != 101 {
		t.Fatalf("expected [CLS]=101, got %d", ids[0])
	}
	if ids[1] != 7592 {
		t.Fatalf("expected hello=7592, got %d", ids[1])
	}
	if ids[2] != 1010 {
		t.Fatalf("expected comma=1010, got %d", ids[2])
	}
	if ids[3] != 2088 {
		t.Fatalf("expected world=2088, got %d", ids[3])
	}
	if ids[4] != 102 {
		t.Fatalf("expected [SEP]=102, got %d", ids[4])
	}
}

func TestAccentNormalisation(t *testing.T) {
	tok := mustTokenizer(t)
	// "café" should normalise to "cafe" before tokenization; no [UNK] expected.
	ids, _, _ := tok.Encode("café")
	const unkID = int32(100)
	for _, id := range ids {
		if id == unkID {
			t.Fatalf("unexpected [UNK] token in output for 'café': ids=%v", ids)
		}
	}
	if len(ids) < 2 {
		t.Fatalf("expected at least [CLS] and [SEP], got %v", ids)
	}
}

func TestUNKFallback(t *testing.T) {
	tok := mustTokenizer(t)
	// A token made entirely of characters with no vocab entry forces [UNK].
	ids, _, _ := tok.Encode("\u9fff\u9ffe\u9ffd")
	const unkID = int32(100)
	found := false
	for _, id := range ids {
		if id == unkID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected [UNK]=100 in output for unknown token, got %v", ids)
	}
}

func TestEmptyString(t *testing.T) {
	tok := mustTokenizer(t)
	ids, mask, typeIDs := tok.Encode("")
	if len(ids) != 2 || ids[0] != 101 || ids[1] != 102 {
		t.Fatalf("expected [101, 102], got %v", ids)
	}
	if len(mask) != 2 || mask[0] != 1 || mask[1] != 1 {
		t.Fatalf("expected attention_mask [1, 1], got %v", mask)
	}
	if len(typeIDs) != 2 || typeIDs[0] != 0 || typeIDs[1] != 0 {
		t.Fatalf("expected token_type_ids [0, 0], got %v", typeIDs)
	}
}

func TestNewTokenizerMissingSpecialTokens(t *testing.T) {
	_, err := embeddings.NewTokenizer(map[string]int32{"hello": 1}, 512)
	if err == nil {
		t.Fatal("expected error for missing special tokens")
	}
}

func TestNewTokenizerNilVocab(t *testing.T) {
	_, err := embeddings.NewTokenizer(nil, 512)
	if err == nil {
		t.Fatal("expected error for nil vocab")
	}
}
