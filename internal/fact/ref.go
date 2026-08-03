package fact

import (
	"fmt"
	"strings"
)

// Scheme prefixes and hash widths.
//
// RepoIDWireLen is 12 because kb://<id12>/… is the established canonical form
// (lenses RFC §6.2 decision 11); src:// reuses the same width so both schemes
// address repos identically. gitHashLen is the full width of a git object id:
// src:// commit and blob hashes are stored unabbreviated because expanding an
// abbreviation requires the source repo's object database, which knomit does
// not have — an abbreviated hash in a stored ref is unverifiable by anyone who
// does not already have that repo, which is exactly the reader a citation must
// serve.
const (
	KBScheme      = "kb://"
	SrcScheme     = "src://"
	RepoIDWireLen = 12
	gitHashLen    = 40
)

// DisplayHashLen is how much of a src:// commit or blob hash survives into the
// DISPLAY form — 8, git's own default short-hash width, which is enough to
// recognise a commit at a glance and useless for retrieving one. That asymmetry
// is the point: abbreviation is a rendering choice and never touches the stored
// ref, which stays full-width for the reason gitHashLen documents above.
const DisplayHashLen = 8

// RefKind is what a ref IS — syntax plus repo identity, both independent of any
// commit. It is deliberately NOT a resolution status: whether a target exists
// is anchor-dependent, and is decided by the consumer that holds the anchor
// (internal/web's ref resolver, the UI's commit-anchored hop). Computing
// resolution here would force a HEAD answer onto readers anchored elsewhere.
type RefKind string

const (
	// RefLocalFact is a fact in the caller's own repo — a bare repo-relative
	// path, or kb://<own-id>/<path>.
	RefLocalFact RefKind = "local_fact"
	// RefForeignFact is a fact in a different knomit repo: kb://<other>/<path>.
	RefForeignFact RefKind = "foreign_fact"
	// RefSourceCode is a source-code citation. Terminal: knomit's object
	// database holds fact blobs only, never source, so a src:// ref never
	// resolves here and never becomes an edge. Named "source_code" rather than
	// "source" because facts already carry a `sources` field and the two would
	// read as related.
	RefSourceCode RefKind = "source_code"
	// RefExternalURL is http://, https://, file://, or any other scheme.
	RefExternalURL RefKind = "external_url"
	// RefMalformed matched a knomit scheme but failed to parse; Err says why.
	RefMalformed RefKind = "malformed"
)

// Ref is a classified reference. Fields not applicable to the Kind are empty.
// Comparable by design so tests can assert on the whole struct.
//
// Raw is always byte-identical to the input: it is what the author wrote, and
// what error messages and the UI must echo back. Path is the canonical form for
// lookups, which for fact kinds means lowercased.
type Ref struct {
	Raw    string
	Kind   RefKind
	RepoID string // kb: 12-hex repo id. src: 12-hex repo id, or a legacy name.
	Path   string // repo-relative; lowercased for fact kinds only
	Commit string // src only
	Blob   string // src only, new form only
	Lines  string // src only, e.g. "241-259"
	Legacy bool   // src:// in the pre-blob form (named repo and/or no blob)
	Err    string // set only when Kind == RefMalformed
}

// ClassifyRef is THE answer to "what is this ref?" — the single authority the
// write gate, the edge builder, replay, knomit_explain, the fact API, and the
// web client all consume. Pure: no I/O, no corpus lookup, no git.
//
// localRepoID is the caller's own 12-hex repo id, used only to decide local vs
// foreign for kb:// refs. Pass "" when unknown: bare paths still classify as
// RefLocalFact (the kind is not in doubt) and every kb:// ref reads as foreign,
// which is the safe direction — consumers under-report local refs rather than
// inventing them.
func ClassifyRef(raw, localRepoID string) Ref {
	r := Ref{Raw: raw}
	switch {
	case raw == "":
		r.Kind, r.Err = RefMalformed, "empty ref"

	case strings.HasPrefix(raw, KBScheme):
		id, rel, _, err := ParseKBPath(raw)
		if err != nil {
			r.Kind, r.Err = RefMalformed, err.Error()
			return r
		}
		// Fact paths are lowercase-canonical in storage (NewFact lowercases
		// unconditionally) and every lookup consuming Path is case-sensitive.
		r.RepoID, r.Path = id, strings.ToLower(rel)
		if localRepoID != "" && id == localRepoID {
			r.Kind = RefLocalFact
		} else {
			r.Kind = RefForeignFact
		}

	case strings.HasPrefix(raw, SrcScheme):
		return classifySrc(raw)

	case hasScheme(raw):
		r.Kind = RefExternalURL

	default:
		// Schemeless: a repo-relative fact path in the caller's own repo.
		r.Kind, r.RepoID, r.Path = RefLocalFact, localRepoID, strings.ToLower(raw)
	}
	return r
}

// classifySrc parses src://<repo>/<path>[@<commit>[:<blob>]][#L<a>[-L<b>]].
//
// Order matters: the fragment comes off first (RFC 3986 puts it last), then the
// version splits at the LAST '@' so an '@' inside a path does not confuse it,
// then repo and path split at the FIRST '/'.
//
// Source paths are NOT lowercased — they name real files in a case-sensitive
// tree (FactBody.tsx, README.md).
func classifySrc(raw string) Ref {
	r := Ref{Raw: raw, Kind: RefSourceCode}
	malformed := func() Ref {
		return Ref{
			Raw: raw, Kind: RefMalformed,
			Err: fmt.Sprintf("malformed src:// ref %q — want src://<repo>/<path>[@<commit>[:<blob>]]", raw),
		}
	}

	rest := strings.TrimPrefix(raw, SrcScheme)

	// Only a well-formed line range is treated as a fragment; '#' is legal in a
	// filename, so anything else stays part of the path.
	if i := strings.LastIndex(rest, "#"); i >= 0 {
		if lines, ok := parseLineRange(rest[i+1:]); ok {
			r.Lines, rest = lines, rest[:i]
		}
	}

	if i := strings.LastIndex(rest, "@"); i >= 0 {
		commit, blob, hasBlob := strings.Cut(rest[i+1:], ":")
		rest = rest[:i]
		if commit == "" || (hasBlob && blob == "") {
			return malformed()
		}
		r.Commit, r.Blob = commit, blob
	}

	repo, path, found := strings.Cut(rest, "/")
	if !found || repo == "" || path == "" {
		return malformed()
	}
	r.RepoID, r.Path = repo, path

	// Legacy is anything short of the full new form: a named repo (not a 12-hex
	// id), a missing blob, or a missing commit.
	r.Legacy = r.Blob == "" || r.Commit == "" ||
		len(repo) != RepoIDWireLen || !isLowerHex(repo)
	return r
}

// Display renders a repo-qualified ref in the compact form a human reads: the
// 12-hex repo id replaced by repoName when the caller could resolve one, and —
// for src:// — the 40-hex commit and blob abbreviated to DisplayHashLen plus an
// ellipsis.
//
// It is a LABEL, not an address. Raw is what the corpus holds and what a reader
// must be able to copy, so consumers show Display as the visible text and Raw
// as the hover title — never the reverse, and never Display alone where the
// full citation is what the reader came for.
//
// repoName is the caller's answer to "is this repo id one I know?", since this
// package is pure and cannot look one up. Pass "" when unknown: the id stays,
// which is the honest fallback. A legacy src ref already names its repo, and
// "" for those leaves that name untouched.
//
// Returns "" — meaning "no display form, show Raw" — for the kinds with nothing
// to gain. A LOCAL fact ref is already rendered by its repo-relative Path, so a
// second shortening of the same thing would only diverge from it; a bare path
// is the shortest true form of itself; and an external URL must never be
// silently truncated into something unfollowable.
func (r Ref) Display(repoName string) string {
	repo := r.RepoID
	if repoName != "" {
		repo = repoName
	}
	var b strings.Builder
	switch r.Kind {
	case RefForeignFact:
		b.WriteString(KBScheme)
	case RefSourceCode:
		b.WriteString(SrcScheme)
	default:
		return ""
	}
	b.WriteString(repo)
	b.WriteString("/")
	b.WriteString(r.Path)
	if r.Commit != "" {
		b.WriteString("@")
		b.WriteString(abbrevHash(r.Commit))
		if r.Blob != "" {
			b.WriteString(":")
			b.WriteString(abbrevHash(r.Blob))
		}
	}
	if r.Lines != "" {
		b.WriteString("#L")
		b.WriteString(r.Lines)
	}
	return b.String()
}

// abbrevHash shortens a git object id for display. A hash already at or under
// DisplayHashLen is returned unchanged rather than gaining a "…" that would
// claim there is more to it: legacy src refs carry abbreviated commits, and
// marking those as truncated would invent precision the ref never had.
func abbrevHash(h string) string {
	if len(h) <= DisplayHashLen {
		return h
	}
	return h[:DisplayHashLen] + "…"
}

// parseLineRange accepts "L241", "L241-L259" and "L241-259", returning the
// canonical "241" / "241-259" form.
func parseLineRange(s string) (string, bool) {
	if !strings.HasPrefix(s, "L") {
		return "", false
	}
	start, end, hasEnd := strings.Cut(s[1:], "-")
	if !isDigits(start) {
		return "", false
	}
	if !hasEnd {
		return start, true
	}
	if end = strings.TrimPrefix(end, "L"); !isDigits(end) {
		return "", false
	}
	return start + "-" + end, true
}

// hasScheme reports whether s begins with an RFC 3986 scheme: an alpha, then
// alphanumerics/+/-/., then ':'.
func hasScheme(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ':':
			return i > 0
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'):
		default:
			return false
		}
	}
	return false
}

// ParseKBPath splits a kb:// wire path. A bare path returns qualified=false
// with rel=p. A kb:// path must carry exactly RepoIDWireLen lowercase-hex id
// chars and a non-empty repo-relative remainder; anything else is malformed.
// An uppercase id is tolerated on input and canonicalized to lowercase.
//
// This is the implementation federate.ParseQualifiedPath delegates to. It lives
// here because internal/fact is a leaf: federate → repos → store → fact, so
// fact cannot import federate.
func ParseKBPath(p string) (id, rel string, qualified bool, err error) {
	rest, ok := strings.CutPrefix(p, KBScheme)
	if !ok {
		return "", p, false, nil
	}
	id, rel, found := strings.Cut(rest, "/")
	id = strings.ToLower(id) // tolerate a pasted uppercase hash
	if !found || rel == "" || len(id) != RepoIDWireLen || !isLowerHex(id) {
		return "", "", true, fmt.Errorf("malformed kb:// path %q — want kb://<12-hex-repo-id>/<path>", p)
	}
	return id, rel, true, nil
}

// QualifyKBPath renders the canonical qualified wire form.
func QualifyKBPath(id12, rel string) string { return KBScheme + id12 + "/" + rel }

// ID12 shortens a full root-commit hash to the wire form.
func ID12(full string) string {
	if len(full) <= RepoIDWireLen {
		return full
	}
	return full[:RepoIDWireLen]
}

func isLowerHex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// refShapeWarnings is the ref SHAPE rule, as a list of problems. It is the
// single implementation behind both directions of the fourth validation axis,
// which is deliberately ASYMMETRIC:
//
//   - SerializeFact calls ValidateRefs and REFUSES to write a malformed ref.
//   - ParseFact records these as Fact.RefWarnings and reads the fact anyway.
//
// The asymmetry mirrors origin's, for the same reason: this is a historical
// graph, and a version that was legal when committed must stay readable
// forever. Failing the read instead made one bad ref delete a fact from the
// search index and the provenance graph without a word.
//
// Existence is deliberately NOT checked here: a local fact ref is checked by
// the write gate (internal/refs), which has corpus access this package must
// not; a source ref cannot be checked at all, since knomit's object database
// holds fact blobs and never source.
//
// The one substantive rule beyond parseability: a ref in the NEW src form — a
// 12-hex repo id AND a blob — must carry full 40-hex commit and blob. Legacy
// src forms are accepted unconditionally and permanently.
//
// Collects every problem rather than stopping at the first: the caller is
// usually an agent that must fix the refs and retry, and one-problem-per-round-
// trip is the difference between one retry and five.
func refShapeWarnings(refs []string) []string {
	var problems []string
	for _, raw := range refs {
		r := ClassifyRef(raw, "") // localRepoID is irrelevant to shape
		switch {
		case r.Kind == RefMalformed:
			problems = append(problems, fmt.Sprintf("%q — %s", raw, r.Err))
		case r.Kind == RefSourceCode && !r.Legacy:
			// A 12-hex repo id AND a blob means the author intended the new
			// form, so hold them to it. State where each value comes from: an
			// agent that abbreviated needs the command, not a restatement.
			if len(r.Commit) != gitHashLen || !isLowerHex(r.Commit) {
				problems = append(problems, fmt.Sprintf(
					"%q — commit must be a full 40-hex hash (got %d chars); run: git rev-parse %s",
					raw, len(r.Commit), r.Commit))
			}
			if len(r.Blob) != gitHashLen || !isLowerHex(r.Blob) {
				problems = append(problems, fmt.Sprintf(
					"%q — blob must be a full 40-hex hash (got %d chars); run: git rev-parse <commit>:%s",
					raw, len(r.Blob), r.Path))
			}
		}
	}
	return problems
}

// ValidateRefs is the WRITE side of the shape rule: it turns refShapeWarnings
// into an error. SerializeFact calls it, so a malformed ref cannot enter the
// corpus; ParseFact deliberately does not (see refShapeWarnings).
func ValidateRefs(refs []string) error {
	problems := refShapeWarnings(refs)
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("invalid refs:\n  %s\n\nAccepted forms:\n"+
		"  kb/<topic>/…/<id>.md                      a fact in this repo\n"+
		"  kb://<12-hex-repo-id>/<path>              a fact in this or another repo\n"+
		"  src://<12-hex-repo-id>/<path>@<40-hex-commit>:<40-hex-blob>[#L1-L9]\n"+
		"  src://<repo-name>/<path>[@<commit>]       legacy source form, still accepted\n"+
		"  https://… or file:///…                    external",
		strings.Join(problems, "\n  "))
}
