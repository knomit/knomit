package materialize

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"knomit/internal/engine/store/index"
)

// Book sort orders: the two lists the UI's results list shows.
const (
	// SortRelevance is the repo /search route: text ranked by similarity,
	// text-less newest commit first.
	SortRelevance = "relevance"
	// SortRecent is the repo /facts collection with sort=recent: newest
	// commit first (with text, ranked by relevance, recency breaking ties).
	SortRecent = "recent"
)

// BookSearch is the search a book materializes: the same filters as the
// UI's results list (the repo /search and /facts routes), as data. It is
// frozen in the cursor; the book's title and its full-search line are
// computed from it on the server, so REST and MCP answer the same text.
type BookSearch struct {
	Text          string   `json:"text,omitempty"`
	Entities      []string `json:"entities,omitempty"`
	Domains       []string `json:"domains,omitempty"`
	DomainExact   bool     `json:"domain_exact,omitempty"`
	Path          string   `json:"path,omitempty"`
	Types         []string `json:"types,omitempty"`
	ExcludeTypes  []string `json:"exclude_types,omitempty"`
	Kinds         []string `json:"kinds,omitempty"`
	ExcludeKinds  []string `json:"exclude_kinds,omitempty"`
	Origins       []string `json:"origins,omitempty"`
	Episodes      []string `json:"episodes,omitempty"`
	Motifs        []string `json:"motifs,omitempty"`
	MotifMatch    string   `json:"motif_match,omitempty"`
	MinConfidence float64  `json:"min_confidence,omitempty"`
	MinSimilarity float64  `json:"min_similarity,omitempty"`
	SinceFork     bool     `json:"since_fork,omitempty"`
	Sort          string   `json:"sort"`
}

// validate refuses a search the store would misread.
func (s BookSearch) validate() error {
	switch s.Sort {
	case SortRelevance, SortRecent:
	default:
		return fmt.Errorf("%w: sort must be %q or %q, got %q", ErrInvalid, SortRelevance, SortRecent, s.Sort)
	}
	switch index.MotifMatchTier(s.MotifMatch) {
	case "", index.MotifMatchExact, index.MotifMatchStem, index.MotifMatchToken2:
	default:
		return fmt.Errorf(`%w: invalid motif_match value (accepted: "exact", "stem", "token-2")`, ErrInvalid)
	}
	if s.MinSimilarity != 0 {
		if err := index.ValidateMinSimilarity(s.MinSimilarity); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	}
	return nil
}

// options is the store query of s, for limit results.
func (s BookSearch) options(limit int, pathsIn []string) index.SearchOptions {
	tier := index.MotifMatchTier(s.MotifMatch)
	if tier == "" {
		tier = index.MotifMatchExact
	}
	return index.SearchOptions{
		Text: s.Text, Path: s.Path, Entities: s.Entities, Domain: s.Domains, DomainExact: s.DomainExact,
		IncludeTypes: s.Types, ExcludeTypes: s.ExcludeTypes, IncludeKinds: s.Kinds, ExcludeKinds: s.ExcludeKinds,
		IncludeOrigins: s.Origins, EpisodeOps: s.Episodes, Motifs: s.Motifs, MotifMatch: tier,
		MinConfidence: s.MinConfidence, MinSimilarity: s.MinSimilarity, PathsIn: pathsIn, Limit: limit,
	}
}

// BookTitle is the book's mechanical title (design §5):
//   - free text first, as a sentence: capitalized, quotes and a trailing "?"
//     dropped;
//   - then " for X" / " for X and Y" / " for X, Y and N more" for entities;
//   - then the domains in parentheses: " (cost, security)";
//   - no text: the entities lead ("Anthropic (cost)");
//   - domains only: the entity format, capitalized, no parentheses;
//   - with any of text, entities or domains, the other filters stay out of
//     the title (the search line spells them out).
//
// With no text, entities or domains (user ruling, 2026-10-29), the title is
// built from the other filters, in words: fallbackTitle.
//
// root is the ontology root the path is shown under; name is the repo the
// book reads, for "All facts in <name>".
func BookTitle(s BookSearch, root, name string) string {
	text := sentence(s.Text)
	ents := listPhrase(s.Entities)
	switch {
	case text != "":
		t := text
		if ents != "" {
			t += " for " + ents
		}
		return t + domainsSuffix(s.Domains)
	case ents != "":
		return ents + domainsSuffix(s.Domains)
	case len(s.Domains) > 0:
		return capitalize(listPhrase(s.Domains))
	}
	return fallbackTitle(s, root, name)
}

// fallbackTitle is the title of a search with no text, entities or domains
// (user ruling, 2026-10-29), "[head] [about <motifs>] [in <path>]
// [qualifiers]":
//   - head: the types as plurals ("Hypotheses", "Hypotheses and policies"),
//     else the kinds ("Pragmatic facts"), else the origins ("Distilled
//     facts"), else "Facts";
//   - motifs: each slug in words ("misattributed-env-defect" is
//     "misattributed env defect"), listed "A and B" or "A, B and N more";
//   - path: its breadcrumb under the ontology root, "Architecture › fleet";
//   - qualifiers: "changed in this experiment" for since_fork, then
//     "(confidence ≥ 0.8)";
//   - motifs and nothing else: the motif words alone, "Misattributed env
//     defect";
//   - no filter at all: "All facts in <name>".
//
// The title is in sentence case: only its first letter is capitalized.
// Excluded types and kinds, episodes and a similarity floor (inert without
// text) stay out of it; the search line spells them out.
func fallbackTitle(s BookSearch, root, name string) string {
	head := ""
	switch {
	case len(s.Types) > 0:
		plurals := make([]string, len(s.Types))
		for i, t := range s.Types {
			plurals[i] = pluralType(t)
		}
		head = listPhrase(plurals)
	case len(s.Kinds) > 0:
		head = listPhrase(s.Kinds) + " facts"
	case len(s.Origins) > 0:
		head = listPhrase(s.Origins) + " facts"
	}
	motifs := make([]string, len(s.Motifs))
	for i, m := range s.Motifs {
		motifs[i] = strings.Join(strings.FieldsFunc(m, func(r rune) bool { return r == '-' || r == '_' }), " ")
	}
	about := listPhrase(motifs)
	where := pathTitle(s.Path, root)
	var quals []string
	if s.SinceFork {
		quals = append(quals, "changed in this experiment")
	}
	if s.MinConfidence > 0 {
		quals = append(quals, "(confidence ≥ "+strconv.FormatFloat(s.MinConfidence, 'f', -1, 64)+")")
	}
	if head == "" && about != "" && where == "" && len(quals) == 0 {
		return capitalize(about)
	}
	if head == "" && about == "" && where == "" && len(quals) == 0 {
		if name == "" {
			return "All facts"
		}
		return "All facts in " + name
	}
	if head == "" {
		head = "facts"
	}
	parts := []string{head}
	if about != "" {
		parts = append(parts, "about "+about)
	}
	if where != "" {
		parts = append(parts, "in "+where)
	}
	parts = append(parts, quals...)
	return capitalize(strings.Join(parts, " "))
}

// pluralType is a fact type's plural, in words: "hypothesis" is
// "hypotheses", "policy" "policies", "process" "processes".
func pluralType(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	switch {
	case strings.HasSuffix(t, "is"):
		return strings.TrimSuffix(t, "is") + "es"
	case strings.HasSuffix(t, "y") && len(t) > 1 && !strings.ContainsRune("aeiou", rune(t[len(t)-2])):
		return strings.TrimSuffix(t, "y") + "ies"
	case strings.HasSuffix(t, "s"), strings.HasSuffix(t, "x"), strings.HasSuffix(t, "ch"), strings.HasSuffix(t, "sh"):
		return t + "es"
	default:
		return t + "s"
	}
}

// sentence is free text as a title sentence.
func sentence(text string) string {
	t := strings.Map(func(r rune) rune {
		switch r {
		case '"', '“', '”', '„':
			return -1
		}
		return r
	}, text)
	t = strings.Join(strings.Fields(t), " ")
	t = strings.TrimSpace(strings.TrimRight(t, "?"))
	t = strings.Trim(t, "'‘’")
	return capitalize(t)
}

func capitalize(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

// listPhrase is "X", "X and Y", or "X, Y and N more".
func listPhrase(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	default:
		return fmt.Sprintf("%s, %s and %d more", items[0], items[1], len(items)-2)
	}
}

func domainsSuffix(domains []string) string {
	if len(domains) == 0 {
		return ""
	}
	return " (" + strings.Join(domains, ", ") + ")"
}

// pathTitle is a path filter's segments below the ontology root, the first
// capitalized, joined by " › ".
func pathTitle(p, root string) string {
	p = strings.Trim(p, "/")
	if root != "" {
		if rest, ok := strings.CutPrefix(p, strings.Trim(root, "/")); ok && (rest == "" || rest[0] == '/') {
			p = strings.Trim(rest, "/")
		}
	}
	var segs []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	if len(segs) == 0 {
		return ""
	}
	segs[0] = capitalize(segs[0])
	return strings.Join(segs, " › ")
}

// SearchLine spells out the whole search, the filters the title leaves out
// included, in a fixed order, so the reader can see exactly what was
// materialized: `text "…" · entities A, B · domains c · path kb/x/ · …`.
func SearchLine(s BookSearch) string {
	var parts []string
	add := func(label string, vals []string) {
		if len(vals) > 0 {
			parts = append(parts, label+" "+strings.Join(vals, ", "))
		}
	}
	if s.Text != "" {
		parts = append(parts, "text "+strconv.Quote(s.Text))
	}
	add("entities", s.Entities)
	if len(s.Domains) > 0 {
		label := "domains"
		if s.DomainExact {
			label = "domains (exact)"
		}
		add(label, s.Domains)
	}
	if s.Path != "" {
		parts = append(parts, "path "+s.Path)
	}
	add("type", s.Types)
	add("not type", s.ExcludeTypes)
	add("kind", s.Kinds)
	add("not kind", s.ExcludeKinds)
	add("origin", s.Origins)
	add("episode", s.Episodes)
	if len(s.Motifs) > 0 {
		label := "motifs"
		if s.MotifMatch != "" && s.MotifMatch != string(index.MotifMatchExact) {
			label += " (" + s.MotifMatch + ")"
		}
		add(label, s.Motifs)
	}
	if s.MinConfidence > 0 {
		parts = append(parts, "confidence ≥ "+strconv.FormatFloat(s.MinConfidence, 'f', -1, 64))
	}
	if s.MinSimilarity > 0 {
		parts = append(parts, "similarity ≥ "+strconv.FormatFloat(s.MinSimilarity, 'f', -1, 64))
	}
	if s.SinceFork {
		parts = append(parts, "changed in this experiment")
	}
	parts = append(parts, "sorted by "+s.Sort)
	return strings.Join(parts, " · ")
}
