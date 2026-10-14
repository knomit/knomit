package fact

import (
	"fmt"
	"maps"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// The `artifacts:` section of an ontology declares rules for files under
// ArtifactsRoot, by path pattern relative to it (R7). It exists because the
// topic attribute walk cannot reach artifacts/: topics live under the
// ontology root. Each pattern maps to a block of keys, each with a CLOSED
// value set (the closed-value rule of attributeRegistry: a new behaviour is a
// new key, never a new value of an old one), and each value names a Go
// primitive the commit judge (store.judgeCommit) applies to every commit that
// changes a matching path:
//
//	artifacts:
//	  fleet/root.crt:          { pki: root, change: write-once, max_size: 16KiB }
//	  fleet/crl.pem:           { pki: crl, change: replace, replace_when: crl-newer, revoked_set: superset }
//	  fleet/certs/*.pem:       { pki: leaf, change: replace, replace_when: leaf-renewal, max_size: 16KiB }
//	  fleet/issued/*.pem:      { pki: issuance, change: write-once, max_size: 16KiB }
//	  fleet/revocations/*.txt: { pki: revocation, change: write-once, max_size: 4KiB }
//
// Keys and values:
//
//   - change: write-once — may be added; never modified or deleted.
//   - change: append-only — the new blob has the old blob as an exact byte
//     prefix; never deleted.
//   - change: replace — modified only when replace_when holds; never deleted.
//   - replace_when: crl-newer — the new CRL is signed by the declared pki: root
//     and its Number is strictly greater than the old one's.
//   - replace_when: leaf-renewal — the new leaf chains to the root, is not
//     revoked by the CRL, its email SAN's agent id equals the file's id, its
//     key is that agent's member record's current key, and its NotBefore is
//     strictly later than the old leaf's.
//   - revoked_set: superset — (pki: crl only) a replacing CRL also lists every
//     serial the CRL it replaces lists: a revocation is never undone by a
//     reissue. A new key rather than a new crl-newer value (closed-value rule).
//   - pki: root | crl | leaf | issuance | revocation — the file's role.
//     store.FleetAt reads the fleet's certificate artifacts (root, crl, leaf)
//     from these declarations, never from fixed paths. A leaf pattern's last
//     segment holds exactly one '*', which stands for the agent id (the file's
//     name is the lookup key; nothing parses the id). An issuance file is an
//     issued leaf's PEM (pki.CheckIssuance); a revocation file is a root-signed
//     revocation statement (pki.VerifyRevocation). Both are per-operation
//     records, write-once, judged when added and read by nothing else; their
//     names are for uniqueness and readability only — nothing derives order or
//     meaning from a name (D18).
//   - max_size — the largest blob a matching path may hold: a positive
//     integer of bytes, or <n>B, <n>KiB, <n>MiB. Larger fails closed. Never
//     declare it on a file that grows without bound (the CRL): it would turn a
//     legitimate update into a permanent refusal.
//
// A first ADD of any declared file is allowed (bootstrap); a file declared
// with a pki role must still hold as that role when added. A declared path
// must hold a regular file: a tree, symlink or submodule AT a declared path
// is refused, and so is anything UNDER a directory that a pattern declares a
// file (a directory squatting a file's name). A pattern declares a directory
// d a file for a path p beneath it when it matches d but NOT p
// (ArtifactFileAt): "fleet/certs/*.pem" makes certs/x.pem/y a squat, while
// "fleet/**", which matches fleet and everything beneath it, declares a
// family of paths and squats nothing; "fleet/*" matches the directory
// fleet/issued but not fleet/issued/x, so it declares fleet/issued a file.
//
// PATTERNS are relative to ArtifactsRoot and use the trigger glob syntax
// (glob.go): '*' and '?' inside one segment, "**" as a whole segment for any
// number of segments, everything else literal, matched ASCII
// case-insensitively (so a rule also covers every case spelling of its path).
// No segment may begin with '.'. An undeclared path has no rule.
//
// COMBINING: when several patterns match a path, max_size is the SMALLEST any
// of them declares (every cap applies). pki, and change together with
// replace_when, come from the MOST SPECIFIC pattern that declares them: fewer
// "**" segments first, then fewer wildcard segments, then more segments. Two
// equally specific patterns that declare different values for the same path
// make that path's rules ambiguous, and the judge refuses a commit that
// changes it rather than guess.
//
// STRICTNESS: a bad value, a malformed pattern, or an inconsistent block
// (replace without replace_when, a pki file without its change rule, a
// second root) is FATAL on every path: guessing a security rule is worse than
// a repository that refuses writes until it is fixed. An UNKNOWN key is a
// warning on the open path (a newer knomit may have written it; it is
// preserved by Serialize) and fatal for a new ontology (ParseNewOntology),
// where a typo would otherwise silently drop a rule.

// Artifact rule keys and values.
const (
	ArtifactKeyPKI         = "pki"
	ArtifactKeyChange      = "change"
	ArtifactKeyReplaceWhen = "replace_when"
	ArtifactKeyMaxSize     = "max_size"
	ArtifactKeyRevokedSet  = "revoked_set"

	PKIRoot       = "root"
	PKICRL        = "crl"
	PKILeaf       = "leaf"
	PKIIssuance   = "issuance"
	PKIRevocation = "revocation"

	ChangeWriteOnce  = "write-once"
	ChangeAppendOnly = "append-only"
	ChangeReplace    = "replace"

	ReplaceCRLNewer    = "crl-newer"
	ReplaceLeafRenewal = "leaf-renewal"

	RevokedSetSuperset = "superset"
)

// maxArtifactSize bounds what max_size may declare.
const maxArtifactSize = 1 << 30

var artifactValues = map[string][]string{
	ArtifactKeyPKI:         {PKIRoot, PKICRL, PKILeaf, PKIIssuance, PKIRevocation},
	ArtifactKeyChange:      {ChangeWriteOnce, ChangeAppendOnly, ChangeReplace},
	ArtifactKeyReplaceWhen: {ReplaceCRLNewer, ReplaceLeafRenewal},
	ArtifactKeyRevokedSet:  {RevokedSetSuperset},
}

// ArtifactAttrs is what the artifacts section says about one path.
type ArtifactAttrs struct {
	Matched     bool   // some declared pattern matches the path
	PKI         string // "" or one of the PKI* roles
	Change      string // "", ChangeWriteOnce, ChangeAppendOnly or ChangeReplace
	ReplaceWhen string // set exactly when Change is ChangeReplace
	RevokedSet  string // "" or RevokedSetSuperset (pki: crl only)
	MaxSize     int64  // 0: no cap declared
}

// artifactRule is one compiled pattern.
type artifactRule struct {
	pattern     string
	glob        *glob
	rank        [3]int // lower is more specific
	pki         string
	change      string
	replaceWhen string
	revokedSet  string
	maxSize     int64
}

// PKILayout is where an ontology's artifacts section puts the certificate
// artifacts, as repo-root paths.
type PKILayout struct {
	Root string // "" when no pki: root is declared (no PKI)
	CRL  string // "" when none is declared
	// A leaf for agent id is LeafDir + "/" + LeafPrefix + id + LeafSuffix.
	LeafDir, LeafPrefix, LeafSuffix string
}

// HasLeaves reports whether a leaf pattern is declared.
func (l PKILayout) HasLeaves() bool { return l.LeafDir != "" }

// LeafPath is the path of agent id's leaf.
func (l PKILayout) LeafPath(id string) string {
	return l.LeafDir + "/" + l.LeafPrefix + id + l.LeafSuffix
}

// LeafID returns the id a file NAME in LeafDir stands for: the part the
// pattern's '*' matched. It is a lookup key, not a parse of the id.
func (l PKILayout) LeafID(name string) (string, bool) {
	if !l.HasLeaves() || len(name) <= len(l.LeafPrefix)+len(l.LeafSuffix) ||
		!strings.HasPrefix(name, l.LeafPrefix) || !strings.HasSuffix(name, l.LeafSuffix) {
		return "", false
	}
	return name[len(l.LeafPrefix) : len(name)-len(l.LeafSuffix)], true
}

// compiledArtifacts is the parsed artifacts section, built once at parse.
type compiledArtifacts struct {
	rules []artifactRule
	pki   PKILayout
}

// HasArtifactRules reports whether the ontology declares any artifact rule.
func (o *Ontology) HasArtifactRules() bool {
	return o != nil && len(o.cache.artifacts.rules) > 0
}

// PKILayout returns where the certificate artifacts are declared; ok is false
// when no pki: root is declared (the repository has no PKI).
func (o *Ontology) PKILayout() (PKILayout, bool) {
	if o == nil {
		return PKILayout{}, false
	}
	l := o.cache.artifacts.pki
	return l, l.Root != ""
}

// ArtifactAttrs resolves the rules for repoPath (a repo-root path such as
// "artifacts/fleet/crl.pem"); a path outside ArtifactsRoot has none. The
// error names the ambiguity when two equally specific patterns disagree.
func (o *Ontology) ArtifactAttrs(repoPath string) (ArtifactAttrs, error) {
	var out ArtifactAttrs
	rel, ok := strings.CutPrefix(repoPath, ArtifactsRoot+"/")
	if o == nil || !ok || rel == "" {
		return out, nil
	}
	var pkiFrom, changeFrom *artifactRule
	for i := range o.cache.artifacts.rules {
		r := &o.cache.artifacts.rules[i]
		if !r.glob.Match(rel) {
			continue
		}
		out.Matched = true
		if r.maxSize > 0 && (out.MaxSize == 0 || r.maxSize < out.MaxSize) {
			out.MaxSize = r.maxSize
		}
		if r.pki != "" {
			switch {
			case pkiFrom == nil || r.rank != pkiFrom.rank && rankLess(r.rank, pkiFrom.rank):
				pkiFrom = r
			case r.rank == pkiFrom.rank && r.pki != pkiFrom.pki:
				return ArtifactAttrs{}, fmt.Errorf("artifact rules for %s are ambiguous: %q and %q are equally specific and declare different pki roles", repoPath, pkiFrom.pattern, r.pattern)
			}
		}
		if r.change != "" {
			switch {
			case changeFrom == nil || r.rank != changeFrom.rank && rankLess(r.rank, changeFrom.rank):
				changeFrom = r
			case r.rank == changeFrom.rank && (r.change != changeFrom.change || r.replaceWhen != changeFrom.replaceWhen || r.revokedSet != changeFrom.revokedSet):
				return ArtifactAttrs{}, fmt.Errorf("artifact rules for %s are ambiguous: %q and %q are equally specific and declare different change rules", repoPath, changeFrom.pattern, r.pattern)
			}
		}
	}
	if pkiFrom != nil {
		out.PKI = pkiFrom.pki
	}
	if changeFrom != nil {
		out.Change, out.ReplaceWhen, out.RevokedSet = changeFrom.change, changeFrom.replaceWhen, changeFrom.revokedSet
	}
	return out, nil
}

// ArtifactFileAt reports a declared pattern that makes dir a FILE for the
// path beneath it: one that matches dir but NOT beneath (both repo-root
// paths, beneath under dir). A pattern that matches both, such as
// "fleet/**", declares a family of paths, not a file at dir, and so does not
// count. ok is false when no pattern declares dir a file for beneath.
func (o *Ontology) ArtifactFileAt(dir, beneath string) (pattern string, ok bool) {
	relDir, okDir := strings.CutPrefix(dir, ArtifactsRoot+"/")
	rel, okRel := strings.CutPrefix(beneath, ArtifactsRoot+"/")
	if o == nil || !okDir || !okRel || relDir == "" {
		return "", false
	}
	for i := range o.cache.artifacts.rules {
		r := &o.cache.artifacts.rules[i]
		if r.glob.Match(relDir) && !r.glob.Match(rel) {
			return r.pattern, true
		}
	}
	return "", false
}

func rankLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func hasMeta(seg string) bool { return strings.ContainsAny(seg, "*?") }

var maxSizeRe = regexp.MustCompile(`^([1-9][0-9]*)(B|KiB|MiB)$`)

// parseMaxSize accepts a positive integer (bytes) or <n>B|KiB|MiB.
func parseMaxSize(v any) (int64, bool) {
	var n int64
	switch x := v.(type) {
	case int:
		n = int64(x)
	case string:
		m := maxSizeRe.FindStringSubmatch(x)
		if m == nil {
			return 0, false
		}
		b, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return 0, false
		}
		switch m[2] {
		case "KiB":
			b <<= 10
		case "MiB":
			b <<= 20
		}
		n = b
	default:
		return 0, false
	}
	if n <= 0 || n > maxArtifactSize {
		return 0, false
	}
	return n, true
}

// compileArtifacts validates the artifacts section and compiles it. body is
// the yaml value of `artifacts:` (positions only). Unknown keys are newOnly
// warnings; everything else that is wrong is an error.
func compileArtifacts(section map[string]map[string]any, body *yaml.Node) (compiledArtifacts, []Diagnostic) {
	var out compiledArtifacts
	var diags []Diagnostic
	keyNodes := mappingChildren(body)
	fail := func(pat, msg string) {
		diags = append(diags, diagAt(keyNodes[pat], fmt.Sprintf("parse ontology: artifacts %q: %s", pat, msg)))
	}
	for _, pat := range slices.Sorted(maps.Keys(section)) {
		block := section[pat]
		r := artifactRule{pattern: pat}
		g, err := compileGlob(pat)
		if err != nil {
			fail(pat, err.Error())
			continue
		}
		segs := strings.Split(pat, "/")
		for _, s := range segs {
			switch {
			case s == "**":
				r.rank[0]++
			case hasMeta(s):
				r.rank[1]++
			}
		}
		r.glob, r.rank[2] = g, -len(segs)
		for _, k := range slices.Sorted(maps.Keys(block)) {
			v := block[k]
			if k == ArtifactKeyMaxSize {
				n, ok := parseMaxSize(v)
				if !ok {
					fail(pat, fmt.Sprintf("max_size must be a positive number of bytes or <n>B, <n>KiB, <n>MiB (at most 1GiB), got %v (%T)", v, v))
					continue
				}
				r.maxSize = n
				continue
			}
			allowed, known := artifactValues[k]
			if !known {
				d := diagAt(keyNodes[pat], fmt.Sprintf("parse ontology: artifacts %q: unknown key %q: not recognised by this knomit and ignored", pat, k))
				d.Severity, d.newOnly = SeverityWarning, true
				diags = append(diags, d)
				continue
			}
			s, ok := v.(string)
			if !ok || !slices.Contains(allowed, s) {
				fail(pat, fmt.Sprintf("%s must be one of %s, got %v (%T)", k, strings.Join(allowed, ", "), v, v))
				continue
			}
			switch k {
			case ArtifactKeyPKI:
				r.pki = s
			case ArtifactKeyChange:
				r.change = s
			case ArtifactKeyReplaceWhen:
				r.replaceWhen = s
			case ArtifactKeyRevokedSet:
				r.revokedSet = s
			}
		}
		switch {
		case r.change == ChangeReplace && r.replaceWhen == "":
			fail(pat, "change: replace needs replace_when (a replace with no condition is no rule)")
		case r.replaceWhen != "" && r.change != ChangeReplace:
			fail(pat, "replace_when needs change: replace")
		case r.replaceWhen == ReplaceCRLNewer && r.pki != PKICRL:
			fail(pat, "replace_when: crl-newer needs pki: crl")
		case r.replaceWhen == ReplaceLeafRenewal && r.pki != PKILeaf:
			fail(pat, "replace_when: leaf-renewal needs pki: leaf")
		case r.pki == PKIRoot && r.change != ChangeWriteOnce:
			fail(pat, "pki: root needs change: write-once")
		case r.pki == PKICRL && r.replaceWhen != ReplaceCRLNewer:
			fail(pat, "pki: crl needs change: replace and replace_when: crl-newer")
		case r.pki == PKILeaf && r.replaceWhen != ReplaceLeafRenewal:
			fail(pat, "pki: leaf needs change: replace and replace_when: leaf-renewal")
		case r.revokedSet != "" && r.pki != PKICRL:
			fail(pat, "revoked_set needs pki: crl")
		case (r.pki == PKIIssuance || r.pki == PKIRevocation) && r.change != ChangeWriteOnce:
			fail(pat, "pki: "+r.pki+" needs change: write-once (a per-operation record is never changed)")
		case (r.pki == PKIRoot || r.pki == PKICRL) && (r.rank[0] > 0 || r.rank[1] > 0):
			fail(pat, "a pki: "+r.pki+" pattern must name one file (no wildcards)")
		case r.pki == PKILeaf && !leafPatternOK(segs):
			fail(pat, "a pki: leaf pattern must be literal except its last segment, which holds exactly one '*' (the agent id) and no other wildcard")
		default:
			out.rules = append(out.rules, r)
			full := ArtifactsRoot + "/" + pat
			switch r.pki {
			case PKIRoot:
				if out.pki.Root != "" {
					fail(pat, "a second pki: root is declared")
				}
				out.pki.Root = full
			case PKICRL:
				if out.pki.CRL != "" {
					fail(pat, "a second pki: crl is declared")
				}
				out.pki.CRL = full
			case PKILeaf:
				if out.pki.HasLeaves() {
					fail(pat, "a second pki: leaf pattern is declared")
				}
				last := segs[len(segs)-1]
				star := strings.IndexByte(last, '*')
				out.pki.LeafDir = path.Dir(full)
				out.pki.LeafPrefix, out.pki.LeafSuffix = last[:star], last[star+1:]
			}
		}
	}
	opRecords := false
	for _, r := range out.rules {
		opRecords = opRecords || r.pki == PKIIssuance || r.pki == PKIRevocation
	}
	if out.pki.Root == "" && (out.pki.CRL != "" || out.pki.HasLeaves() || opRecords) {
		diags = append(diags, diagAt(body, "parse ontology: artifacts: a pki: crl, leaf, issuance or revocation is declared without a pki: root"))
	}
	return out, diags
}

// leafPatternOK: literal segments, then a last segment with exactly one '*'
// and no other wildcard character.
func leafPatternOK(segs []string) bool {
	for _, s := range segs[:len(segs)-1] {
		if hasMeta(s) {
			return false
		}
	}
	last := segs[len(segs)-1]
	return strings.Count(last, "*") == 1 && !strings.Contains(last, "?")
}

// artifactsNotSubset reports whether a declares a pattern b does not declare
// identically. The boot refresh writes the preset over the stored file, so a
// stored rule the preset lacks would be erased.
func artifactsNotSubset(a, b map[string]map[string]any) bool {
	for pat, block := range a {
		ob, ok := b[pat]
		if !ok || !reflect.DeepEqual(block, ob) {
			return true
		}
	}
	return false
}

// serializeArtifacts emits the section with sorted patterns and keys, values
// as decoded (unknown keys included).
func serializeArtifacts(parent *yaml.Node, section map[string]map[string]any) error {
	m := &yaml.Node{Kind: yaml.MappingNode}
	for _, pat := range slices.Sorted(maps.Keys(section)) {
		bm := &yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle}
		for _, k := range slices.Sorted(maps.Keys(section[pat])) {
			var v yaml.Node
			if err := v.Encode(section[pat][k]); err != nil {
				return fmt.Errorf("artifacts %q %q: %w", pat, k, err)
			}
			bm.Content = append(bm.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: k}, &v)
		}
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: pat}, bm)
	}
	parent.Content = append(parent.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "artifacts"}, m)
	return nil
}
