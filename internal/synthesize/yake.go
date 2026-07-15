package synthesize

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// yakeStopwords is a concise English stopword set used by the YAKE extractor.
// Words in this set are never candidates and never contribute to context windows.
var yakeStopwords = map[string]bool{
	// articles, conjunctions, prepositions
	"a": true, "an": true, "the": true, "and": true, "or": true, "but": true,
	"in": true, "on": true, "at": true, "to": true, "for": true, "of": true,
	"with": true, "by": true, "from": true, "as": true, "into": true,
	"through": true, "during": true, "before": true, "after": true,
	"above": true, "below": true, "between": true, "out": true, "off": true,
	"over": true, "under": true, "again": true, "then": true, "once": true,
	"about": true, "against": true, "along": true, "among": true, "around": true,
	"because": true, "since": true, "until": true, "unless": true, "while": true,
	"if": true, "though": true, "when": true, "where": true, "than": true,
	"up": true, "upon": true, "within": true, "without": true, "across": true,
	"near": true, "past": true, "beyond": true, "inside": true, "outside": true,
	"except": true, "despite": true, "per": true, "via": true, "vs": true,
	// pronouns
	"i": true, "me": true, "my": true, "myself": true, "we": true, "our": true,
	"ours": true, "ourselves": true, "you": true, "your": true, "yours": true,
	"yourself": true, "he": true, "him": true, "his": true, "she": true,
	"her": true, "hers": true, "it": true, "its": true, "they": true,
	"them": true, "their": true, "theirs": true, "what": true, "which": true,
	"who": true, "whom": true, "this": true, "that": true, "these": true,
	"those": true, "there": true, "here": true,
	// auxiliary verbs
	"am": true, "is": true, "are": true, "was": true, "were": true, "be": true,
	"been": true, "being": true, "have": true, "has": true, "had": true,
	"do": true, "does": true, "did": true, "will": true, "would": true,
	"could": true, "should": true, "may": true, "might": true, "shall": true,
	"can": true, "need": true, "dare": true, "ought": true,
	// adverbs / quantifiers / misc
	"not": true, "no": true, "nor": true, "so": true, "yet": true,
	"both": true, "either": true, "neither": true, "each": true, "all": true,
	"any": true, "more": true, "most": true, "other": true, "some": true,
	"such": true, "only": true, "own": true, "same": true, "few": true,
	"very": true, "just": true, "also": true, "even": true, "still": true,
	"already": true, "always": true, "never": true, "often": true,
	"now": true, "ever": true, "too": true, "well": true,
	"etc": true, "eg": true, "ie": true, "e": true,
}

const (
	// yakeTopK is the number of top-ranked keywords to extract per fact.
	yakeTopK = 5
	// yakeMaxN is the maximum n-gram length (bigrams).
	yakeMaxN = 2
	// yakeWindow is the left/right context window for co-occurrence counting.
	yakeWindow = 2
	// yakeMinWordLen is the minimum character length for a content word.
	yakeMinWordLen = 3
)

// yakeWordStat accumulates per-word statistics for YAKE scoring.
type yakeWordStat struct {
	display    string              // first-seen authored casing form
	tf         int                 // total frequency (all forms)
	tfCapital  int                 // capitalized (non-sentence-start) occurrences
	sentences  []int               // sentence index of each occurrence
	leftWords  map[string]struct{} // distinct left-context words
	rightWords map[string]struct{} // distinct right-context words
}

// yakeCandidate is a scored keyword phrase.
type yakeCandidate struct {
	display string   // space-joined display form (original casing)
	canon   string   // space-joined lowercase canonical form
	words   []string // canonical component tokens
	score   float64  // YAKE score — lower means more important
}

// yakeExtract returns up to topK YAKE keyword phrases extracted from text.
// Returned strings are canonical (lowercase) forms for use as bridge tokens.
// Returns nil when text is empty or has too few content words.
func yakeExtract(text string, topK int) []string {
	if topK <= 0 {
		return nil
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	// 1. Tokenize into sentences then into (word, sentence-index, position) tuples.
	sentences := yakeSplitSentences(text)

	type tokPos struct {
		canon       string
		display     string
		sentIdx     int
		isSentStart bool
	}
	var tokens []tokPos
	for sIdx, sent := range sentences {
		words := yakeTokenizeWords(sent)
		for wIdx, w := range words {
			canon := strings.ToLower(yakeStripPunct(w))
			display := yakeStripPunct(w)
			if canon == "" {
				continue
			}
			tokens = append(tokens, tokPos{
				canon:       canon,
				display:     display,
				sentIdx:     sIdx,
				isSentStart: wIdx == 0,
			})
		}
	}

	// 2. Accumulate per-word statistics (content words only).
	stats := map[string]*yakeWordStat{}
	for i, t := range tokens {
		if len(t.canon) < yakeMinWordLen || yakeStopwords[t.canon] || yakeIsNumeric(t.canon) {
			continue
		}
		st, ok := stats[t.canon]
		if !ok {
			st = &yakeWordStat{
				display:    t.display,
				leftWords:  map[string]struct{}{},
				rightWords: map[string]struct{}{},
			}
			stats[t.canon] = st
		}
		st.tf++
		if !t.isSentStart && yakeIsCapitalized(t.display) {
			st.tfCapital++
		}
		st.sentences = append(st.sentences, t.sentIdx)

		for j := max(0, i-yakeWindow); j < i; j++ {
			ctx := tokens[j].canon
			if ctx != "" && len(ctx) >= yakeMinWordLen && !yakeStopwords[ctx] {
				st.leftWords[ctx] = struct{}{}
			}
		}
		for j := i + 1; j <= min(len(tokens)-1, i+yakeWindow); j++ {
			ctx := tokens[j].canon
			if ctx != "" && len(ctx) >= yakeMinWordLen && !yakeStopwords[ctx] {
				st.rightWords[ctx] = struct{}{}
			}
		}
	}

	if len(stats) < 2 {
		return nil
	}

	// 3. Compute corpus-level mean and std-dev of word TF for WFrequency.
	tfs := make([]float64, 0, len(stats))
	for _, st := range stats {
		tfs = append(tfs, float64(st.tf))
	}
	mu := yakeMean(tfs)
	sigma := yakeStdDev(tfs, mu)

	numSentences := len(sentences)
	if numSentences == 0 {
		numSentences = 1
	}

	// 4. Score each content word.
	wordScore := make(map[string]float64, len(stats))
	for canon, st := range stats {
		wordScore[canon] = yakeWordScore(st, mu, sigma, numSentences)
	}

	// 5. Extract n-gram candidates from the token stream.
	// A phrase is a maximal run of content tokens (stopwords/short words break it).
	// We register all sub-phrases of length 1..yakeMaxN starting at each position.
	ngramFreq := map[string]int{}
	ngramWords := map[string][]string{}   // canon phrase → canonical words
	ngramDisplay := map[string][]string{} // canon phrase → display words

	for i, t := range tokens {
		if len(t.canon) < yakeMinWordLen || yakeStopwords[t.canon] || yakeIsNumeric(t.canon) {
			continue
		}
		phrase := []tokPos{t}
		for j := i + 1; j < len(tokens) && len(phrase) < yakeMaxN; j++ {
			next := tokens[j]
			if len(next.canon) < yakeMinWordLen || yakeStopwords[next.canon] || yakeIsNumeric(next.canon) {
				break
			}
			phrase = append(phrase, next)
		}
		// Register all sub-lengths.
		for length := 1; length <= len(phrase); length++ {
			sub := phrase[:length]
			canonParts := make([]string, length)
			dispParts := make([]string, length)
			for k, tok := range sub {
				canonParts[k] = tok.canon
				dispParts[k] = tok.display
			}
			key := strings.Join(canonParts, " ")
			ngramFreq[key]++
			if _, exists := ngramWords[key]; !exists {
				ngramWords[key] = canonParts
				ngramDisplay[key] = dispParts
			}
		}
	}

	// Find maximum phrase frequency for normalization.
	maxFreq := 1
	for _, f := range ngramFreq {
		if f > maxFreq {
			maxFreq = f
		}
	}

	// 6. Score candidates. Skip phrases where any component lacks a word score.
	var candidates []yakeCandidate
	for key, words := range ngramWords {
		allScored := true
		for _, w := range words {
			if _, ok := wordScore[w]; !ok {
				allScored = false
				break
			}
		}
		if !allScored {
			continue
		}
		score := yakePhraseScore(words, wordScore, ngramFreq[key], maxFreq)
		candidates = append(candidates, yakeCandidate{
			display: strings.Join(ngramDisplay[key], " "),
			canon:   key,
			words:   words,
			score:   score,
		})
	}

	// 7. Sort ascending (lower score = more important), then dedup dominated candidates.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score != candidates[j].score {
			return candidates[i].score < candidates[j].score
		}
		return candidates[i].canon < candidates[j].canon
	})
	candidates = yakeDedup(candidates)

	// 8. Return top topK canonical forms with word-overlap diversity filter.
	// A candidate is skipped when it shares more than one content word with any
	// already-selected candidate. At yakeMaxN=2 this is equivalent to exact-duplicate
	// filtering (already covered by seen), but becomes meaningful if yakeMaxN is raised.
	seen := map[string]bool{}
	selectedWords := [][]string{}
	result := make([]string, 0, topK)
	for _, c := range candidates {
		if len(result) >= topK {
			break
		}
		if seen[c.canon] {
			continue
		}
		diverse := true
		for _, sw := range selectedWords {
			shared := 0
			for _, w := range c.words {
				for _, s := range sw {
					if w == s {
						shared++
					}
				}
			}
			if shared > 1 {
				diverse = false
				break
			}
		}
		if !diverse {
			continue
		}
		seen[c.canon] = true
		selectedWords = append(selectedWords, c.words)
		result = append(result, c.canon)
	}
	return result
}

// yakeWordScore computes the YAKE score for a single content word.
// Following Campos et al. (2020): lower score = more important.
func yakeWordScore(st *yakeWordStat, meanTF, stdTF float64, numSentences int) float64 {
	// WCase: capitalization ratio — higher when word tends to appear capitalized.
	wCase := float64(st.tfCapital) / (1.0 + math.Log(float64(st.tf)+1.0))
	if wCase < 1e-9 {
		wCase = 1e-9 // avoid division by zero in combined formula
	}

	// WPosition: penalty for appearing late in text — log(log(3 + median sentence index)).
	// Words in sentence 0 (the first) yield log(log(3)) ≈ 0.095; later = higher = worse.
	medSent := float64(yakeMedianInt(st.sentences))
	wPos := math.Log(math.Log(3.0 + medSent))
	if wPos < 1e-9 {
		wPos = 1e-9
	}

	// WFrequency: normalized term frequency.
	denom := meanTF + stdTF
	if denom < 1e-9 {
		denom = 1
	}
	wFreq := float64(st.tf) / denom

	// WRelated: context diversity — more unique neighbours = more general = less important.
	// Formulated as 1 + (leftUniq + rightUniq) * tf so higher diversity inflates the score.
	dl := float64(len(st.leftWords))
	dr := float64(len(st.rightWords))
	wRel := 1.0 + (dl+dr)*float64(st.tf)

	// WDifferentSentence: fraction of distinct sentences containing the word.
	uniqueSents := map[int]struct{}{}
	for _, s := range st.sentences {
		uniqueSents[s] = struct{}{}
	}
	wDiff := float64(len(uniqueSents)) / float64(numSentences)

	// Combined score (lower = more important).
	numerator := wPos * wCase
	denominator := wRel + wFreq/wCase + wDiff/wCase
	if denominator < 1e-9 {
		return 1.0
	}
	return numerator / denominator
}

// yakePhraseScore computes the YAKE multi-word score for a candidate phrase.
// Following the paper: score = Π(S(wi)) / (freq_norm * n * (1 + Σ S(wi))).
// Lower = more important.
func yakePhraseScore(words []string, wordScore map[string]float64, freq, maxFreq int) float64 {
	n := float64(len(words))
	product := 1.0
	sum := 0.0
	for _, w := range words {
		s := wordScore[w]
		product *= s
		sum += s
	}
	freqNorm := float64(freq) / float64(maxFreq)
	divisor := freqNorm * n * (1.0 + sum)
	if divisor < 1e-9 {
		return product
	}
	return product / divisor
}

// yakeDedup removes candidates dominated by a higher-ranked (lower-score)
// longer phrase that contains them as a contiguous sub-sequence.
// Input must already be sorted by score ascending.
func yakeDedup(candidates []yakeCandidate) []yakeCandidate {
	out := make([]yakeCandidate, 0, len(candidates))
	for i, c := range candidates {
		dominated := false
		for j, other := range candidates {
			if i == j || len(other.words) <= len(c.words) {
				continue
			}
			if other.score <= c.score && yakeContainsSubseq(other.words, c.words) {
				dominated = true
				break
			}
		}
		if !dominated {
			out = append(out, c)
		}
	}
	return out
}

// yakeContainsSubseq reports whether super contains sub as a contiguous sub-sequence.
func yakeContainsSubseq(super, sub []string) bool {
	if len(sub) > len(super) {
		return false
	}
	for i := 0; i <= len(super)-len(sub); i++ {
		match := true
		for j := range sub {
			if super[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// yakeSplitSentences splits text into sentences on common terminal punctuation.
func yakeSplitSentences(text string) []string {
	var sentences []string
	current := &strings.Builder{}
	runes := []rune(strings.TrimSpace(text))
	for i, r := range runes {
		current.WriteRune(r)
		if r == '.' || r == '!' || r == '?' {
			next := i + 1
			if next >= len(runes) || runes[next] == ' ' || runes[next] == '\n' {
				s := strings.TrimSpace(current.String())
				if s != "" {
					sentences = append(sentences, s)
				}
				current.Reset()
			}
		} else if r == '\n' {
			s := strings.TrimSpace(current.String())
			if s != "" {
				sentences = append(sentences, s)
			}
			current.Reset()
		}
	}
	if remainder := strings.TrimSpace(current.String()); remainder != "" {
		sentences = append(sentences, remainder)
	}
	if len(sentences) == 0 {
		sentences = []string{text}
	}
	return sentences
}

// yakeTokenizeWords splits a sentence into raw word tokens. Hyphens within
// words are preserved (e.g. "well-known" stays as one token).
func yakeTokenizeWords(s string) []string {
	var words []string
	var cur strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || (r == '-' && cur.Len() > 0) {
			cur.WriteRune(r)
		} else {
			if cur.Len() > 0 {
				words = append(words, cur.String())
				cur.Reset()
			}
		}
	}
	if cur.Len() > 0 {
		words = append(words, cur.String())
	}
	return words
}

// yakeStripPunct trims leading/trailing punctuation from a raw token.
func yakeStripPunct(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// yakeIsCapitalized reports whether s begins with an uppercase letter.
func yakeIsCapitalized(s string) bool {
	for _, r := range s {
		return unicode.IsUpper(r)
	}
	return false
}

// yakeIsNumeric reports whether s consists entirely of digits (and punctuation).
func yakeIsNumeric(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

// yakeMedianInt returns the median of a non-empty int slice.
func yakeMedianInt(vs []int) int {
	if len(vs) == 0 {
		return 0
	}
	cp := make([]int, len(vs))
	copy(cp, vs)
	sort.Ints(cp)
	return cp[len(cp)/2]
}

// yakeMean returns the arithmetic mean of a float64 slice.
func yakeMean(vs []float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	var s float64
	for _, v := range vs {
		s += v
	}
	return s / float64(len(vs))
}

// yakeStdDev returns the population standard deviation of vs.
func yakeStdDev(vs []float64, mean float64) float64 {
	if len(vs) < 2 {
		return 0
	}
	var s float64
	for _, v := range vs {
		d := v - mean
		s += d * d
	}
	return math.Sqrt(s / float64(len(vs)))
}
