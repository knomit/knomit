package embeddings_test

import (
	"testing"

	"knomit/internal/embeddings"
)

func TestTokenize(t *testing.T) {
	tok, err := embeddings.LoadTokenizer("testdata/tokenizer.json")
	if err != nil {
		t.Skip("tokenizer.json not available:", err)
	}
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
	tok, err := embeddings.LoadTokenizer("testdata/tokenizer.json")
	if err != nil {
		t.Skip("tokenizer.json not available:", err)
	}
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
	tok, err := embeddings.LoadTokenizer("testdata/tokenizer.json")
	if err != nil {
		t.Skip("tokenizer.json not available:", err)
	}
	// "hello, world" should split "hello" and "," and "world" separately
	ids, _, _ := tok.Encode("hello, world")
	if ids[0] != 101 {
		t.Fatalf("expected [CLS]=101, got %d", ids[0])
	}
	if ids[1] != 7592 {
		t.Fatalf("expected hello=7592, got %d", ids[1])
	}
	// comma should be token 1010 in BERT vocab
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
