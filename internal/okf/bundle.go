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
func Build(repo RepoIdentity, facts []FactInput, log []LogEntry) (Bundle, SkipReport) {
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

	for _, fi := range facts {
		body, err := Concept(fi, repo)
		if err != nil {
			skips.Skipped++
			skips.Reasons = append(skips.Reasons, fi.Fact.Path()+": "+err.Error())
			continue
		}
		kp := fi.Fact.Path() // e.g. kb/decisions/okf/scope/d9d6557d.md
		rel := strings.TrimPrefix(kp, "kb/")
		dir := parentDir(rel) // decisions/okf/scope
		uuid8 := strings.TrimSuffix(path.Base(rel), ".md")
		lastCat := path.Base(dir)
		fname := Slug(fi.Fact.Title, lastCat, uuid8)
		bundlePath := path.Join(dir, fname)
		files[bundlePath] = body

		concepts[dir] = append(concepts[dir], conceptEntry{
			file:  fname,
			title: firstNonEmpty(fi.Fact.Title, string(fi.Fact.Type)),
			typ:   okfType(kp, string(fi.Fact.Type)), // OKF type = singularized topic (concept.go)
		})
		registerDir(dir)
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
			link := "/" + path.Join(d, cd, "index.md")
			b.WriteString("- [" + cd + "](" + link + ")\n")
		}
		ces := concepts[d]
		sort.Slice(ces, func(i, j int) bool { return ces[i].file < ces[j].file })
		for _, ce := range ces {
			b.WriteString("- " + ce.title + " — " + ce.typ + "\n")
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

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
