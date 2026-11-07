package kb

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/rs/zerolog/log"

	"knomit/internal/corpus/repos"
	knomitfact "knomit/internal/domain/fact"
	"knomit/internal/edge/web/hal"
	"knomit/internal/edge/web/httpx"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/index"
	"knomit/internal/foundation/auth"
)

// maxResolveHandles caps one request: a chat turn cites a handful of facts,
// and the resolver reads each one it finds.
const maxResolveHandles = 100

// resolveExcerptRunes is how much of a fact's body the excerpt carries: enough
// for the hover card's two lines at its width, which it clamps.
const resolveExcerptRunes = 300

// resolveMaxBody bounds the request body: maxResolveHandles handles of a long
// kb:// path each.
const resolveMaxBody = 256 << 10

type resolveRequest struct {
	Handles []string `json:"handles"`
	Prefer  []string `json:"prefer"`
}

// resolvedFact is one answer: the handle as it was asked, and what the chat's
// fact link draws (the chip, the hover card) and opens (repo, branch, path).
type resolvedFact struct {
	Handle  string `json:"handle"`
	ID      string `json:"id"`
	Title   string `json:"title"`
	Type    string `json:"type"`
	Repo    string `json:"repo"`
	Branch  string `json:"branch"`
	Path    string `json:"path"`
	Excerpt string `json:"excerpt"`
	// Confidence is ABSENT (not 0) when the fact could not be read or
	// parsed: Browse's meta line omits a missing confidence, never shows 0.
	Confidence *float64 `json:"confidence,omitempty"`
	// What Browse's meta line (FactMetaLine) shows besides, so the chat's
	// hover card draws it with the same component: kind, origin, sources.
	Kind    string `json:"kind"`
	Origin  string `json:"origin"`
	Sources int    `json:"sources"`
	// Commit is set, to the full hash, on an item that answers a COMMIT hash
	// handle instead of a fact: then only handle, repo, branch and commit are
	// filled. Absent on a fact.
	Commit string `json:"commit,omitempty"`
}

var (
	shortFactID = regexp.MustCompile(`^[0-9a-f]{8}$`)
	commitHash  = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
)

// wantedHandle is one parsed handle: a short id (any path), or a path, in one
// named repo or in any; and/or a commit hash.
type wantedHandle struct {
	handle string
	id     string // the short id, when the handle is one
	path   string // the full fact path (root/…/name.md), when the handle names one
	repo   *repos.RepoInstance
	commit string // the hash (7-40 lowercase hex), when the handle may be a commit
}

// parseHandle reads the shapes a chat answer cites a fact by: an 8-hex short
// id; a path with or without the ontology root and the .md extension
// (invariants/web/gate/3fc77970); and kb://<repo id or name>/<path>. It also
// reads a commit hash (7 to 40 lowercase hex; an 8-hex one is tried as a fact
// first). Anything else, a dot path, and a kb:// URI naming no registered repo
// are not handles.
func parseHandle(h, root string, mgr *repos.Manager) (wantedHandle, bool) {
	w := wantedHandle{handle: h}
	s := strings.TrimSpace(h)
	if commitHash.MatchString(s) {
		w.commit = s
		if shortFactID.MatchString(s) {
			w.id = s
		}
		return w, true
	}
	if rest, ok := strings.CutPrefix(s, "kb://"); ok {
		store, p, ok := strings.Cut(rest, "/")
		if !ok || store == "" {
			return w, false
		}
		ri, found := mgr.ByRootID(store)
		if !found {
			ri = mgr.Get(store)
		}
		if ri == nil {
			return w, false
		}
		w.repo, s = ri, p
	}
	if !strings.Contains(s, "/") || strings.ContainsAny(s, " \t\n\\") || strings.Contains(s, "..") {
		return w, false
	}
	// The store's own spelling: root prefix, .md, lowercase after the root.
	s = knomitfact.NormalizePath(root, strings.TrimPrefix(s, "/"))
	if !strings.HasPrefix(s, root+"/") || knomitfact.IsPrivatePath(s) {
		return w, false
	}
	w.path = s
	return w, true
}

// handleFactsResolve serves POST /facts/resolve {handles, prefer?} →
// [resolvedFact]: the chat's fact links, resolved in one call.
func handleFactsResolve(mgr *repos.Manager, root string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req resolveRequest
		if !httpx.DecodeJSON(w, r, &req, resolveMaxBody) {
			return
		}
		if len(req.Handles) > maxResolveHandles {
			hal.WriteProblem(w, http.StatusBadRequest, "Too many handles",
				"at most 100 handles per request", r.URL.Path)
			return
		}
		out := resolveHandles(r.Context(), mgr, root, req.Handles, req.Prefer)
		body, err := json.Marshal(out)
		if err != nil {
			hal.WriteProblem(w, http.StatusInternalServerError, "Resolve failed", err.Error(), r.URL.Path)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}

// The resolver's two reads, swappable so a test can count them: the index
// lookup that says whether a repo holds a handle, and the git read of a fact
// that will be returned.
var (
	lookupIndex  = recentFacts
	readResolved = readFact
	// listCommits is the index read that expands a hash: up to two commits on
	// branch with that prefix (two means ambiguous).
	listCommits = func(ctx context.Context, svc *store.Service, branch, prefix string) ([]string, error) {
		return svc.HistoryQuery().CommitsWithPrefix(ctx, branch, prefix, 2)
	}
)

// hit is where a handle was found.
type hit struct {
	ri    *repos.RepoInstance
	entry index.RecentFactEntry
}

// resolveHandles answers handles for the caller in ctx.
//
// Which repos: only those the caller may read (auth.ReachesRepo, the reach a
// repo route is judged by), never the fleet repository, each at its read
// branch. A kb:// handle names its repo and is looked for only there.
//
// Order and collisions: prefer (repo names, 12-hex or full repo ids, or lens
// names, which stand for their members, write repo first) lists the repos the
// turn worked in. They are searched first, in that order, and the first that
// holds a handle answers it; a handle answered is not looked for again. Any
// handle still open is then looked for in every other reachable repo: found in
// exactly one it resolves, found in two or more it is AMBIGUOUS and omitted
// (the client leaves it as code) rather than picked by name. prefer never
// widens the reach.
//
// Cost: the index answers whether a repo holds a handle; only the facts that
// are returned are read from git. A done ctx stops the search: what is not
// yet resolved is omitted.
func resolveHandles(ctx context.Context, mgr *repos.Manager, root string, handles, prefer []string) []resolvedFact {
	var wanted []wantedHandle
	seen := map[string]bool{}
	for _, h := range handles {
		if seen[h] {
			continue
		}
		seen[h] = true
		if wh, ok := parseHandle(h, root, mgr); ok {
			wanted = append(wanted, wh)
		}
	}

	reach := map[string]*repos.RepoInstance{}
	var names []string
	mgr.ForEach(func(name string, ri *repos.RepoInstance) {
		if ri != nil && !ri.IsFleet() && auth.ReachesRepo(ctx, name) {
			reach[name] = ri
			names = append(names, name)
		}
	})
	sort.Strings(names)
	preferred := preferredRepos(mgr, prefer, reach)
	isPreferred := map[string]bool{}
	for _, ri := range preferred {
		isPreferred[ri.Name()] = true
	}

	won := map[int]hit{}       // wanted index → the answer
	found := map[int][]hit{}   // wanted index → matches outside prefer
	open := func(i int) bool { // still to look for
		_, done := won[i]
		return !done && len(found[i]) < 2
	}
	search := func(ri *repos.RepoInstance, collect bool) {
		var ids, paths []string
		var which []int
		for i, wh := range wanted {
			if !open(i) || (wh.repo != nil && wh.repo != ri) || (wh.id == "" && wh.path == "") {
				continue
			}
			which = append(which, i)
			if wh.id != "" {
				ids = append(ids, wh.id)
			} else {
				paths = append(paths, wh.path)
			}
		}
		if len(which) == 0 || ctx.Err() != nil {
			return
		}
		byPath, byID := lookupRepo(ctx, ri, ids, paths)
		for _, i := range which {
			wh := wanted[i]
			e, ok := byPath[wh.path]
			if wh.id != "" {
				e, ok = byID[wh.id]
			}
			switch {
			case !ok:
			case !collect || wh.repo != nil:
				won[i] = hit{ri, e}
			default:
				found[i] = append(found[i], hit{ri, e})
			}
		}
	}
	for _, ri := range preferred {
		search(ri, false)
	}
	for _, name := range names {
		if !isPreferred[name] {
			search(reach[name], true)
		}
	}
	for i, hs := range found {
		if _, done := won[i]; !done && len(hs) == 1 {
			won[i] = hs[0]
		}
	}

	// A commit hash that named no fact is looked up as a commit, but only when
	// the turn worked in exactly ONE repo the caller reaches: a hash names no
	// repo, so with none or several known it stays code.
	commits := map[int]resolvedFact{}
	if len(preferred) == 1 {
		ri := preferred[0]
		for i, wh := range wanted {
			if _, done := won[i]; done || len(found[i]) > 0 || wh.commit == "" || ctx.Err() != nil {
				continue
			}
			if full, ok := lookupCommit(ctx, ri, wh.commit); ok {
				commits[i] = resolvedFact{Handle: wh.handle, Repo: ri.Name(), Branch: ri.ReadBranch(), Commit: full}
			}
		}
	}

	out := []resolvedFact{}
	read := map[string]resolvedFact{}
	for i, wh := range wanted {
		if c, ok := commits[i]; ok {
			out = append(out, c)
			continue
		}
		h, ok := won[i]
		if !ok || ctx.Err() != nil {
			continue
		}
		key := h.ri.Name() + "\x00" + h.entry.Path
		f, done := read[key]
		if !done {
			if f, ok = readHit(ctx, h); !ok {
				continue
			}
			read[key] = f
		}
		f.Handle = wh.handle
		out = append(out, f)
	}
	return out
}

// preferredRepos turns prefer into reachable repo instances, in order, once
// each: a repo name, a repo id (12 hex or full, as kb:// carries it), or a
// lens name standing for its write repo then its reads. Anything else, and
// anything the caller cannot reach, is dropped.
func preferredRepos(mgr *repos.Manager, prefer []string, reach map[string]*repos.RepoInstance) []*repos.RepoInstance {
	var out []*repos.RepoInstance
	seen := map[string]bool{}
	add := func(ri *repos.RepoInstance) {
		if ri == nil || seen[ri.Name()] || reach[ri.Name()] != ri {
			return
		}
		seen[ri.Name()] = true
		out = append(out, ri)
	}
	for _, p := range prefer {
		if ri := mgr.Get(p); ri != nil {
			add(ri)
			continue
		}
		if ri, ok := mgr.ByRootID(p); ok {
			add(ri)
			continue
		}
		reg := mgr.LensRegistry()
		if reg == nil {
			continue
		}
		l, ok, err := reg.Get(p)
		if err != nil || !ok {
			continue
		}
		add(mgr.GetByUID(l.WriteUID))
		for _, rd := range l.Reads {
			add(mgr.GetByUID(rd.RepoUID))
		}
	}
	return out
}

// lookupRepo asks ri's index (no git read) which of ids (short ids) and paths
// it holds on its read branch. An id two files share answers with the first
// path in order. A repo whose index cannot be read holds nothing.
func lookupRepo(ctx context.Context, ri *repos.RepoInstance, ids, paths []string) (map[string]index.RecentFactEntry, map[string]index.RecentFactEntry) {
	branch := ri.ReadBranch()
	byPath := map[string]index.RecentFactEntry{}
	byID := map[string]index.RecentFactEntry{}
	for _, opts := range []index.SearchOptions{{PathsIn: paths}, {IDsIn: ids}} {
		if len(opts.PathsIn) == 0 && len(opts.IDsIn) == 0 {
			continue
		}
		// A literal LIMIT: SearchOptions{}'s zero Limit would be LIMIT 0.
		opts.Limit = 4 * maxResolveHandles
		entries, _, err := lookupIndex(ctx, ri, branch, opts)
		if err != nil {
			log.Warn().Err(err).Str("repo", ri.Name()).Msg("facts resolve: lookup failed")
			continue
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
		for _, e := range entries {
			if knomitfact.IsPrivatePath(e.Path) {
				continue
			}
			if len(opts.IDsIn) > 0 {
				id := strings.TrimSuffix(e.Path[strings.LastIndex(e.Path, "/")+1:], ".md")
				if _, dup := byID[id]; !dup {
					byID[id] = e
				}
			} else {
				byPath[e.Path] = e
			}
		}
	}
	return byPath, byID
}

// lookupCommit expands hash (7 to 40 hex) to the one commit on ri's read
// branch it abbreviates, and checks the repository holds that commit object.
// An abbreviation two commits share, a hash the branch does not have, and a
// store that cannot be read are all "no".
func lookupCommit(ctx context.Context, ri *repos.RepoInstance, hash string) (string, bool) {
	var full string
	if err := ri.WithRead(func(svc *store.Service) {
		hs, err := listCommits(ctx, svc, ri.ReadBranch(), hash)
		if err != nil {
			log.Warn().Err(err).Str("repo", ri.Name()).Msg("facts resolve: commit lookup failed")
			return
		}
		if len(hs) == 1 && svc.HasCommit(hs[0]) {
			full = hs[0]
		}
	}); err != nil {
		return "", false
	}
	return full, full != ""
}

// readHit reads the fact a handle resolved to, for its confidence and excerpt.
// A fact the index lists but that does not parse still links, by its index
// title and type; one gone since the lookup is not returned.
func readHit(ctx context.Context, h hit) (resolvedFact, bool) {
	ri, p, branch := h.ri, h.entry.Path, h.ri.ReadBranch()
	rf := resolvedFact{
		ID:     strings.TrimSuffix(p[strings.LastIndex(p, "/")+1:], ".md"),
		Title:  h.entry.Title,
		Type:   h.entry.Type,
		Repo:   ri.Name(),
		Branch: branch,
		Path:   p,
	}
	f, _, err := readResolved(ctx, ri, hal.Anchor{Branch: branch}, p, false)
	switch {
	case err == nil:
		rf.Title, rf.Type = f.Title, string(f.Type)
		conf := f.Confidence
		rf.Excerpt, rf.Confidence = excerptOf(f.Body, f.Title), &conf
		rf.Kind, rf.Origin, rf.Sources = string(f.Kind), string(f.ResolvedOrigin()), f.Sources
	case errors.Is(err, errFactNotFound):
		return rf, false
	default:
		log.Warn().Err(err).Str("repo", ri.Name()).Str("path", p).Msg("facts resolve: read failed")
	}
	return rf, true
}

// excerptOf is the start of a fact's body as one line of prose: the title
// heading (when the body repeats it) and blank lines dropped, whitespace
// collapsed, cut at resolveExcerptRunes on a rune boundary.
func excerptOf(body, title string) string {
	body = strings.TrimSpace(body)
	if first, rest, _ := strings.Cut(body, "\n"); strings.HasPrefix(first, "# ") &&
		(title == "" || strings.TrimSpace(strings.TrimPrefix(first, "# ")) == title) {
		body = rest
	}
	s := strings.Join(strings.Fields(body), " ")
	if utf8.RuneCountInString(s) <= resolveExcerptRunes {
		return s
	}
	return string([]rune(s)[:resolveExcerptRunes])
}
