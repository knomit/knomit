// internal/okf/bundle.go
package okf

import (
	"path"
	"sort"
	"strings"
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
	placements := make([]placed, 0, len(facts))
	for _, fi := range facts {
		kp := fi.Fact.Path() // e.g. kb/decisions/okf/scope/d9d6557d.md
		rel := strings.TrimPrefix(kp, "kb/")
		dir := parentDir(rel) // decisions/okf/scope
		uuid8 := strings.TrimSuffix(path.Base(rel), ".md")
		fname := Slug(fi.Fact.Title, path.Base(dir), uuid8)
		byKnomitPath[kp] = path.Join(dir, fname)
		placements = append(placements, placed{fi: fi, dir: dir, fname: fname})
	}
	resolve := func(knomitPath string) (string, bool) {
		p, ok := byKnomitPath[knomitPath]
		return p, ok
	}

	// Pass 2: render.
	for _, pl := range placements {
		fi := pl.fi
		body, err := Concept(fi, repo, pl.dir, RenderOpts{
			ResolveFact:   resolve,
			ResolveSource: opts.ResolveSource,
		})
		if err != nil {
			skips.Skipped++
			skips.Reasons = append(skips.Reasons, fi.Fact.Path()+": "+err.Error())
			continue
		}
		kp := fi.Fact.Path()
		files[path.Join(pl.dir, pl.fname)] = body

		concepts[pl.dir] = append(concepts[pl.dir], conceptEntry{
			file:  pl.fname,
			title: firstNonEmpty(fi.Fact.Title, string(fi.Fact.Type)),
			typ:   okfType(kp, string(fi.Fact.Type)), // OKF type = singularized topic (concept.go)
		})
		registerDir(pl.dir)
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
	for d := range allDirs {
		var b strings.Builder
		if d == "" {
			b.WriteString("---\nokf_version: \"" + OKFVersion + "\"\n---\n\n")
		}
		b.WriteString("# " + indexTitle(d) + "\n\n")

		childDirs := sortedKeys(subdirs[d])
		for _, cd := range childDirs {
			link := relLink(d, path.Join(d, cd, "index.md"))
			b.WriteString("- [" + escapeLinkText(cd) + "](" + link + ")\n")
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

	files["log.md"] = RenderLog(log)

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
