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
	"knomit/internal/engine/store/index"
	"knomit/internal/foundation/auth"
)

// FactsResolvePath is the resolver's route under the API base. The read-only
// and write gates exempt exactly this path (it only reads), and the root
// document links to it.
const FactsResolvePath = "/facts/resolve"

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
}

// resolvedFact is one answer: the handle as it was asked, and what the chat's
// fact link draws (the chip, the hover card) and opens (repo, branch, path).
type resolvedFact struct {
	Handle     string  `json:"handle"`
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	Type       string  `json:"type"`
	Repo       string  `json:"repo"`
	Branch     string  `json:"branch"`
	Path       string  `json:"path"`
	Excerpt    string  `json:"excerpt"`
	Confidence float64 `json:"confidence"`
}

var shortFactID = regexp.MustCompile(`^[0-9a-f]{8}$`)

// wantedHandle is one parsed handle: a short id (any path), or a path, in one
// named repo or in any.
type wantedHandle struct {
	handle string
	id     string // the short id, when the handle is one
	path   string // the full fact path (root/…/name.md), when the handle names one
	repo   *repos.RepoInstance
}

// parseHandle reads the shapes a chat answer cites a fact by: an 8-hex short
// id; a path with or without the ontology root and the .md extension
// (invariants/web/gate/3fc77970); and kb://<repo id or name>/<path>. Anything
// else, a dot path, and a kb:// URI naming no registered repo are not handles.
func parseHandle(h, root string, mgr *repos.Manager) (wantedHandle, bool) {
	w := wantedHandle{handle: h}
	s := strings.TrimSpace(h)
	if shortFactID.MatchString(s) {
		w.id = s
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
	s = strings.TrimPrefix(s, "/")
	if !strings.HasPrefix(s, root+"/") {
		s = root + "/" + s
	}
	if !strings.HasSuffix(s, ".md") {
		s += ".md"
	}
	if knomitfact.IsPrivatePath(s) {
		return w, false
	}
	w.path = s
	return w, true
}

// handleFactsResolve serves POST /facts/resolve {handles} → [resolvedFact]:
// the chat's fact links, resolved in one call per turn.
//
// A handle that names no repo is looked for in every repo the caller may read
// (auth.ReachesRepo, the same reach a repo route is judged by), in name order,
// at each repo's read branch; the first match answers. A handle in a repo the
// caller cannot read, or that matches nothing, is OMITTED: the client leaves
// it as code. The fleet repository is never searched (no generic route serves
// it). Results follow the order of the handles asked, once per distinct
// handle.
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
		ctx := r.Context()
		var wanted []wantedHandle
		seen := map[string]bool{}
		for _, h := range req.Handles {
			if seen[h] {
				continue
			}
			seen[h] = true
			if wh, ok := parseHandle(h, root, mgr); ok {
				wanted = append(wanted, wh)
			}
		}

		var names []string
		mgr.ForEach(func(name string, ri *repos.RepoInstance) {
			if ri != nil && !ri.IsFleet() && auth.ReachesRepo(ctx, name) {
				names = append(names, name)
			}
		})
		sort.Strings(names)

		// found[repo][path] is a fact the repo holds; byID[repo][id] its path.
		found := map[string]map[string]resolvedFact{}
		byID := map[string]map[string]string{}
		for _, name := range names {
			ri := mgr.Get(name)
			if ri == nil {
				continue
			}
			var ids, paths []string
			for _, wh := range wanted {
				switch {
				case wh.repo != nil && wh.repo != ri:
				case wh.id != "":
					ids = append(ids, wh.id)
				default:
					paths = append(paths, wh.path)
				}
			}
			if len(ids) == 0 && len(paths) == 0 {
				continue
			}
			facts, idPaths := lookupRepo(ctx, ri, ids, paths)
			found[name], byID[name] = facts, idPaths
		}

		out := []resolvedFact{}
		for _, wh := range wanted {
			for _, name := range names {
				if wh.repo != nil && wh.repo.Name() != name {
					continue
				}
				path := wh.path
				if wh.id != "" {
					path = byID[name][wh.id]
				}
				if f, ok := found[name][path]; ok && path != "" {
					f.Handle = wh.handle
					out = append(out, f)
					break
				}
			}
		}
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

// lookupRepo finds which of ids (short ids) and paths exist on ri's read branch
// and reads each one found. A repo whose store cannot be read answers nothing:
// its handles stay unresolved rather than failing the whole turn's links.
func lookupRepo(ctx context.Context, ri *repos.RepoInstance, ids, paths []string) (map[string]resolvedFact, map[string]string) {
	branch := ri.ReadBranch()
	facts := map[string]resolvedFact{}
	idPaths := map[string]string{}
	var hits []index.RecentFactEntry
	for _, opts := range []index.SearchOptions{{PathsIn: paths}, {IDsIn: ids}} {
		if len(opts.PathsIn) == 0 && len(opts.IDsIn) == 0 {
			continue
		}
		// A literal LIMIT: SearchOptions{}'s zero Limit would be LIMIT 0.
		opts.Limit = 4 * maxResolveHandles
		entries, _, err := recentFacts(ctx, ri, branch, opts)
		if err != nil {
			log.Warn().Err(err).Str("repo", ri.Name()).Msg("facts resolve: lookup failed")
			continue
		}
		// Path order, so an id two files share resolves the same way every time.
		sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
		for _, e := range entries {
			if knomitfact.IsPrivatePath(e.Path) {
				continue
			}
			id := strings.TrimSuffix(e.Path[strings.LastIndex(e.Path, "/")+1:], ".md")
			if _, dup := idPaths[id]; !dup && len(opts.IDsIn) > 0 {
				idPaths[id] = e.Path
			}
			hits = append(hits, e)
		}
	}
	for _, e := range hits {
		p := e.Path
		if _, done := facts[p]; done {
			continue
		}
		rf := resolvedFact{
			ID:     strings.TrimSuffix(p[strings.LastIndex(p, "/")+1:], ".md"),
			Title:  e.Title,
			Type:   e.Type,
			Repo:   ri.Name(),
			Branch: branch,
			Path:   p,
		}
		// The body (excerpt) and confidence need the fact itself. A fact the
		// index lists but that does not parse still links, by its index
		// title and type, without them.
		f, _, err := readFact(ctx, ri, hal.Anchor{Branch: branch}, p, false)
		switch {
		case err == nil:
			rf.Title, rf.Type = f.Title, string(f.Type)
			rf.Excerpt, rf.Confidence = excerptOf(f.Body, f.Title), f.Confidence
		case errors.Is(err, errFactNotFound):
			continue
		default:
			log.Warn().Err(err).Str("repo", ri.Name()).Str("path", p).Msg("facts resolve: read failed")
		}
		facts[p] = rf
	}
	return facts, idPaths
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
