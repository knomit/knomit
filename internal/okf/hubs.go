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

// domainHubMinFacts is the floor for a domain PAGE. A group of one gets no
// page — a hub whose whole body is a single link is a wasted click — but it is
// still listed on the directory index, linking straight to its one fact, so
// every domain stays answerable.
const domainHubMinFacts = 2

// Derived views live under their own top-level directory, kept strictly apart
// from the authored ontology under kb/. Two reasons: a knomit topic could
// legitimately be named "domains" or "entities" and would otherwise collide,
// and a reader can tell at a glance which documents were authored and which
// were generated.
const (
	viewsRoot   = "views"
	domainsDir  = viewsRoot + "/domains"
	entitiesDir = viewsRoot + "/entities"
)

// hubMember is one fact referenced from a hub document.
type hubMember struct {
	title      string
	typ        string
	bundlePath string
}

// hubPlan names the hub pages that WILL exist. It is computed from the facts
// alone — before any document is rendered — because concept documents link to
// their domain and entity pages, and those links must be known at render time.
type hubPlan struct {
	domain map[string]string // key -> bundle path, only for keys that get a page
	entity map[string]string
}

// pageFor returns the bundle path of the hub page for a key, if one exists.
func (p hubPlan) pageFor(kind, key string) (string, bool) {
	var m map[string]string
	switch kind {
	case "domain":
		m = p.domain
	case "entity":
		m = p.entity
	}
	v, ok := m[key]
	return v, ok
}

// planHubs decides which hub pages exist and where. Deterministic: keys are
// processed in sorted order, so slug collisions disambiguate identically on
// every run.
func planHubs(facts []FactInput) hubPlan {
	domainCount := map[string]int{}
	entityCount := map[string]int{}
	for _, fi := range facts {
		for _, d := range dedupe(fi.Fact.Domain) {
			domainCount[d]++
		}
		for _, e := range dedupe(fi.Fact.Entities) {
			entityCount[e]++
		}
	}
	return hubPlan{
		domain: assignPaths(domainCount, domainsDir, domainHubMinFacts),
		entity: assignPaths(entityCount, entitiesDir, entityHubMinFacts),
	}
}

// assignPaths maps each qualifying key to a collision-free bundle path.
func assignPaths(counts map[string]int, dir string, min int) map[string]string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	used := map[string]bool{}
	out := map[string]string{}
	for _, k := range keys {
		if counts[k] < min {
			continue
		}
		base := slugify(k)
		if base == "" {
			continue
		}
		fname := base + ".md"
		for i := 2; used[fname]; i++ {
			fname = base + "-" + itoa(i) + ".md"
		}
		used[fname] = true
		out[k] = path.Join(dir, fname)
	}
	return out
}

// renderHubs produces the hub pages and their directory indexes.
//
// The spec defines no aggregation structure (§5) and notes a consumer MAY
// synthesize a tag view at consumption time — but that only helps consumers
// that HAVE a runtime. A human reading the bundle on GitHub has none, which is
// exactly where these pay off. They stay fully conformant because any .md with
// a non-empty `type` is a valid concept document.
func renderHubs(plan hubPlan, facts []FactInput, pathOf map[string]string) map[string][]byte {
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
	emit := func(dir, okfTypeName, label string, groups map[string][]hubMember, pages map[string]string, listSingletons bool) []indexEntry {
		keys := make([]string, 0, len(groups))
		for k := range groups {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		var entries []indexEntry
		for _, k := range keys {
			members := groups[k]
			if p, ok := pages[k]; ok {
				files[p] = renderHub(okfTypeName, label, k, members, dir)
				entries = append(entries, indexEntry{
					name: k, target: path.Base(p), note: itoa(len(members)) + " facts",
				})
				continue
			}
			// No page for this key. Link its single fact directly so the key
			// stays answerable; skip entirely for the long-tail axis.
			if listSingletons && len(members) == 1 {
				entries = append(entries, indexEntry{
					name: k, target: relLink(dir, members[0].bundlePath), note: members[0].typ,
				})
			}
		}
		return entries
	}

	domainEntries := emit(domainsDir, "Domain Overview", "domain", byDomain, plan.domain, true)
	entityEntries := emit(entitiesDir, "Entity Index", "entity", byEntity, plan.entity, false)

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
