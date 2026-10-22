package okf

import (
	"path"
	"sort"
	"strconv"
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

// reservedHubFile is the directory's own index, which renderHubs writes AFTER
// the hub pages. A key that slugifies to "index" must not be allowed to claim
// it: the index would silently overwrite that key's hub page, and every concept
// document in the group would link to the directory listing instead.
const reservedHubFile = "index.md"

// assignPaths maps each qualifying key to a collision-free bundle path.
func assignPaths(counts map[string]int, dir string, min int) map[string]string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	used := map[string]bool{reservedHubFile: true}
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
			fname = base + "-" + strconv.Itoa(i) + ".md"
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
					name: k, target: path.Base(p), note: pluralFacts(len(members)),
				})
				continue
			}
			// No page for this key. Link its single fact directly so the key
			// stays answerable; skip entirely for the long-tail axis. The note
			// carries the fact's TITLE, since the entry's label is the key and
			// the reader would otherwise not know what the link opens.
			if listSingletons && len(members) == 1 {
				entries = append(entries, indexEntry{
					name: k, target: relLink(dir, members[0].bundlePath), note: members[0].title,
				})
			}
		}
		return entries
	}

	domainEntries := emit(domainsDir, "Domain Overview", "domain", byDomain, plan.domain, true)
	entityEntries := emit(entitiesDir, "Entity Index", "entity", byEntity, plan.entity, false)

	if len(domainEntries) > 0 {
		files[path.Join(domainsDir, reservedHubFile)] = renderHubIndex("Domains",
			"Every domain tag in this knowledge base. Tags on a single fact link straight to it.", domainEntries)
	}
	if len(entityEntries) > 0 {
		files[path.Join(entitiesDir, reservedHubFile)] = renderHubIndex("Entities",
			"Code symbols, files, and tools referenced by at least "+strconv.Itoa(entityHubMinFacts)+" facts.", entityEntries)
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
	b.WriteString("knomit_member_count: " + strconv.Itoa(len(members)) + "\n")
	b.WriteString("---\n\n")
	b.WriteString("# " + name + "\n\n")
	b.WriteString(pluralFacts(len(members)) + " reference this " + label + ".\n\n")
	for _, m := range members {
		b.WriteString("- [" + escapeLinkText(m.title) + "](" + relLink(fromDir, m.bundlePath) + ") — " + m.typ + "\n")
	}
	return []byte(b.String())
}

// alphaIndexMinEntries is the point past which a flat list stops being
// navigable and letter sections plus a jump bar earn their extra markup. Real
// bundles carry hundreds of domains and entities.
const alphaIndexMinEntries = 25

// renderHubIndex renders the index.md for a hub directory. Entries show the
// real key (e.g. "web/src/state.ts"), not its slugified filename. Past
// alphaIndexMinEntries the list is bucketed by initial letter, with a jump bar
// of markdown anchors at the top.
func renderHubIndex(heading, blurb string, entries []indexEntry) []byte {
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	var b strings.Builder
	b.WriteString("# " + heading + "\n\n")
	b.WriteString(blurb + "\n\n")

	writeEntry := func(e indexEntry) {
		b.WriteString("- [" + escapeLinkText(e.name) + "](" + e.target + ")")
		if e.note != "" {
			b.WriteString(" — " + e.note)
		}
		b.WriteString("\n")
	}

	if len(entries) < alphaIndexMinEntries {
		for _, e := range entries {
			writeEntry(e)
		}
		return []byte(b.String())
	}

	buckets := map[string][]indexEntry{}
	var order []string
	for _, e := range entries {
		k := bucketOf(e.name)
		if _, seen := buckets[k]; !seen {
			order = append(order, k)
		}
		buckets[k] = append(buckets[k], e)
	}
	// Letters first, then the catch-all buckets — a plain string sort would
	// file "Other" between "O" and "P".
	sort.Slice(order, func(i, j int) bool {
		return bucketRank(order[i]) < bucketRank(order[j])
	})

	jump := make([]string, 0, len(order))
	for _, k := range order {
		jump = append(jump, "["+k+"](#"+anchorFor(k)+")")
	}
	b.WriteString("**Jump to:** " + strings.Join(jump, " · ") + "\n")

	for _, k := range order {
		b.WriteString("\n## " + k + "\n\n")
		for _, e := range buckets[k] {
			writeEntry(e)
		}
	}
	return []byte(b.String())
}

// bucketOf returns the letter section a key belongs to. Digits collapse into
// one bucket and everything else (punctuation, non-Latin) into "Other", so
// every key lands somewhere.
func bucketOf(name string) string {
	if name == "" {
		return "Other"
	}
	c := name[0]
	switch {
	case c >= 'a' && c <= 'z':
		return string(c - 32)
	case c >= 'A' && c <= 'Z':
		return string(c)
	case c >= '0' && c <= '9':
		return "0-9"
	default:
		return "Other"
	}
}

// bucketRank orders the letter sections: A–Z, then digits, then everything
// else, so the catch-all buckets sit at the end where a reader expects them.
func bucketRank(b string) string {
	switch b {
	case "0-9":
		return "|" + b // after 'Z'
	case "Other":
		return "}" + b
	default:
		return b
	}
}

// anchorFor mirrors how markdown renderers slugify a heading into a fragment
// id: lowercased, spaces to hyphens.
func anchorFor(heading string) string {
	return strings.ToLower(strings.ReplaceAll(heading, " ", "-"))
}

// renderViewsIndex lists everything generated under views/: the hub
// directories and the single-file digest pages. The generic per-directory pass
// only knows about subdirectories, so the digest files would otherwise be
// unreachable by navigation.
func renderViewsIndex(files map[string][]byte, digests []indexEntry) []byte {
	dirSeen := map[string]bool{}
	var dirs []string
	for p := range files {
		rel := strings.TrimPrefix(p, viewsRoot+"/")
		if rel == p || rel == "index.md" {
			continue
		}
		if i := strings.IndexByte(rel, '/'); i >= 0 {
			if d := rel[:i]; !dirSeen[d] {
				dirSeen[d] = true
				dirs = append(dirs, d)
			}
		}
	}
	sort.Strings(dirs)
	sort.Slice(digests, func(i, j int) bool { return digests[i].name < digests[j].name })

	var b strings.Builder
	b.WriteString("# Views\n\n")
	b.WriteString("Cross-cutting views generated from the knowledge base. " +
		"They are derived: regenerated with every export, never authored.\n\n")
	for _, d := range dirs {
		b.WriteString("- [" + escapeLinkText(d) + "](" + d + "/index.md)\n")
	}
	for _, e := range digests {
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

// dedupe removes blanks and repeats and SORTS. Use it where input order carries
// no meaning — the domain and entity tag sets, which render as alphabetical
// lists. Where the order IS meaningful, use dedupeStable.
func dedupe(in []string) []string {
	out := dedupeStable(in)
	sort.Strings(out)
	return out
}

// dedupeStable removes blanks and repeats while KEEPING first-seen order. A
// fact's refs are authored in a meaningful sequence, and both the Citations
// list and sources[] preserve it — sorting them would reorder a reader's
// citations for no reason.
func dedupeStable(in []string) []string {
	seen := make(map[string]bool, len(in))
	var out []string
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// pluralFacts renders a fact count with correct agreement.
func pluralFacts(n int) string {
	if n == 1 {
		return "1 fact"
	}
	return strconv.Itoa(n) + " facts"
}
