// internal/okf/bundle.go
package okf

import (
	"path"
	"sort"
	"strings"

	"knomit/internal/fact"
)

// SkipReport counts facts dropped during Build (conformance is an output
// invariant: a fact that cannot be mapped is never emitted).
type SkipReport struct {
	Skipped int
	Reasons []string
}

// Build assembles the full OKF bundle from facts + precomputed log entries.
// opts carries optional cross-document resolvers; the zero value is valid and
// simply leaves src:// citations inert.
func Build(repo RepoIdentity, facts []FactInput, log []LogEntry, opts RenderOpts) (Bundle, SkipReport) {
	var skips SkipReport
	files := map[string][]byte{}

	// dirs tracks, per directory, its immediate child subdirectories and its
	// concept entries, so we can synthesize index.md at every level.
	type conceptEntry struct{ file, title, typ string }
	subdirs := map[string]map[string]bool{} // dir -> set of child dir basenames
	concepts := map[string][]conceptEntry{} // dir -> concept entries

	registerDir := func(dir string) {
		// Walk up, registering each dir as a child of its parent (root = "").
		for dir != "" && dir != "." {
			parent := parentDir(dir)
			if subdirs[parent] == nil {
				subdirs[parent] = map[string]bool{}
			}
			subdirs[parent][path.Base(dir)] = true
			dir = parent
		}
	}

	// Pass 1: compute every fact's bundle path. A kb/… citation resolves to a
	// filename derived from the TARGET fact's title, so the full map must exist
	// before any document is rendered — this is what makes the derivation graph
	// linkable at all.
	type placed struct {
		fi         FactInput
		dir, fname string
	}
	byKnomitPath := make(map[string]string, len(facts))
	dirOfFact := make(map[string]string, len(facts))
	refByPath := make(map[string]FactRef, len(facts))
	placements := make([]placed, 0, len(facts))
	for _, fi := range facts {
		kp := fi.Fact.Path() // e.g. kb/decisions/okf/scope/d9d6557d.md
		dir := parentDir(kp) // kb/decisions/okf/scope
		uuid8 := strings.TrimSuffix(path.Base(kp), ".md")
		fname := Slug(fi.Fact.Title, path.Base(dir), uuid8)
		bundlePath := path.Join(dir, fname)
		byKnomitPath[kp] = bundlePath
		refByPath[kp] = FactRef{
			Path:  bundlePath,
			Title: firstNonEmpty(fi.Fact.Title, string(fi.Fact.Type)),
		}
		placements = append(placements, placed{fi: fi, dir: dir, fname: fname})
	}
	resolve := func(knomitPath string) (FactRef, bool) {
		r, ok := refByPath[knomitPath]
		return r, ok
	}
	// Invert the ref graph so every fact can name what cites it. Built from the
	// same data as refByPath, so it costs one pass and no new inputs. Sorted by
	// title for deterministic output.
	citersByPath := map[string][]FactRef{}
	for _, fi := range facts {
		self, ok := refByPath[fi.Fact.Path()]
		if !ok {
			continue
		}
		for _, r := range dedupe(fi.Fact.Refs) {
			if !strings.HasPrefix(r, "kb/") || r == fi.Fact.Path() {
				continue // external ref, or a self-citation
			}
			if _, live := refByPath[r]; !live {
				continue // points outside this bundle
			}
			citersByPath[r] = append(citersByPath[r], self)
		}
	}
	for p := range citersByPath {
		cs := citersByPath[p]
		sort.Slice(cs, func(i, j int) bool {
			if cs[i].Title != cs[j].Title {
				return cs[i].Title < cs[j].Title
			}
			return cs[i].Path < cs[j].Path
		})
		citersByPath[p] = cs
	}

	// Hub pages are PLANNED before rendering: a concept links to its domain and
	// entity pages, so their paths must be known before any document is built.
	// The pages themselves are rendered afterwards, once bundle paths exist.
	plan := planHubs(facts)

	// Pass 2: render.
	for _, pl := range placements {
		fi := pl.fi
		body, err := Concept(fi, repo, pl.dir, RenderOpts{
			// The bundle's own repo, so a canonical kb://<own-id>/… ref
			// resolves to a bundle document like its bare equivalent.
			LocalRepoID:   fact.ID12(repo.ID),
			ResolveFact:   resolve,
			ResolveSource: opts.ResolveSource,
			ResolveHub:    plan.pageFor,
			ResolveCiters: func(p string) []FactRef { return citersByPath[p] },
		})
		if err != nil {
			skips.Skipped++
			skips.Reasons = append(skips.Reasons, fi.Fact.Path()+": "+err.Error())
			continue
		}
		kp := fi.Fact.Path()
		files[path.Join(pl.dir, pl.fname)] = body
		// Recorded only for facts that actually rendered, so a per-directory log
		// can never name a document the bundle does not contain.
		dirOfFact[kp] = pl.dir

		concepts[pl.dir] = append(concepts[pl.dir], conceptEntry{
			file:  pl.fname,
			title: firstNonEmpty(fi.Fact.Title, string(fi.Fact.Type)),
			typ:   okfType(kp, string(fi.Fact.Type)), // OKF type = singularized topic (concept.go)
		})
		registerDir(pl.dir)
	}

	// Cross-cutting hub views ("all facts in domain X" / "touching entity Y").
	// Built before the index pass so their directories are registered and the
	// root index links them; their own index.md files are merged in afterwards,
	// replacing the empty ones the generic pass would emit.
	hubFiles := renderHubs(plan, facts, byKnomitPath)
	digestFiles, digestEntries := buildDigests(facts, byKnomitPath)
	for p, content := range digestFiles {
		hubFiles[p] = content
	}
	// Withdrawn knowledge, as a single index. Retired facts get no concept
	// document of their own — see renderRetired — so this page (and the git
	// history the bundle ships in) is the whole of their presence here.
	if doc := renderRetired(opts.Retired, RenderOpts{ResolveFact: resolve}); doc != nil {
		hubFiles[retiredFile] = doc
		digestEntries = append(digestEntries, indexEntry{
			name:   "retired",
			target: path.Base(retiredFile),
			note:   pluralFacts(len(opts.Retired)) + " withdrawn",
		})
	}
	if len(hubFiles) > 0 {
		hubFiles[viewsRoot+"/index.md"] = renderViewsIndex(hubFiles, digestEntries)
	}
	for p := range hubFiles {
		registerDir(parentDir(p)) // walks up: views/domains -> views -> root
	}

	// Per-directory index.md (every dir that has children), plus root.
	allDirs := map[string]bool{"": true}
	for d := range subdirs {
		allDirs[d] = true
	}
	for d := range concepts {
		allDirs[d] = true
		registerDirInto(allDirs, d)
	}
	ont := opts.Ontology
	for d := range allDirs {
		var b strings.Builder
		if d == "" {
			b.WriteString("---\nokf_version: \"" + OKFVersion + "\"\n---\n\n")
		}
		b.WriteString("# " + indexHeading(d, ont) + "\n\n")

		// The scheme's own authored description, and each topic/category's,
		// become the index prose OKF recommends for progressive disclosure.
		if desc := ontologyDesc(d, ont); desc != "" {
			b.WriteString(desc + "\n\n")
		}

		childDirs := sortedKeys(subdirs[d])
		for _, cd := range childDirs {
			link := relLink(d, path.Join(d, cd, "index.md"))
			b.WriteString("- [" + escapeLinkText(cd) + "](" + link + ")")
			// Entry-level description, per the spec's index format: entries
			// SHOULD carry the linked document's description.
			if cdesc := ontologyDesc(path.Join(d, cd), ont); cdesc != "" {
				b.WriteString(" — " + cdesc)
			}
			b.WriteString("\n")
		}
		// Concept entries are LINKED, not just named: index.md is OKF's
		// progressive-disclosure surface, so it must be navigable to the
		// documents themselves, not only to child directories.
		ces := concepts[d]
		sort.Slice(ces, func(i, j int) bool { return ces[i].file < ces[j].file })
		for _, ce := range ces {
			link := relLink(d, path.Join(d, ce.file))
			b.WriteString("- [" + escapeLinkText(ce.title) + "](" + link + ") — " + ce.typ + "\n")
		}
		files[path.Join(d, "index.md")] = []byte(b.String())
	}

	// Hub documents last: their index.md replaces the empty placeholder the
	// generic pass emits for a directory holding no concept documents.
	for p, content := range hubFiles {
		files[p] = content
	}

	// The changelog, split across the scopes §9 allows. Withdrawals join it here:
	// they are changes to a knowledge base like any other, and reporting only
	// what it gained described half of what happened.
	//
	// A directory is a valid home only if it still holds rendered documents —
	// dirOfFact is built from facts that actually rendered. Filing an event under
	// a directory whose last fact was retired would mint a folder containing
	// nothing but a log, unreachable from any index.
	liveDirs := make(map[string]bool, len(dirOfFact))
	for _, d := range dirOfFact {
		liveDirs[d] = true
	}
	entries := append(append([]LogEntry{}, log...),
		retirementLogEntries(opts.Retired, RenderOpts{ResolveFact: resolve})...)
	rootLog, folderLogs := partitionLog(entries, liveDirs)

	// No entries means no file, here and at every level.
	if doc := RenderLog(rootLog); doc != nil {
		files["log.md"] = doc
	}
	for dir, es := range folderLogs {
		if doc := RenderLog(es); doc != nil {
			files[path.Join(dir, "log.md")] = doc
		}
	}

	// Materialize the map into a sorted Bundle for deterministic ordering.
	out := Bundle{}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		out.Files = append(out.Files, File{Path: p, Content: files[p]})
	}
	return out, skips
}

func parentDir(p string) string {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return ""
	}
	return p[:i]
}

func registerDirInto(set map[string]bool, dir string) {
	for dir != "" {
		set[dir] = true
		dir = parentDir(dir)
	}
}

func indexTitle(dir string) string {
	if dir == "" {
		return "Knowledge Base"
	}
	return path.Base(dir)
}

// ontologyRoot is the bundle directory holding the authored ontology.
const ontologyRoot = "kb"

// indexHeading titles a directory index, preferring the ontology's own name for
// the knowledge root over the bare directory name.
func indexHeading(dir string, ont OntologyDoc) string {
	if dir == ontologyRoot && ont.Name != "" {
		return ont.Name
	}
	return indexTitle(dir)
}

// ontologyDesc returns the authored description for a bundle directory: the
// scheme's own for kb/, and the matching topic/category node's below it.
// Directories outside the authored ontology (views/) have none.
func ontologyDesc(dir string, ont OntologyDoc) string {
	if dir == ontologyRoot {
		return ont.Description
	}
	rel := strings.TrimPrefix(dir, ontologyRoot+"/")
	if rel == dir { // not under kb/
		return ""
	}
	return ont.Nodes[rel]
}

func sortedKeys(m map[string]bool) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// relLink builds a relative markdown link target from fromDir to the
// bundle-relative toPath. Relative rather than absolute (bundle-root) form is
// deliberate: GitHub resolves a leading "/" against the repository root, which
// breaks every link once the bundle is published there — the distribution path
// this export is designed for. The spec permits both (§3).
func relLink(fromDir, toPath string) string {
	if fromDir == "" {
		return toPath
	}
	from := strings.Split(fromDir, "/")
	to := strings.Split(toPath, "/")
	i := 0
	for i < len(from) && i < len(to)-1 && from[i] == to[i] {
		i++
	}
	var b strings.Builder
	for range from[i:] {
		b.WriteString("../")
	}
	b.WriteString(strings.Join(to[i:], "/"))
	return b.String()
}

// linkTextEscaper escapes the markdown link-label delimiters. Real knomit
// titles contain brackets (e.g. "no skipped[] block", "refs → [:DERIVED_FROM]
// edges"), which would otherwise terminate the label early and emit a broken
// link into index.md.
var linkTextEscaper = strings.NewReplacer("[", `\[`, "]", `\]`)

func escapeLinkText(s string) string { return linkTextEscaper.Replace(s) }

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
