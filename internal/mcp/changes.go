package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"knomit/internal/federate"
	"knomit/internal/repos"
	"knomit/internal/store"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// changesPageSize is the default and maximum page size: the same cap as
// knomit_query's snippet pages. Rows are tiny (a path and one word), so the
// cap is about bounding one response, not its weight.
const changesPageSize = 100

// ChangesDescription is the plain-English contract of the MCP tool. The REST
// route's OpenAPI entry restates the parts it shares.
const ChangesDescription = "What is different under a folder since a commit you saw. " +
	"Compares the folder at commit `since` with the folder now and returns each fact path under it as " +
	"added, modified or deleted, plus `head` — the commit it compared against — and `bookmark`. " +
	"No timestamp is read anywhere: the answer depends only on the two commits. " +
	"WHICH HISTORY: one repo and one branch per call. `repo` (a 12-hex id from knomit_repos) defaults to the " +
	"repo you write to; `branch` defaults to the branch your binding reads that repo at (for the write repo, normally your " +
	"agent branch, or your experiment while you are in one), so your own writes appear immediately, and two machines get " +
	"different answers. Pass `consensus: true` instead to read that repo's consensus branch (main, or whatever " +
	"it is named there): the shared history every synced machine agrees on. Coordination agents reading a task " +
	"pool, an inbox or anything another machine also reads should pass `consensus: true`. The response names " +
	"the `repo` and the `branch` it actually read. " +
	"KEEP THE BOOKMARK YOURSELF: store `bookmark` in your own state and pass it as `since` next time, with the " +
	"same repo and branch arguments; knomit writes nothing for you. A bookmark names its repo, so one passed " +
	"against another repo is refused; pass that repo or omit since. Omit `since` to get the whole folder as it " +
	"is now (all rows `added`) — that is also the recovery when you have lost your bookmark. " +
	"It is the NET difference: something added and removed between two reads is not reported, because it " +
	"is no longer there. A rename or move is a `deleted` row plus an `added` row (no rename detection). " +
	"If `since` is not behind the current head (a bookmark from a machine that was further ahead, or from " +
	"another branch) the call refuses with a named error instead of reporting false deletions: retry after " +
	"sync, or omit `since`. " +
	"Rows from the repo you write to are bare paths; rows from any other repo are kb://<id>/… paths, ready " +
	"for knomit_explain. Rows are sorted by path; when `has_more` is true pass the returned `cursor` (alone) " +
	"for the next page, which compares the SAME two commits of the same repo and branch."

// changesTool returns the Tool definition for knomit_changes.
func changesTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_changes",
		mcpgo.WithDescription(ChangesDescription),
		bindingArg(true),
		mcpgo.WithString("repo",
			mcpgo.Description("The repo to read: its 12-hex id from knomit_repos (not its name). Must be mounted in "+
				"your binding. Omit for the repo you write to."),
		),
		mcpgo.WithString("branch",
			mcpgo.Description("The branch to read in that repo, e.g. main, agent/<id> or exp/<name>. Omit for the "+
				"branch you are bound to there. Do not combine with consensus."),
		),
		mcpgo.WithBoolean("consensus",
			mcpgo.Description("true: read the repo's consensus branch (the shared history every synced machine "+
				"agrees on) whatever it is named. Do not combine with branch."),
		),
		mcpgo.WithString("prefix",
			mcpgo.Description("The folder, relative to the repo's ontology root, e.g. \"tasks/research\" for kb/tasks/research. "+
				"Case-insensitive; leading/trailing \"/\" ignored; no .md. Omit for the whole ontology root. "+
				"Not a kb:// path: name another repo with `repo`. "+
				"A folder that did not exist at `since`, or no longer exists now, reads as empty."),
		),
		mcpgo.WithString("since",
			mcpgo.Description("Your bookmark: the `bookmark` a previous call returned (<repo12>:<commit40>), or a "+
				"bare 40-hex commit of the repo being read. Omit to list the folder as it is now."),
		),
		mcpgo.WithNumber("limit",
			mcpgo.Description(fmt.Sprintf("Page size (default and max %d).", changesPageSize)),
		),
		mcpgo.WithString("cursor",
			mcpgo.Description("Page token from a previous response's `cursor`. Pass it alone: it carries the repo, "+
				"branch, prefix and since of the comparison it continues."),
		),
	)
}

// changesResponse is the knomit_changes envelope. Cursor is non-nil only while
// more pages remain.
type changesResponse struct {
	Changes  []store.PathChange `json:"changes"`
	Repo     string             `json:"repo"`
	Branch   string             `json:"branch"`
	Head     string             `json:"head"`
	Bookmark string             `json:"bookmark"`
	HasMore  bool               `json:"has_more"`
	Cursor   *string            `json:"cursor"`
}

// changesArgs are the routing arguments of one call, as the caller sent them.
type changesArgs struct {
	repo, branch string
	consensus    bool
}

// ChangesHandler returns the handler for knomit_changes. It only READS: two
// trees of one branch of one mounted repo. It never calls a store write.
func ChangesHandler() func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		b, err := repos.RequireBinding(ctx)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}

		limit := req.GetInt("limit", changesPageSize)
		if limit <= 0 || limit > changesPageSize {
			limit = changesPageSize
		}
		args := changesArgs{
			repo:      req.GetString("repo", ""),
			branch:    req.GetString("branch", ""),
			consensus: req.GetBool("consensus", false),
		}
		if args.branch != "" && args.consensus {
			return mcpgo.NewToolResultError("pass branch or consensus, not both"), nil
		}

		var (
			q      store.ChangesQuery
			scope  store.ChangesScope
			cursor = req.GetString("cursor", "")
		)
		if cursor != "" {
			if q, scope, err = store.DecodeChangesCursor(cursor); err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}
		} else {
			q = store.ChangesQuery{Since: req.GetString("since", ""), Prefix: req.GetString("prefix", "")}
			if strings.HasPrefix(strings.ToLower(q.Prefix), "kb://") {
				return mcpgo.NewToolResultError(kbPrefixRefusal(q.Prefix)), nil
			}
		}
		q.Limit = limit

		// The repo: the cursor's, else the argument, else the write repo. A
		// cursor always names both; an explicit argument that contradicts it
		// is refused.
		repoID := args.repo
		if cursor != "" {
			if args.repo != "" && !strings.EqualFold(args.repo, scope.Repo) {
				return mcpgo.NewToolResultError(fmt.Sprintf("cursor is for repo %s, this call names repo %s; pass the cursor alone", scope.Repo, args.repo)), nil
			}
			repoID = scope.Repo
		}
		rt, errText := resolveChangesRepo(b, repoID)
		if errText != "" {
			if cursor != "" {
				errText = fmt.Sprintf("cursor is for repo %s, which is not mounted in this binding; restart without a cursor", scope.Repo)
			}
			return mcpgo.NewToolResultError(errText), nil
		}
		if rt.RI.ID() == "" {
			return mcpgo.NewToolResultError("this repo's identity is not resolved yet (no root commit); cannot name it in a bookmark or cursor — retry after the repo has its first commit"), nil
		}
		repo12 := federate.ID12(rt.RI.ID())

		svc, release, err := rt.RI.Acquire()
		if err != nil {
			return mcpgo.NewToolResultError(errStoreUnavailable.Error()), nil
		}
		defer release()

		// The branch: the cursor's, else the argument, else consensus on
		// request, else the branch this binding reads the repo at.
		branch, fromConsensus := rt.Branch, false
		switch {
		case cursor != "":
			branch = scope.Branch
		case args.branch != "":
			branch = args.branch
		case args.consensus:
			branch, fromConsensus = svc.UpstreamBranch(), true
		}
		if cursor != "" && (args.branch != "" && args.branch != branch || args.consensus && svc.UpstreamBranch() != branch) {
			return mcpgo.NewToolResultError(fmt.Sprintf("cursor is for branch %q of repo %s; pass the cursor alone", branch, repo12)), nil
		}

		// A bookmark names its repo; a bare hash means this one.
		sinceRepo, sinceCommit, err := store.ParseChangesSince(q.Since)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		if sinceRepo != "" && sinceRepo != repo12 {
			return mcpgo.NewToolResultError(fmt.Sprintf("bookmark is for repo %s, this call reads %s; pass repo: %s or omit since",
				describeChangesRepo(b, sinceRepo), describeChangesRepo(b, repo12), sinceRepo)), nil
		}
		q.Since = sinceCommit

		res, err := svc.Facts().ChangesUnder(ctx, branch, q)
		if err != nil {
			return mcpgo.NewToolResultError(changesErrorText(err, repo12, branch, fromConsensus)), nil
		}

		out := changesResponse{
			Changes:  make([]store.PathChange, 0, len(res.Changes)),
			Repo:     repo12,
			Branch:   branch,
			Head:     res.Head,
			Bookmark: store.ChangesBookmark(repo12, res.Head),
			HasMore:  res.HasMore,
		}
		for _, c := range res.Changes {
			out.Changes = append(out.Changes, store.PathChange{Path: wirePath(b, rt, c.Path), Change: c.Change})
		}
		if res.HasMore {
			// After stays repo-relative: it is compared against store rows.
			cur := store.EncodeChangesCursor(store.ChangesScope{Repo: repo12, Branch: branch},
				q.Since, res.Head, q.Prefix, res.Changes[len(res.Changes)-1].Path)
			out.Cursor = &cur
		}
		data, err := json.Marshal(out)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("marshal error: %v", err)), nil
		}
		return mcpgo.NewToolResultText(string(data)), nil
	}
}

// resolveChangesRepo routes a repo id to its mount. "" is the write repo, read
// at the branch this binding reads it at (the agent branch, or the
// experiment the session is in). Anything else must be a 12-hex id mounted in
// the binding. Returns the refusal text, or "".
func resolveChangesRepo(b *repos.Binding, id string) (repos.ReadTarget, string) {
	if id == "" {
		return repos.ReadTarget{RI: b.Write(), Branch: b.WriteMountBranch()}, ""
	}
	if len(id) != 12 || !isHexDigits(id) {
		return repos.ReadTarget{}, fmt.Sprintf("repo must be a 12-hex id from knomit_repos, not %q", id)
	}
	id = strings.ToLower(id)
	if rt, ok := b.ByID(id); ok {
		return rt, ""
	}
	if strings.HasPrefix(b.Write().ID(), id) {
		return repos.ReadTarget{RI: b.Write(), Branch: b.WriteMountBranch()}, ""
	}
	return repos.ReadTarget{}, fmt.Sprintf("repo %s is not mounted in this binding", id)
}

// describeChangesRepo renders a 12-hex id with its name when the binding
// mounts it, for refusals a caller must be able to act on.
func describeChangesRepo(b *repos.Binding, id12 string) string {
	if rt, ok := b.ByID(id12); ok {
		return fmt.Sprintf("%s (%s)", id12, rt.RI.Name())
	}
	if strings.HasPrefix(b.Write().ID(), id12) {
		return fmt.Sprintf("%s (%s)", id12, b.Write().Name())
	}
	return id12 + " (not mounted in this binding)"
}

// kbPrefixRefusal points a caller who sent a kb:// path as the prefix at the
// two arguments that say the same thing.
func kbPrefixRefusal(prefix string) string {
	id, _, _ := strings.Cut(prefix[len("kb://"):], "/")
	if id == "" {
		id = "<repo-id>"
	}
	return fmt.Sprintf("prefix %q is a kb:// path; knomit_changes takes the repo separately: pass repo: %q and prefix: the folder "+
		"relative to that repo's ontology root (e.g. \"meta\" for kb/meta)", prefix, id)
}

// isHexDigits reports whether s is all hex digits.
func isHexDigits(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

// changesErrorText words a store error for the caller. The client errors
// carry their remedy already; an unknown branch names the repo and branch,
// and a missing consensus branch says when it will exist.
func changesErrorText(err error, repo12, branch string, consensus bool) string {
	if errors.Is(err, store.ErrBranchNotFound) {
		if consensus {
			return fmt.Sprintf("repo %s has no branch %q (its consensus branch appears after the first sync); nothing to compare", repo12, branch)
		}
		return fmt.Sprintf("repo %s has no branch %q", repo12, branch)
	}
	return err.Error()
}
