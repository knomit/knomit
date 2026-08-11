package web

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/repos"
	"knomit/internal/store"
)

// factVersionDate returns the RFC3339 commit date of the version of `path`
// visible at `anchor` on `branch`, or "" when it cannot be resolved.
//
// "The version visible at the anchor" means the newest commit at or before the
// anchor that ADDED or MODIFIED this path — the fact's own last-changed date,
// not the anchor commit's date. In a HEAD view the anchor is the branch tip,
// whose own date would mean "when the repo last changed" and would tick every
// time an unrelated fact was written.
//
// Never fatal: a fact whose commit_log row is missing (an unindexed or GC'd
// commit) yields "" and the field is omitted from the response, rather than
// rendering as 1970.
func factVersionDate(ctx context.Context, ri *repos.RepoInstance, branch, path, anchor string) string {
	var out string
	// WithRead's error means the store was closed or detached, in which case fn
	// never ran. That is a missing date, not a failed request — the fact body
	// has already been read successfully by the time we are called.
	_ = ri.WithRead(func(svc *store.Service) {
		out = versionDateFromService(ctx, svc, branch, path, anchor)
	})
	return out
}

// versionDateFromService is factVersionDate's testable core: same contract,
// against an already-acquired service.
//
// RevisionsBefore is the ONLY correct source here. It walks first-parent
// ancestry via the shared firstParentChainCTE and joins branch_commits, so a
// sibling-branch commit merged back in cannot win. Do NOT replace this with a
// direct commit_log query: the index commit_log_path_time (path, committed_at
// DESC) makes `ORDER BY committed_at DESC LIMIT 1` look like the obvious
// implementation, and it is precisely the wall-clock ordering that
// kb/invariants/store/resolver/first-parent-not-wall-clock/00a49427.md forbids.
// TestRevisionsBefore_MergeAnomalyPicksFirstParent is the guard.
func versionDateFromService(ctx context.Context, svc *store.Service, branch, path, anchor string) string {
	if svc == nil || anchor == "" || path == "" {
		return ""
	}
	revs, err := svc.Search().RevisionsBefore(ctx, branch, path, anchor, 1)
	if err != nil {
		log.Warn().Err(err).Str("branch", branch).Str("path", path).Str("anchor", anchor).
			Msg("fact version date: RevisionsBefore failed; omitting as_of.date")
		return ""
	}
	if len(revs) == 0 || revs[0].CommittedAt == 0 {
		return ""
	}
	return time.Unix(revs[0].CommittedAt, 0).UTC().Format(time.RFC3339)
}
