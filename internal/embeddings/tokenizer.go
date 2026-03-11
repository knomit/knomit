// Package embeddings provides text tokenization for the all-MiniLM-L6-v2 model
// using a BERT WordPiece algorithm.
package embeddings

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// Tokenizer holds the vocab and special token IDs for WordPiece tokenization.
type Tokenizer struct {
	vocab  map[string]int32
	unkID  int32
	clsID  int32
	sepID  int32
	maxLen int
}

// tokenizerJSON mirrors the structure of the tokenizer.json file used by
// all-MiniLM-L6-v2 / HuggingFace tokenizers.
type tokenizerJSON struct {
	Model struct {
		Vocab map[string]int32 `json:"vocab"`
	} `json:"model"`
}

// LoadTokenizer reads a HuggingFace tokenizer.json and returns a Tokenizer.
func LoadTokenizer(path string) (*Tokenizer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load tokenizer: %w", err)
	}

	var tj tokenizerJSON
	if err := json.Unmarshal(data, &tj); err != nil {
		return nil, fmt.Errorf("parse tokenizer: %w", err)
	}

	vocab := tj.Model.Vocab
	if vocab == nil {
		return nil, fmt.Errorf("tokenizer.json: model.vocab is missing")
	}

	get := func(key string) int32 {
		if id, ok := vocab[key]; ok {
			return id
		}
		return -1
	}

	clsID := get("[CLS]")
	sepID := get("[SEP]")
	unkID := get("[UNK]")
	if clsID < 0 || sepID < 0 || unkID < 0 {
		return nil, fmt.Errorf("tokenizer.json: missing special tokens [CLS]/[SEP]/[UNK]")
	}

	return &Tokenizer{
		vocab:  vocab,
		unkID:  unkID,
		clsID:  clsID,
		sepID:  sepID,
		maxLen: 512,
	}, nil
}

// accentStripper is a transform that removes Unicode combining characters
// (category Mn — marks, non-spacing), which are what NFD decomposition
// produces for accents.
type accentStripper struct{}

func (accentStripper) Transform(dst, src []byte, atEOF bool) (nDst, nSrc int, err error) {
	for nSrc < len(src) {
		r, size := rune(src[nSrc]), 1
		if src[nSrc] >= 0x80 {
			r, size = utf8.DecodeRune(src[nSrc:])
		}
		if size > len(src)-nSrc {
			if !atEOF {
				return nDst, nSrc, transform.ErrShortSrc
			}
			size = len(src) - nSrc
		}
		if unicode.Is(unicode.Mn, r) {
			nSrc += size
			continue
		}
		if nDst+size > len(dst) {
			return nDst, nSrc, transform.ErrShortDst
		}
		copy(dst[nDst:], src[nSrc:nSrc+size])
		nDst += size
		nSrc += size
	}
	return nDst, nSrc, nil
}

func (accentStripper) Reset() {}

// normalize lowercases text, applies NFD decomposition, strips combining
// accents (Unicode category Mn), and collapses whitespace — matching the
// JavaScript: text.toLowerCase().normalize("NFD").replace(/[\u0300-\u036f]/g,"")
func normalize(text string) string {
	// Lowercase
	text = strings.ToLower(text)

	// NFD + accent strip using golang.org/x/text
	t := transform.Chain(norm.NFD, accentStripper{}, norm.NFC)
	result, _, err := transform.String(t, text)
	if err != nil {
		// Fall back to NFD without accent stripping on error
		result = norm.NFD.String(text)
	}

	// Collapse whitespace and trim
	fields := strings.Fields(result)
	return strings.Join(fields, " ")
}

// isPunctOrSymbol reports whether r is a Unicode punctuation or symbol
// character, matching the JS regex /[\p{P}\p{S}]/u.
func isPunctOrSymbol(r rune) bool {
	return unicode.IsPunct(r) || unicode.IsSymbol(r)
}

// preTokenize splits normalized text on whitespace then splits each word on
// punctuation/symbol boundaries, exactly as the TypeScript implementation does.
func preTokenize(normalized string) []string {
	var tokens []string
	for _, word := range strings.Split(normalized, " ") {
		if word == "" {
			continue
		}
		current := strings.Builder{}
		for _, ch := range word {
			if isPunctOrSymbol(ch) {
				if current.Len() > 0 {
					tokens = append(tokens, current.String())
					current.Reset()
				}
				tokens = append(tokens, string(ch))
			} else {
				current.WriteRune(ch)
			}
		}
		if current.Len() > 0 {
			tokens = append(tokens, current.String())
		}
	}
	return tokens
}

// wordPiece applies greedy longest-match-first WordPiece tokenization.
// It matches the TypeScript wordPiece function exactly.
func (t *Tokenizer) wordPiece(word string) []int32 {
	runes := []rune(word)
	ids := []int32{}
	start := 0

	for start < len(runes) {
		end := len(runes)
		matched := false

		for start < end {
			var substr string
			if start == 0 {
				substr = string(runes[start:end])
			} else {
				substr = "##" + string(runes[start:end])
			}
			if id, ok := t.vocab[substr]; ok {
				ids = append(ids, id)
				start = end
				matched = true
				break
			}
			end--
		}

		if !matched {
			return []int32{t.unkID}
		}
	}

	return ids
}

// Encode tokenizes text and returns (inputIDs, attentionMask, tokenTypeIDs).
// The sequence is: [CLS] tokens... [SEP], truncated to maxLen (512).
// attentionMask is all 1s; tokenTypeIDs is all 0s.
func (t *Tokenizer) Encode(text string) (inputIDs, attentionMask, tokenTypeIDs []int32) {
	normalized := normalize(text)
	preTokens := preTokenize(normalized)

	ids := []int32{t.clsID}

	for _, token := range preTokens {
		pieces := t.wordPiece(token)
		for _, id := range pieces {
			if len(ids) >= t.maxLen-1 {
				goto done
			}
			ids = append(ids, id)
		}
		if len(ids) >= t.maxLen-1 {
			break
		}
	}

done:
	ids = append(ids, t.sepID)

	mask := make([]int32, len(ids))
	typeIDs := make([]int32, len(ids))
	for i := range ids {
		mask[i] = 1
		// typeIDs[i] is already 0
	}

	return ids, mask, typeIDs
}
