package fact

import (
	"errors"
	"fmt"
	"strings"
)

// A glob is a compiled trigger `match` pattern: a path pattern relative to the
// ontology root, split into segments once at compile time so that matching a
// path allocates nothing.
//
//   - one whole segment, or any run of characters inside one; never crosses "/"
//     **   zero or more whole segments; only allowed as a segment of its own
//     ?    exactly one character that is not "/"
//
// Everything else is literal. Matching is ASCII case-insensitive, because the
// rest of knomit treats topic paths that way (Attr, ValidatePath and F05's
// NormalizeChangesPrefix all lowercase), while tree paths keep their case.
//
// There is no escape character and no [class] or {a,b} syntax: the three
// placeholders are substituted BEFORE compileGlob sees the pattern, and a
// value that would read as syntax is refused there.
type glob struct {
	segs []globSeg
}

type globSegKind uint8

const (
	segLiteral    globSegKind = iota // compared with ASCII case folding
	segWild                          // contains * or ?, matched within one segment
	segDoubleStar                    // "**": zero or more whole segments
)

type globSeg struct {
	kind globSegKind
	pat  string // lowercased
}

var errEmptyGlob = errors.New("empty pattern")

// compileGlob compiles a pattern. It refuses an empty pattern, an empty
// segment ("a//b", a leading or trailing "/"), a "**" that shares its segment
// with anything else, and any segment beginning with "." — dot paths are
// private and are never trigger-visible (user ruling, 2026-09-27:
// ".knomit is for data and code only"). The repo-root artifacts/ folder is
// not trigger-visible either, by location: trigger paths are relative to the
// ontology root and the trigger diff covers only that subtree (F25).
func compileGlob(pattern string) (*glob, error) {
	if pattern == "" {
		return nil, errEmptyGlob
	}
	parts := strings.Split(strings.ToLower(pattern), "/")
	g := &glob{segs: make([]globSeg, 0, len(parts))}
	for _, p := range parts {
		switch {
		case p == "":
			return nil, fmt.Errorf("empty path segment in %q", pattern)
		case p == "**":
			g.segs = append(g.segs, globSeg{kind: segDoubleStar, pat: p})
		case strings.Contains(p, "**"):
			return nil, fmt.Errorf("%q: ** must be a whole path segment", pattern)
		case strings.HasPrefix(p, "."):
			return nil, fmt.Errorf("%q: a segment starting with \".\" names private state, which triggers never see", pattern)
		case strings.ContainsAny(p, "*?"):
			g.segs = append(g.segs, globSeg{kind: segWild, pat: p})
		default:
			g.segs = append(g.segs, globSeg{kind: segLiteral, pat: p})
		}
	}
	return g, nil
}

// Match reports whether path (relative to the ontology root, "/"-separated)
// matches. It walks path in place and allocates nothing.
func (g *glob) Match(path string) bool {
	return matchSegs(g.segs, path)
}

// matchSegs matches the remaining segments against the remaining path. An
// empty path means every path segment has been consumed.
func matchSegs(segs []globSeg, path string) bool {
	for len(segs) > 0 {
		s := segs[0]
		if s.kind == segDoubleStar {
			rest := segs[1:]
			if len(rest) == 0 {
				return true
			}
			// Zero segments first, then consume one whole segment at a time.
			for q := path; ; {
				if matchSegs(rest, q) {
					return true
				}
				i := strings.IndexByte(q, '/')
				if i < 0 {
					return false
				}
				q = q[i+1:]
			}
		}
		if path == "" {
			return false
		}
		seg := path
		path = ""
		if i := strings.IndexByte(seg, '/'); i >= 0 {
			seg, path = seg[:i], seg[i+1:]
		}
		switch s.kind {
		case segLiteral:
			if !asciiEqualFold(s.pat, seg) {
				return false
			}
		case segWild:
			if !wildMatch(s.pat, seg) {
				return false
			}
		}
		segs = segs[1:]
	}
	return path == ""
}

// wildMatch matches one segment against a pattern of literals, * and ?
// (pat is already lowercase). Iterative with single-star backtracking, so it
// is linear in practice and never allocates.
func wildMatch(pat, s string) bool {
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		switch {
		case pi < len(pat) && pat[pi] == '*':
			star, mark = pi, si
			pi++
		case pi < len(pat) && (pat[pi] == '?' || pat[pi] == lowerASCII(s[si])):
			pi++
			si++
		case star >= 0:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(pat) && pat[pi] == '*' {
		pi++
	}
	return pi == len(pat)
}

// asciiEqualFold compares a lowercase pattern with s, folding only ASCII
// letters in s. Non-ASCII bytes must match exactly.
func asciiEqualFold(lowerPat, s string) bool {
	if len(lowerPat) != len(s) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if lowerPat[i] != lowerASCII(s[i]) {
			return false
		}
	}
	return true
}

func lowerASCII(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// literalPrefix reports whether the pattern's first len(prefix) segments are
// literals equal to prefix's segments (prefix is a lowercase node path such as
// "tasks" or "tasks/research"). A trigger's match must stay inside its node.
func (g *glob) literalPrefix(prefix []string) bool {
	if len(g.segs) < len(prefix) {
		return false
	}
	for i, p := range prefix {
		if g.segs[i].kind != segLiteral || g.segs[i].pat != p {
			return false
		}
	}
	return true
}
