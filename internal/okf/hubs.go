package okf

import (
	"path"
	"sort"
	"strings"
)

// entityHubMinFacts is the floor for generating an entity hub. knomit entities
// are numerous and long-tailed (file paths, symbol names); hubs for one- or
// two-fact entities would multiply documents without giving a reader a view
// worth opening.
const entityHubMinFacts = 3

// hubDir names the generated cross-cutting views. They are DERIVED documents:
// regenerated with every bundle and keyed on the mapper version like everything
// else, never authored.
const (
	domainsDir  = "domains"
	entitiesDir = "entities"
)

// hubMember is one fact referenced from a hub document.
type hubMember struct {
	title      string
	typ        string
	bundlePath string
}

// buildHubs generates the cross-cutting views OKF has no native structure for:
// "every fact in domain X" and "every fact touching entity Y".
//
// The spec defines no aggregation format (§5) and notes a consumer MAY
// synthesize a tag view at consumption time — but that only helps consumers
// that HAVE a runtime. A human reading the bundle on GitHub has none, which is
// exactly where these pay off. They stay fully conformant because any .md with
// a non-empty `type` is a valid concept document.
//
// Returns the generated files keyed by bundle path. Deterministic: every
// grouping and member list is sorted.
func buildHubs(facts []FactInput, pathOf map[string]string) map[string][]byte {
	byDomain := map[string][]hubMember{}
	byEntity := map[string][]hubMember{}

	for _, fi := range facts {
		bp, ok := pathOf[fi.Fact.Path()]
		if !ok {
			continue // skipped fact: never appears in a hub
		}
		m := hubMember{
			title:      firstNonEmpty(fi.Fact.Title, string(fi.Fact.Type)),
			typ:        okfType(fi.Fact.Path(), string(fi.Fact.Type)),
			bundlePath: bp,
		}
		for _, d := range dedupe(fi.Fact.Domain) {
			byDomain[d] = append(byDomain[d], m)
		}
		for _, e := range dedupe(fi.Fact.Entities) {
			byEntity[e] = append(byEntity[e], m)
		}
	}

	files := map[string][]byte{}

	// emit builds hub pages and the index entries pointing at them.
	//
	// A group of ONE gets no page: a hub whose whole content is a single link
	// is an extra click for nothing. Instead the index links that fact
	// directly, so coverage stays complete (every key is answerable) without
	// the dead pages — 92 of this corpus's 190 domains have exactly one fact.
	// Groups below minPage and larger than one are listed inline on the index.
	emit := func(dir, okfTypeName, label string, groups map[string][]hubMember, minPage int) []indexEntry {
		names := make([]string, 0, len(groups))
		for n := range groups {
			names = append(names, n)
		}
		sort.Strings(names)

		// Slugified filenames can collide (entities include paths like
		// web/src/state.ts). Disambiguate in sorted order so the result is
		// stable across runs.
		used := map[string]bool{}
		var entries []indexEntry
		for _, n := range names {
			members := groups[n]
			switch {
			case len(members) == 1:
				m := members[0]
				entries = append(entries, indexEntry{
					name:   n,
					target: relLink(dir, m.bundlePath),
					note:   m.typ,
				})
			case len(members) >= minPage:
				base := slugify(n)
				if base == "" {
					continue
				}
				fname := base + ".md"
				for i := 2; used[fname]; i++ {
					fname = base + "-" + itoa(i) + ".md"
				}
				used[fname] = true
				files[path.Join(dir, fname)] = renderHub(okfTypeName, label, n, members, dir)
				entries = append(entries, indexEntry{
					name:   n,
					target: fname,
					note:   itoa(len(members)) + " facts",
				})
			}
		}
		return entries
	}

	// Domains are the curated filtering axis, so every one is represented.
	domainEntries := emit(domainsDir, "Domain Overview", "domain", byDomain, 2)
	// Entities are a long tail of symbols and file paths; only those with
	// enough facts to be worth a page are surfaced.
	entityEntries := emit(entitiesDir, "Entity Index", "entity", byEntity, entityHubMinFacts)

	if len(domainEntries) > 0 {
		files[path.Join(domainsDir, "index.md")] = renderHubIndex("Domains",
			"Every domain tag in this knowledge base. Tags on a single fact link straight to it.", domainEntries)
	}
	if len(entityEntries) > 0 {
		files[path.Join(entitiesDir, "index.md")] = renderHubIndex("Entities",
			"Code symbols, files, and tools referenced by at least "+itoa(entityHubMinFacts)+" facts.", entityEntries)
	}
	return files
}

// indexEntry is one line on a hub directory's index.
type indexEntry struct {
	name   string
	target string
	note   string
}

// renderHub renders one hub concept document.
func renderHub(okfTypeName, label, name string, members []hubMember, fromDir string) []byte {
	sort.Slice(members, func(i, j int) bool {
		if members[i].title != members[j].title {
			return members[i].title < members[j].title
		}
		return members[i].bundlePath < members[j].bundlePath
	})

	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("type: " + okfTypeName + "\n")
	b.WriteString("title: " + yamlScalar(name) + "\n")
	b.WriteString("tags:\n  - " + yamlScalar(name) + "\n")
	b.WriteString("knomit_hub: " + label + "\n")
	b.WriteString("knomit_hub_key: " + yamlScalar(name) + "\n")
	b.WriteString("knomit_member_count: " + itoa(len(members)) + "\n")
	b.WriteString("---\n\n")
	b.WriteString("# " + name + "\n\n")
	b.WriteString(itoa(len(members)) + " facts reference this " + label + ".\n\n")
	for _, m := range members {
		b.WriteString("- [" + escapeLinkText(m.title) + "](" + relLink(fromDir, m.bundlePath) + ") — " + m.typ + "\n")
	}
	return []byte(b.String())
}

// renderHubIndex renders the index.md for a hub directory. Entries show the
// real key (e.g. "web/src/state.ts"), not its slugified filename.
func renderHubIndex(heading, blurb string, entries []indexEntry) []byte {
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	var b strings.Builder
	b.WriteString("# " + heading + "\n\n")
	b.WriteString(blurb + "\n\n")
	for _, e := range entries {
		b.WriteString("- [" + escapeLinkText(e.name) + "](" + e.target + ")")
		if e.note != "" {
			b.WriteString(" — " + e.note)
		}
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// yamlScalar quotes a scalar when it could otherwise be misparsed (paths with
// colons, leading symbols, etc.).
func yamlScalar(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, ":#{}[],&*?|-<>=!%@`\"'\n") || strings.TrimSpace(s) != s {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", " ").Replace(s) + `"`
	}
	return s
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	return string(d)
}
