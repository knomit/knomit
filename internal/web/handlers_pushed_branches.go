package web

// Pushed branches (F11 UI merge): a peer pushes its own agent branch to this
// host over /git (F11); here the host's operator lists those branches and
// merges one into this instance's agent branch. The host's ordinary sync then
// carries the merge upstream. Nothing here merges on its own: the only write
// is the POST, which needs `operator` and the tip the operator reviewed.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-git/go-git/v5/plumbing"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// pushedBranchView is one row of GET /repos/{repo}/pushed-branches.
type pushedBranchView struct {
	Name              string      `json:"name"`
	Tip               string      `json:"tip"`
	TipTime           string      `json:"tip_time"`
	TipAuthor         string      `json:"tip_author"`
	ToMerge           int         `json:"to_merge"`
	InAgentBranch     bool        `json:"in_agent_branch"`
	MergeBase         string      `json:"merge_base,omitempty"`
	OtherFilesChanged int         `json:"other_files_changed"`
	Links             hal.LinkMap `json:"_links"`
}

// handleHALPushedBranches serves GET /repos/{repo}/pushed-branches: every
// branch a peer has pushed to this host (repos.RepoInstance.IsPushedBranch)
// with what merging it would bring. A subscription has none.
func handleHALPushedBranches(
	b hal.URLBuilder,
	lister func(context.Context, *repos.RepoInstance) ([]store.Branch, error),
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())
		self := b.Repo(repoName) + "/pushed-branches"

		items := []pushedBranchView{}
		if !ri.Subscribed() && ri.AgentBranch() != "" {
			branches, err := lister(r.Context(), ri)
			if err != nil {
				hal.WriteProblem(w, http.StatusInternalServerError, "Failed to list branches", err.Error(), r.URL.Path)
				return
			}
			var names []string
			for _, br := range branches {
				if ri.IsPushedBranch(br.Name) {
					names = append(names, br.Name)
				}
			}
			sort.Strings(names)

			var (
				infos []store.PushedBranchInfo
				err2  error
			)
			if len(names) > 0 {
				if aerr := ri.WithRead(func(svc *store.Service) {
					infos, err2 = svc.PushedBranches(r.Context(), names, ri.AgentBranch(), ri.OntologyRoot())
				}); aerr != nil {
					hal.WriteProblem(w, http.StatusServiceUnavailable, "Store unavailable", aerr.Error(), r.URL.Path)
					return
				}
				if err2 != nil {
					hal.WriteProblem(w, http.StatusInternalServerError, "Failed to read pushed branches", err2.Error(), r.URL.Path)
					return
				}
			}
			for _, in := range infos {
				branchURL := b.Branch(repoName, hal.Anchor{Branch: in.Name})
				changes := branchURL + "/changes"
				if in.MergeBase != "" {
					changes += "?since=" + url.QueryEscape(in.MergeBase)
				}
				items = append(items, pushedBranchView{
					Name: in.Name, Tip: in.Tip,
					TipTime:   in.TipTime.UTC().Format(time.RFC3339),
					TipAuthor: in.TipAuthor, ToMerge: in.ToMerge,
					InAgentBranch: in.ToMerge == 0, MergeBase: in.MergeBase,
					OtherFilesChanged: in.OtherFilesChanged,
					Links: hal.LinkMap{
						"self":    {Href: branchURL},
						"merge":   {Href: branchURL + "/merge"},
						"changes": {Href: changes},
					},
				})
			}
		}
		hal.WriteHAL(w, http.StatusOK, hal.CollectionView[pushedBranchView]{
			Count:    len(items),
			Links:    hal.LinkMap{"self": {Href: self}},
			Embedded: map[string][]pushedBranchView{"pushed_branches": items},
		})
	}
}

var fullHash = regexp.MustCompile(`^[0-9a-f]{40}$`)

// mergeRequest is the body of POST …/branches/{branch}/merge.
//
// expected_tip is the pushed branch's commit the operator reviewed (the
// `head` of the changes list the dialog showed); the merge takes exactly that
// commit or refuses. resolution is the whole-set answer to a refused merge:
// "host" keeps this host's version of every conflicting fact, "peer" takes the
// pushed branch's. Deliberately NOT ours/theirs: which side is "ours" depends
// on who is asking (store.ResolutionSide), and here the asker is the host's
// operator, for whom git's "ours" is the host — the opposite of
// knomit_experiment's.
type mergeRequest struct {
	ExpectedTip string `json:"expected_tip"`
	Resolution  string `json:"resolution,omitempty"`
}

// handlePushedMerge serves POST /repos/{repo}/branches/{branch}/merge: merge
// the pushed branch {branch} into THIS repo's agent branch. The target is not
// a parameter. The route requires `operator` (Require, in the router) and the
// global writeGate adds `write`.
func handlePushedMerge() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := repos.RepoFromContext(r.Context())
		branch := BranchFromContext(r.Context())

		var req mergeRequest
		if !decodeJSON(w, r, &req, 0) {
			return
		}
		if !fullHash.MatchString(req.ExpectedTip) {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid request",
				"expected_tip must be the full 40-hex commit of the pushed branch you reviewed", r.URL.Path)
			return
		}
		var side store.ResolutionSide
		switch req.Resolution {
		case "":
		case "host":
			side = store.ResolveDst
		case "peer":
			side = store.ResolveSrc
		default:
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid resolution",
				`resolution must be "host" (keep this host's version of every conflicting fact) or "peer" (take the pushed branch's); got `+
					`"`+req.Resolution+`"`, r.URL.Path)
			return
		}

		if ri.Subscribed() || ri.AgentBranch() == "" {
			hal.WriteProblem(w, http.StatusConflict, "Repository has no agent branch",
				"repo "+ri.Name()+" is a subscription: it has no agent branch to merge into", r.URL.Path)
			return
		}
		if refuseUnwritableBranch(w, r, ri, ri.AgentBranch()) {
			return
		}

		// Classified BEFORE the store call: IsPushedBranch takes its own read
		// of the instance, which must not nest inside this one.
		pushed := ri.IsPushedBranch(branch)
		var (
			exists bool
			res    store.AgentReconcileResult
			err    error
		)
		if aerr := ri.WithRead(func(svc *store.Service) {
			if _, herr := svc.Branches().HeadCommit(r.Context(), branch); herr != nil {
				return
			}
			exists = true
			if !pushed {
				return
			}
			res, err = svc.MergePushed(r.Context(), branch, ri.AgentBranch(), plumbing.NewHash(req.ExpectedTip), side)
		}); aerr != nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Store unavailable", aerr.Error(), r.URL.Path)
			return
		}
		if !exists {
			hal.WriteProblem(w, http.StatusNotFound, "Branch not found", "no branch "+branch+" in repo "+ri.Name(), r.URL.Path)
			return
		}
		if !pushed {
			hal.WriteProblem(w, http.StatusUnprocessableEntity, "Not a pushed branch",
				branch+" is not a branch a peer pushed to this host; only those can be merged here", r.URL.Path)
			return
		}
		if err != nil {
			writePushedMergeError(w, r, err)
			return
		}
		hal.WriteHAL(w, http.StatusOK, map[string]any{
			"mode":       string(res.Mode),
			"new_tip":    res.NewTip,
			"merged":     branch,
			"merged_tip": req.ExpectedTip,
			"into":       ri.AgentBranch(),
		})
	}
}

// writePushedMergeError maps MergePushed's refusals. A conflict carries the
// three commits (N8), named for this asker — host_commit is the agent
// branch's, peer_commit the pushed branch's — so the UI can link each fact's
// two versions.
func writePushedMergeError(w http.ResponseWriter, r *http.Request, err error) {
	var conflict *store.MergeConflictError
	var moved *store.BranchMovedError
	switch {
	case errors.As(err, &conflict):
		hal.WriteProblemWithExtra(w, http.StatusConflict, "Merge has conflicting changes",
			"this host and the pushed branch changed the same fact(s); nothing was merged and nothing changed. "+
				`Merge again with "resolution": "host" to keep this host's version of all of them, or "peer" to take the pushed branch's`,
			r.URL.Path, map[string]any{
				"conflicting_paths":  conflict.Paths,
				"paths_without_base": conflict.PathsWithoutBase,
				"base_commit":        conflict.BaseCommit,
				"host_commit":        conflict.DstCommit,
				"peer_commit":        conflict.SrcCommit,
			})
	case errors.As(err, &moved):
		hal.WriteProblemWithExtra(w, http.StatusConflict, "Branch moved",
			moved.Branch+" moved since you reviewed it; review it again", r.URL.Path,
			map[string]any{"expected_tip": moved.Expected, "actual_tip": moved.Actual})
	case errors.Is(err, store.ErrUnrelatedHistories):
		hal.WriteProblem(w, http.StatusConflict, "Unrelated histories", err.Error(), r.URL.Path)
	case errors.Is(err, store.ErrNoSigner):
		hal.WriteProblem(w, http.StatusConflict, "Cannot sign", err.Error(), r.URL.Path)
	case errors.Is(err, store.ErrBranchNotFound):
		hal.WriteProblem(w, http.StatusNotFound, "Branch not found", err.Error(), r.URL.Path)
	default:
		hal.WriteProblem(w, http.StatusInternalServerError, "Could not merge", err.Error(), r.URL.Path)
	}
}
