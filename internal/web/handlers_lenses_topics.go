package web

import (
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"

	knomitfact "knomit/internal/fact"
	"knomit/internal/federate"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// lensTopicSource identifies which mount a union tree leaf came from. Unlike
// lensFactSource it carries no branch: the leaf's `path` already addresses the
// owning mount canonically, and the source badge only needs the repo name.
type lensTopicSource struct {
	Repo string `json:"repo"`
	ID   string `json:"id"`
}

// lensTopicChild is one child of a unified lens tree level. Directories are
// merged across mounts into plain {name,is_dir} nodes (shared topic
// vocabulary — no source, no path). Fact leaves are per-mount occurrences
// (server-uuid filenames never collide): type/title enriched from the owning
// mount's search index, `path` the canonical wire address (bare for the write
// mount, kb://<id12>/… for a read mount — what the frontend hands to
// openFact), and `source` the owning mount's tag.
type lensTopicChild struct {
	Name   string           `json:"name"`
	IsDir  bool             `json:"is_dir"`
	Type   string           `json:"type,omitempty"`
	Title  string           `json:"title,omitempty"`
	Path   string           `json:"path,omitempty"`
	Source *lensTopicSource `json:"source,omitempty"`
}

// lensTopicsResponse is the unified tree-level envelope. Flat (path +
// children), consistent with the other lens union reads; `path` echoes the
// ontology-rooted directory this level lists.
type lensTopicsResponse struct {
	Path     string           `json:"path"`
	Children []lensTopicChild `json:"children"`
}

// handleHALLensTopics serves GET /lenses/{lens}/topics and
// GET /lenses/{lens}/topics/* — ONE level of the unified, merged-by-topic
// ontology tree across the lens's write repo + N read mounts (lazy per-level,
// like the repo /topics browse; no eager full-tree build). It is the lens twin
// of topicHandler (handlers_topics.go), fanned out like the facts/search/stats
// siblings: federate.ReadTargetsFor selects the mounts (the ontology-aware
// topic skip applies once the path is kb/<topic>/…-deep), narrowByRepo applies
// repo= narrowing, and any mount error fails the WHOLE request (RFC §9.1 — a
// lens never silently shrinks its read set).
//
// Assumption: every mount shares the server's global ontology root ("kb") —
// knomit's ontology root is global config, not per-repo — so the single
// dirPath below maps uniformly onto every mount. (The topic skip additionally
// relies on the root being literally "kb": federate.topicOfPathFilter only
// recognizes kb/<topic>/… filters.)
func handleHALLensTopics(lister TopicLister, ontologyRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b := repos.BindingFromContext(r.Context())

		// nodePath is "" on the root route (no wildcard param).
		nodePath := chi.URLParam(r, "*")
		dirPath := ontologyRoot
		if nodePath != "" {
			dirPath = ontologyRoot + "/" + nodePath
		}

		// The ontology-rooted dirPath is the fan-out path filter: at the root
		// ("kb") and one level deep ("kb/decisions") it constrains nothing (a
		// prefix that could still match any topic), but from kb/<topic>/…
		// onward mounts whose ontology lacks the topic are skipped — the same
		// seam and semantics as the facts/search/stats siblings.
		targets, err := federate.ReadTargetsFor(b, dirPath)
		if err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
				err.Error(), r.URL.Path)
			return
		}

		// Optional repeatable `repo=<mount name>` narrows the fan-out (422 on
		// an unknown mount name) — the shared lens union-read filter.
		targets, ok := narrowByRepo(w, r, b, targets, r.URL.Query()["repo"])
		if !ok {
			return
		}

		// Split each mount's listing into directory names (deduped by shared
		// topic vocabulary — first-seen wins, dirs carry no per-mount payload)
		// and leaf entries (kept per-mount for now, deduped by winner below).
		var dirNames []string
		dirSeen := make(map[string]bool)
		leafLists := make([][]store.DirEntry, len(targets))
		for i, t := range targets {
			entries, err := lister.ListDir(r.Context(), t.RT.RI, t.RT.Branch, dirPath)
			if err != nil {
				writeStoreError(w, r, err, "Failed to list topics", t.RT.Branch)
				return
			}
			var leafEntries []store.DirEntry
			for _, e := range entries {
				// Private paths are excluded from discovery everywhere else
				// (indexer, Verify, the OKF exporter, the repo topicHandler
				// above) — the lens union tree is a reader too, and checking
				// the full path (not just e.Name) also covers a private
				// dirPath itself, since every child then inherits its
				// parent's private segment.
				if knomitfact.IsPrivatePath(dirPath + "/" + e.Name) {
					continue
				}
				if e.IsDir {
					if !dirSeen[e.Name] {
						dirSeen[e.Name] = true
						dirNames = append(dirNames, e.Name)
					}
					continue
				}
				leafEntries = append(leafEntries, e)
			}
			leafLists[i] = leafEntries
		}

		// A fact path CAN appear on more than one mount (a lens whose write repo
		// is a fork of a read-mounted upstream shares the fork's fact UUIDs), so
		// the tree must dedup leaves by the SAME write-first rule as the flat
		// facts union (writeFirstWinners) — otherwise the two union views
		// disagree and a shadowed read-mount copy the flat list hides becomes
		// openable here. Within one level the repo-relative path collides iff
		// the filename does, so the entry name is the winner key.
		winner := federate.WriteFirstWinners(targets, b.Write(), leafLists,
			func(e store.DirEntry) string { return e.Name })

		leaves := []lensTopicChild{}
		for i, t := range targets {
			for _, e := range leafLists[i] {
				if winner[e.Name] != i {
					continue // a different mount won this repo-relative path
				}
				fullPath := dirPath + "/" + e.Name
				child := lensTopicChild{
					Name: e.Name,
					Path: lensWirePath(b, t.RT, fullPath),
					Source: &lensTopicSource{
						Repo: t.RT.RI.Name(),
						ID:   federate.ID12(t.RT.RI.ID()),
					},
				}
				// Enrich with type/title from the owning mount's search index
				// (best-effort, mirroring the repo topicHandler).
				if fb, gerr := lister.GetByPath(r.Context(), t.RT.RI, t.RT.Branch, fullPath); gerr == nil && fb != nil {
					child.Type = fb.Type
					child.Title = fb.Title
				}
				leaves = append(leaves, child)
			}
		}

		// Deterministic order mirroring the repo tree's visual order:
		// directories first (deduped, name-sorted), then winning leaves by name
		// (each repo-relative name now appears exactly once).
		sort.Strings(dirNames)
		sort.SliceStable(leaves, func(i, j int) bool {
			return leaves[i].Name < leaves[j].Name
		})
		children := make([]lensTopicChild, 0, len(dirNames)+len(leaves))
		for _, name := range dirNames {
			children = append(children, lensTopicChild{Name: name, IsDir: true})
		}
		children = append(children, leaves...)

		hal.WriteHAL(w, http.StatusOK, lensTopicsResponse{Path: dirPath, Children: children})
	}
}
