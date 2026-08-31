package web

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// factSubProvider is the narrow interface for fact sub-resource handlers
// (commits, incoming, outgoing). Production wires it to commitsProvider +
// search index; tests inject stubs.
type factSubProvider interface {
	// LogPaginatedForPath returns a paged commit log for a specific fact path.
	LogPaginatedForPath(ctx context.Context, ri *repos.RepoInstance, branch, path string, limit int, after, from, before string) ([]store.LogEntryWithTags, string, string, error)

	// ExplainFact returns the incoming and outgoing ref graph for a fact.
	ExplainFact(ctx context.Context, ri *repos.RepoInstance, branch, path string) (store.ExplainResult, error)

	// IncomingAtCommit returns the version-aware incoming refs for a fact at
	// a specific commit: every (path, commit) whose ref resolved to this path
	// at that version.
	IncomingAtCommit(ctx context.Context, ri *repos.RepoInstance, branch, path, commitHash string) ([]store.RefSummary, error)

	// OutgoingAtCommit returns the version-aware outgoing refs for a fact at
	// a specific commit: the refs written by this version of the fact.
	OutgoingAtCommit(ctx context.Context, ri *repos.RepoInstance, branch, path, commitHash string) ([]store.RefSummary, error)

	// FactLiveAtCommit reports whether the fact is live (present, not
	// retracted) as of the pinned commit — the delete-RESPECTING check. Used
	// to 404 the commit-anchored sub-resources in lockstep with the
	// (no-fallback) fact read, so a fact retracted before this commit is gone
	// rather than surfacing a misleading empty 200.
	FactLiveAtCommit(ctx context.Context, ri *repos.RepoInstance, branch, path, commit string) (bool, error)

	// FactExistsAt reports whether the fact has ANY valid version ≤ commit
	// (stepping over retractions) — the fallback-before gate. With
	// ?fallback=before set, the edges follow the fact read: a retracted fact
	// still resolves to its last-valid version, so only a fact that never
	// existed in the ancestry 404s.
	FactExistsAt(ctx context.Context, ri *repos.RepoInstance, branch, path, commit string) (bool, error)
}

// defaultFactSubProvider implements factSubProvider using the store.
type defaultFactSubProvider struct{}

func (defaultFactSubProvider) LogPaginatedForPath(
	ctx context.Context,
	ri *repos.RepoInstance, branch, path string, limit int, after, from, before string,
) ([]store.LogEntryWithTags, string, string, error) {
	var (
		entries []store.LogEntryWithTags
		next    string
		prev    string
		err     error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		entries, next, prev, err = svc.HistoryQuery().LogPaginated(ctx, branch, path, limit, after, from, before)
	})
	return entries, next, prev, err
}

func (defaultFactSubProvider) ExplainFact(
	ctx context.Context,
	ri *repos.RepoInstance, branch, path string,
) (store.ExplainResult, error) {
	var (
		result store.ExplainResult
		err    error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		result, err = svc.GraphStore().ExplainFact(ctx, branch, path)
	})
	return result, err
}

func (defaultFactSubProvider) IncomingAtCommit(ctx context.Context, ri *repos.RepoInstance, branch, path, commitHash string) ([]store.RefSummary, error) {
	var (
		out []store.RefSummary
		err error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		out, err = svc.GraphStore().IncomingAtCommit(ctx, branch, path, commitHash)
	})
	return out, err
}

func (defaultFactSubProvider) OutgoingAtCommit(ctx context.Context, ri *repos.RepoInstance, branch, path, commitHash string) ([]store.RefSummary, error) {
	var (
		out []store.RefSummary
		err error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		out, err = svc.GraphStore().OutgoingAtCommit(ctx, branch, path, commitHash)
	})
	return out, err
}

func (defaultFactSubProvider) FactLiveAtCommit(ctx context.Context, ri *repos.RepoInstance, branch, path, commit string) (bool, error) {
	var (
		live bool
		err  error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		live, err = svc.FactQuery().FactLiveAtCommit(ctx, branch, path, commit)
	})
	return live, err
}

func (defaultFactSubProvider) FactExistsAt(ctx context.Context, ri *repos.RepoInstance, branch, path, commit string) (bool, error) {
	var (
		exists bool
		err    error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		exists, err = svc.FactQuery().FactExistsAt(ctx, branch, path, commit)
	})
	return exists, err
}

// factPresentAtCommitOr404 guards the commit-anchored /incoming and /outgoing
// sub-resources: it writes a 404 (fact absent as of this commit) or 500
// (lookup failed) and returns false when the caller should stop. The gate
// mirrors the commit-anchored fact read so a fact's edges 404 in lockstep with
// the fact itself:
//
//   - default (no fallback): FactLiveAtCommit — a fact retracted before this
//     commit is gone (404), not a misleading empty 200;
//   - ?fallback=before: FactExistsAt — the edges follow the fact's fallback
//     read, resolving a retracted fact to its last-valid version; only a fact
//     that never existed in the ancestry 404s.
func factPresentAtCommitOr404(
	subProvider factSubProvider,
	w http.ResponseWriter,
	r *http.Request,
	ri *repos.RepoInstance,
	a hal.Anchor,
	factPath string,
) bool {
	fallback := r.URL.Query().Get("fallback") == "before"

	var (
		present bool
		err     error
	)
	if fallback {
		present, err = subProvider.FactExistsAt(r.Context(), ri, a.Branch, factPath, a.Commit)
	} else {
		present, err = subProvider.FactLiveAtCommit(r.Context(), ri, a.Branch, factPath, a.Commit)
	}
	if err != nil {
		writeStoreError(w, r, err, "Failed to resolve fact", a.Branch)
		return false
	}
	if !present {
		hal.WriteProblem(w, http.StatusNotFound, "Fact not found",
			`no fact at path "`+factPath+`" on branch "`+a.Branch+`" at commit "`+a.Commit+`"`,
			r.URL.Path)
		return false
	}
	return true
}

// commitEntry is one item in the per-fact commit log collection.
type commitEntry struct {
	Commit    string             `json:"commit"`
	Date      string             `json:"date"`
	Message   string             `json:"message"`
	Operation string             `json:"operation,omitempty"`
	Author    store.CommitAuthor `json:"author"`
	Files     store.FileCounts   `json:"files,omitempty"`
	Links     hal.LinkMap        `json:"_links"`
}

// graphRefEntry is one item in the incoming/outgoing graph collection.
// Commit is required for HEAD and commit-anchored alike: it pins the entry
// to a specific source-version (incoming) or target-version (outgoing).
type graphRefEntry struct {
	Path        string      `json:"path"`
	Title       string      `json:"title"`
	Type        string      `json:"type,omitempty"`
	Commit      string      `json:"commit,omitempty"`
	CommittedAt int64       `json:"committed_at,omitempty"`
	Deleted     bool        `json:"deleted,omitempty"`
	Links       hal.LinkMap `json:"_links,omitempty"`
}

// handleFactCommits serves GET /repos/{repo}/branches/{branch}/facts/*/commits.
// Dispatched from handleHALFact when the wildcard path ends with "/commits".
func handleFactCommits(b hal.URLBuilder, provider factSubProvider, ri *repos.RepoInstance, repoName, branch, factPath string, w http.ResponseWriter, r *http.Request) {
	limit, ok := limitParam(w, r)
	if !ok {
		return
	}

	after := r.URL.Query().Get("after")
	from := r.URL.Query().Get("from")
	before := r.URL.Query().Get("before")

	entries, next, prev, err := provider.LogPaginatedForPath(r.Context(), ri, branch, factPath, limit, after, from, before)
	if err != nil {
		writeStoreError(w, r, err, "Failed to list commits", branch)
		return
	}
	if entries == nil {
		entries = []store.LogEntryWithTags{}
	}

	a := hal.Anchor{Branch: branch}
	commitsURL := b.FactCommits(repoName, a, factPath)
	selfURL := selfWithQuery(commitsURL, r)

	links := hal.LinkMap{"self": {Href: selfURL}}
	if next != "" {
		links["next"] = hal.Link{Href: commitsURL + buildCursorQuery(r, "after", next)}
	}
	if prev != "" {
		links["prev"] = hal.Link{Href: commitsURL + buildCursorQuery(r, "after", prev)}
	}

	branchURL := b.Branch(repoName, a)
	items := make([]commitEntry, 0, len(entries))
	for _, e := range entries {
		items = append(items, commitEntry{
			Commit:    e.Commit,
			Date:      e.Date,
			Message:   e.Message,
			Operation: e.Operation,
			Author:    e.Author,
			Files:     e.Files,
			Links: hal.LinkMap{
				"self": {Href: branchURL + "/commits/" + e.Commit},
			},
		})
	}

	view := hal.CollectionView[commitEntry]{
		Count: len(items),
		Links: links,
		Embedded: map[string][]commitEntry{
			"commits": items,
		},
	}
	hal.WriteHAL(w, http.StatusOK, view)
}

// handleFactIncoming serves GET /repos/{repo}/branches/{branch}/facts/*/incoming.
func handleFactIncoming(b hal.URLBuilder, provider factSubProvider, ri *repos.RepoInstance, repoName, branch, factPath string, w http.ResponseWriter, r *http.Request) {
	result, err := provider.ExplainFact(r.Context(), ri, branch, factPath)
	if err != nil {
		writeStoreError(w, r, err, "Failed to load incoming refs", branch)
		return
	}

	a := hal.Anchor{Branch: branch}
	selfURL := b.FactIncoming(repoName, a, factPath)

	items := buildGraphRefItems(b, repoName, a, result.Incoming)
	view := hal.CollectionView[graphRefEntry]{
		Count: len(items),
		Links: hal.LinkMap{"self": {Href: selfURL}},
		Embedded: map[string][]graphRefEntry{
			"refs": items,
		},
	}
	hal.WriteHAL(w, http.StatusOK, view)
}

// handleFactOutgoing serves GET /repos/{repo}/branches/{branch}/facts/*/outgoing.
func handleFactOutgoing(b hal.URLBuilder, provider factSubProvider, ri *repos.RepoInstance, repoName, branch, factPath string, w http.ResponseWriter, r *http.Request) {
	result, err := provider.ExplainFact(r.Context(), ri, branch, factPath)
	if err != nil {
		writeStoreError(w, r, err, "Failed to load outgoing refs", branch)
		return
	}

	a := hal.Anchor{Branch: branch}
	selfURL := b.FactOutgoing(repoName, a, factPath)

	items := buildGraphRefItems(b, repoName, a, result.Outgoing)
	view := hal.CollectionView[graphRefEntry]{
		Count: len(items),
		Links: hal.LinkMap{"self": {Href: selfURL}},
		Embedded: map[string][]graphRefEntry{
			"refs": items,
		},
	}
	hal.WriteHAL(w, http.StatusOK, view)
}

// buildGraphRefItems converts []store.RefSummary into []graphRefEntry. Each
// item's _links.self is anchored to the entry's own commit (incoming → the
// source's commit; outgoing → the target's commit) so callers can navigate
// directly to the version that produced or received the ref-event.
func buildGraphRefItems(b hal.URLBuilder, repoName string, a hal.Anchor, refs []store.RefSummary) []graphRefEntry {
	items := make([]graphRefEntry, 0, len(refs))
	for _, ref := range refs {
		item := graphRefEntry{
			Path:        ref.Path,
			Title:       ref.Title,
			Type:        ref.Type,
			Commit:      ref.Commit,
			CommittedAt: ref.CommittedAt,
			Deleted:     ref.Deleted,
		}
		if !ref.Deleted {
			anchor := hal.Anchor{Branch: a.Branch, Commit: ref.Commit}
			item.Links = hal.LinkMap{"self": {Href: b.Fact(repoName, anchor, ref.Path)}}
		}
		items = append(items, item)
	}
	return items
}

// factSubResources are the suffixes a facts/* wildcard may address instead of
// the fact itself.
var factSubResources = [...]string{"commits", "incoming", "outgoing"}

// splitFactSubResource splits a facts/* wildcard path into the fact path and
// the sub-resource it addresses, or (path, "") when it addresses the fact.
//
// Splitting is separated from dispatching because the lens route has to do it
// FIRST: its path may be kb://<id12>-qualified, and the mount must be resolved
// from the fact path with the suffix already off. Leaving the two fused is what
// let "<uuid>.md/incoming" travel into the store as a fact path (issue #178).
func splitFactSubResource(path string) (factPath, sub string) {
	for _, s := range factSubResources {
		if strings.HasSuffix(path, "/"+s) {
			return strings.TrimSuffix(path, "/"+s), s
		}
	}
	return path, ""
}

// dispatchFactSubResource checks if a fact wildcard path ends with a known
// sub-resource suffix and dispatches accordingly. Returns true if dispatched.
func dispatchFactSubResource(
	b hal.URLBuilder,
	subProvider factSubProvider,
	ri *repos.RepoInstance,
	repoName, branch string,
	w http.ResponseWriter,
	r *http.Request,
) bool {
	factPath, sub := splitFactSubResource(chi.URLParam(r, "*"))
	return dispatchResolvedFactSubResource(b, subProvider, ri, repoName, branch, factPath, sub, w, r)
}

// dispatchResolvedFactSubResource dispatches an ALREADY-split sub-resource
// against an already-resolved mount. The repo route reaches it through
// dispatchFactSubResource; the lens route calls it directly, after resolving
// the addressed mount itself.
func dispatchResolvedFactSubResource(
	b hal.URLBuilder,
	subProvider factSubProvider,
	ri *repos.RepoInstance,
	repoName, branch, factPath, sub string,
	w http.ResponseWriter,
	r *http.Request,
) bool {
	switch sub {
	case "commits":
		handleFactCommits(b, subProvider, ri, repoName, branch, factPath, w, r)
	case "incoming":
		handleFactIncoming(b, subProvider, ri, repoName, branch, factPath, w, r)
	case "outgoing":
		handleFactOutgoing(b, subProvider, ri, repoName, branch, factPath, w, r)
	default:
		return false
	}
	return true
}
