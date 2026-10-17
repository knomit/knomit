package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog/log"
)

// Snapshot-then-reconcile for a change of origin
// (kb/decisions/store/sync-design/snapshot-and-reconcile/8f04bc80.md).
//
// THE SNAPSHOT is one meta row: an origin identity (originIdentity of its
// stored URL) and the SET S of commits this copy may have taken from that
// origin since it last FULLY reconciled with it. Every write replaces the
// whole row in one statement:
//   - after a fetch from the SAME identity succeeded, before the reconcile,
//     the fetched refs/remotes/origin/<main> is ADDED to S: a round that
//     then fails after the branch merged or rebuilt onto it still has that
//     commit covered (#544 review 3, X1);
//   - after a round's fetch and reconcile both succeeded, S COLLAPSES to
//     {fetched} under the current identity.
//
// It is never deleted. A round that fails at any step leaves it covering at
// least what it covered before. The fetched commit is never written ALONE
// early: a same-origin scrub round that failed would then lose the scrubbed
// commit from S, and a later change of origin would push it.
//
// THE CLASSIFICATION, recomputed every round from the snapshot (identity I0,
// set S) and what was just fetched (identity I, local main M after the main
// reconcile), for the commits on the agent branch that M lacks:
//  1. not reachable from any commit in S: this copy's own unsynced work.
//     Replayed and pushed, whoever signed it.
//  2. reachable from S, I == I0: the origin removed it. The existing rewind
//     rules (the watermark, reconcileAgentRebase) decide, unchanged.
//  3. reachable from S, I != I0: it came from the OLD origin and the new one
//     lacks it. It is not replayed and not pushed; the branch is rebuilt
//     without it, and the tip that held it is kept under leftOutRefPrefix
//     (with its origin in a meta row). The report (AgentReconcileResult.
//     LeftOut*) is DERIVED every round from those durable refs
//     (leftOutReport), so a failed round loses nothing. Issue #541 will add
//     the review-and-choose flow.
//
// With no snapshot (a repository synced before this existed, or never), the
// existing rules apply, and the first round that succeeds writes one.
//
// The snapshot is NOT the watermark (refs/knomit/agent-base/<branch>). The
// watermark is the same-origin rewind's stop point: per agent branch, written
// by every reconcile and by the init paths in the middle of a round, and it
// carries no identity. Pairing it with a separately written identity would
// take two writes that a crash can split; the snapshot is one row. The
// object S names stays readable: this store never deletes objects.
const originSnapshotKey = "origin_snapshot"

// leftOutRefPrefix keeps, per agent branch, the tips whose class-3 commits a
// rebuild left out: refs/knomit/left-out/<branch>/<tip>. Never moved or
// deleted by the reconcile, so what was left out stays recoverable.
const leftOutRefPrefix = "refs/knomit/left-out/"

// leftOutOriginKeyPrefix + <left-out ref name> is the meta row naming the
// origin a left-out tip's class-3 commits came from. Written BEFORE its ref,
// so every left-out ref has one; a row whose ref was never written is
// ignored.
const leftOutOriginKeyPrefix = "left_out_origin:"

// originSnapshot is the recorded identity and commit set. Zero when none is
// recorded.
type originSnapshot struct {
	Identity string
	Commits  []plumbing.Hash
}

func (s originSnapshot) ok() bool { return s.Identity != "" && len(s.Commits) > 0 }

func (s originSnapshot) has(h plumbing.Hash) bool {
	for _, c := range s.Commits {
		if c == h {
			return true
		}
	}
	return false
}

// readOriginSnapshot returns the recorded snapshot, the zero value when none
// is recorded or the row does not parse (then the existing rules apply). The
// row is the identity, then one commit hash per line.
func (rh *repoHandler) readOriginSnapshot() (originSnapshot, error) {
	var v string
	err := rh.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, originSnapshotKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return originSnapshot{}, nil
	}
	if err != nil {
		return originSnapshot{}, fmt.Errorf("read origin snapshot: %w", err)
	}
	s := parseOriginSnapshot(v)
	if !s.ok() {
		log.Warn().Msg("origin snapshot: unreadable row; the existing rules apply until a round rewrites it")
		return originSnapshot{}, nil
	}
	return s, nil
}

// parseOriginSnapshot reads the row format: the identity, then one commit
// hash per line. Zero when it does not parse.
func parseOriginSnapshot(v string) originSnapshot {
	lines := strings.Split(v, "\n")
	s := originSnapshot{Identity: lines[0]}
	for _, l := range lines[1:] {
		if !plumbing.IsHash(l) {
			return originSnapshot{}
		}
		s.Commits = append(s.Commits, plumbing.NewHash(l))
	}
	if !s.ok() {
		return originSnapshot{}
	}
	return s
}

// String is the row format parseOriginSnapshot reads.
func (s originSnapshot) String() string {
	v := s.Identity
	for _, c := range s.Commits {
		v += "\n" + c.String()
	}
	return v
}

// writeOriginSnapshot replaces the snapshot in ONE statement.
func (rh *repoHandler) writeOriginSnapshot(s originSnapshot) error {
	if _, err := rh.db.Exec(`INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`, originSnapshotKey, s.String()); err != nil {
		return fmt.Errorf("write origin snapshot: %w", err)
	}
	return nil
}

// historyOf is every commit reachable from any of tips, not entering prune.
func (rh *repoHandler) historyOf(tips []plumbing.Hash, prune map[plumbing.Hash]bool) (map[plumbing.Hash]bool, error) {
	out := map[plumbing.Hash]bool{}
	for _, tip := range tips {
		if out[tip] || prune[tip] {
			continue
		}
		stop := map[plumbing.Hash]bool{}
		for h := range prune {
			stop[h] = true
		}
		for h := range out {
			stop[h] = true
		}
		if err := walkHistory(rh.gits, tip, stop, func(c *object.Commit) { out[c.Hash] = true }); err != nil {
			return nil, fmt.Errorf("history of %s: %w", tip, err)
		}
	}
	return out, nil
}

// reconcileAgentAcrossOrigins reconciles the agent branch when the snapshot
// belongs to ANOTHER origin than the one just fetched (class 3 possible). It
// is a pure function of the branch tip, local main and the snapshot commit,
// whatever the main reconcile did this round:
//   - nothing on the branch is reachable from the snapshot but not from main:
//     no class-3 commit, so the steady-state merge of main is safe;
//   - otherwise the branch is rebuilt on main with the class-1 commits only
//     (replay walk with the snapshot's whole history held), and the old tip is
//     kept under leftOutRefPrefix.
//
// Holds rh.lockBranch(agentBranch) for the rebuild; the merge path takes it
// itself. Writes the watermark like every reconcile.
func (rh *repoHandler) reconcileAgentAcrossOrigins(ctx context.Context, agentBranch, upstreamMain string, strategy ConflictStrategy, snap originSnapshot) (AgentReconcileResult, error) {
	leftOut, tip, mainHash, err := rh.classifyLeftOut(agentBranch, upstreamMain, snap.Commits)
	if err != nil {
		return AgentReconcileResult{}, fmt.Errorf("reconcileAgentAcrossOrigins: %w", err)
	}
	if leftOut == 0 {
		return rh.reconcileAgentMerge(ctx, agentBranch, upstreamMain, strategy)
	}

	unlock := rh.lockBranch(agentBranch)
	defer unlock()
	// Re-read under the lock: a write between the classification and here
	// only adds class-1 commits, which the walk below replays.
	agentRef, err := rh.gits.Reference(plumbing.NewBranchReferenceName(agentBranch))
	if err != nil {
		return AgentReconcileResult{}, fmt.Errorf("reconcileAgentAcrossOrigins: agent ref: %w", err)
	}
	tip = agentRef.Hash()

	keep := plumbing.ReferenceName(leftOutRefPrefix + agentBranch + "/" + tip.String())
	// The row first (the snapshot as it was: identity and set), then the ref:
	// a ref never lacks it, and the report is derived from both.
	if _, err := rh.db.Exec(`INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`,
		leftOutOriginKeyPrefix+keep.String(), snap.String()); err != nil {
		return AgentReconcileResult{}, fmt.Errorf("reconcileAgentAcrossOrigins: record the left-out origin: %w", err)
	}
	if err := rh.gits.SetReference(plumbing.NewHashReference(keep, tip)); err != nil {
		return AgentReconcileResult{}, fmt.Errorf("reconcileAgentAcrossOrigins: keep the left-out tip: %w", err)
	}
	if err := rh.markExperimentsRewound(ctx, agentBranch, tip); err != nil {
		return AgentReconcileResult{}, fmt.Errorf("reconcileAgentAcrossOrigins: %w", err)
	}
	held, err := rh.historyOf(snap.Commits, nil)
	if err != nil {
		return AgentReconcileResult{}, fmt.Errorf("reconcileAgentAcrossOrigins: snapshot history: %w", err)
	}
	res, err := rh.replayOntoUpstreamOpts(ctx, agentBranch, mainHash, snap.Commits[0], strategy,
		replayOpts{extraHeld: held, rebuild: true})
	if err != nil {
		return res, err
	}
	if err := rh.writeAgentBase(agentBranch, mainHash); err != nil {
		return res, fmt.Errorf("reconcileAgentAcrossOrigins: write watermark: %w", err)
	}
	log.Warn().Str("branch", agentBranch).Int("left_out", leftOut).
		Str("previous_origin", snap.Identity).Str("kept_at", keep.String()).
		Msg("reconcile: commits the previous origin held and this origin lacks were left out of the branch and are not pushed (see issue #541)")
	return res, nil
}

// leftOutReport derives, from the durable left-out refs of agentBranch and
// their rows (the snapshot each rebuild classified against), what is left out
// NOW: the commits reachable from a kept tip AND from that snapshot's set that
// neither the agent branch nor local main holds. Recomputed every round, so a
// round that failed after a rebuild loses nothing, and a commit that came back
// (the old origin re-attached) stops counting. The class-1 commits a kept tip
// also holds were replayed, are not in the snapshot's history, and are not
// counted.
func (rh *repoHandler) leftOutReport(agentBranch, upstreamMain string) (n int, origins, refs []string, err error) {
	iter, err := rh.gits.IterReferences()
	if err != nil {
		return 0, nil, nil, err
	}
	prefix := leftOutRefPrefix + agentBranch + "/"
	type kept struct {
		name string
		tip  plumbing.Hash
	}
	var all []kept
	if err := iter.ForEach(func(r *plumbing.Reference) error {
		if strings.HasPrefix(r.Name().String(), prefix) && r.Type() == plumbing.HashReference {
			all = append(all, kept{r.Name().String(), r.Hash()})
		}
		return nil
	}); err != nil {
		return 0, nil, nil, err
	}
	if len(all) == 0 {
		return 0, nil, nil, nil
	}
	var present []plumbing.Hash
	for _, b := range []string{agentBranch, upstreamMain} {
		if r, err := rh.gits.Reference(plumbing.NewBranchReferenceName(b)); err == nil {
			present = append(present, r.Hash())
		}
	}
	have, err := rh.historyOf(present, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	counted := map[plumbing.Hash]bool{}
	seen := map[string]bool{}
	for _, k := range all {
		var v string
		if err := rh.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, leftOutOriginKeyPrefix+k.name).Scan(&v); err != nil {
			continue
		}
		snap := parseOriginSnapshot(v)
		if !snap.ok() {
			continue
		}
		fromOld, err := rh.historyOf(snap.Commits, have)
		if err != nil {
			return 0, nil, nil, err
		}
		onTip, err := rh.historyOf([]plumbing.Hash{k.tip}, have)
		if err != nil {
			return 0, nil, nil, err
		}
		held := 0
		for h := range onTip {
			if fromOld[h] {
				counted[h] = true
				held++
			}
		}
		if held == 0 {
			continue // nothing of it is left out any more
		}
		refs = append(refs, k.name)
		if !seen[snap.Identity] {
			seen[snap.Identity] = true
			origins = append(origins, snap.Identity)
		}
	}
	return len(counted), origins, refs, nil
}

// classifyLeftOut counts the class-3 commits: on the agent branch, reachable
// from any commit of the snapshot set, not reachable from local main. Returns the count,
// the agent tip and local main.
func (rh *repoHandler) classifyLeftOut(agentBranch, upstreamMain string, snapCommits []plumbing.Hash) (int, plumbing.Hash, plumbing.Hash, error) {
	agentRef, err := rh.gits.Reference(plumbing.NewBranchReferenceName(agentBranch))
	if err != nil {
		return 0, plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("agent ref: %w", err)
	}
	mainRef, err := rh.gits.Reference(plumbing.NewBranchReferenceName(upstreamMain))
	if err != nil {
		return 0, plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("local %s: %w", upstreamMain, err)
	}
	onMain := map[plumbing.Hash]bool{}
	if err := walkHistory(rh.gits, mainRef.Hash(), nil, func(c *object.Commit) { onMain[c.Hash] = true }); err != nil {
		return 0, plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("main history: %w", err)
	}
	inSnap, err := rh.historyOf(snapCommits, onMain)
	if err != nil {
		return 0, plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("snapshot history: %w", err)
	}
	n := 0
	if !onMain[agentRef.Hash()] {
		if err := walkHistory(rh.gits, agentRef.Hash(), onMain, func(c *object.Commit) {
			if inSnap[c.Hash] {
				n++
			}
		}); err != nil {
			return 0, plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("agent history: %w", err)
		}
	}
	return n, agentRef.Hash(), mainRef.Hash(), nil
}

// originIdentity is the part of an origin URL that names WHICH origin it is.
// These spellings of one origin give one identity:
//   - credentials (user info, which may hold a token) are dropped, also from a
//     URL that does not parse (everything up to its LAST "@");
//   - the scheme and host are lower-cased and a default port (https 443,
//     http 80, ssh 22) is dropped;
//   - the path is cleaned (`/./`, `/../`, doubled slashes), and a trailing
//     `.git` and a trailing slash are dropped;
//   - the scp form (`user@host:owner/repo`) is the ssh URL of the same host
//     and path (`ssh://host/owner/repo`);
//   - a bare absolute path is the file:// URL of that path.
//
// Every other difference is a different origin: the path's case, http versus
// https, https versus ssh, another host name for the same server, a
// non-default port, a symlinked path. This is what the snapshot records, so
// no credential is ever written to the meta table or logged.
//
// It has siblings that answer "the same origin?" for other purposes:
// repos.sameFleetURL (the fleet's credential-change test) and the web UI's
// own comparison; they are not required to agree with this one.
func originIdentity(raw string) string {
	raw = strings.TrimSpace(raw)
	switch {
	case strings.Contains(raw, "://"):
		u, err := url.Parse(raw)
		if err != nil {
			return strings.TrimSuffix(stripUserinfo(raw), "/")
		}
		scheme := strings.ToLower(u.Scheme)
		host := strings.ToLower(u.Hostname())
		if port := u.Port(); port != "" && !isDefaultPort(scheme, port) {
			host += ":" + port
		}
		return scheme + "://" + host + cleanOriginPath(u.EscapedPath())
	case isScpLike(raw):
		hostPart, p, _ := scpSplit(raw)
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		return "ssh://" + hostPart + cleanOriginPath(p)
	case strings.HasPrefix(raw, "/"):
		return "file://" + cleanOriginPath(raw)
	case len(raw) >= 3 && raw[1] == ':' && (raw[2] == '/' || raw[2] == '\\'):
		// A Windows drive path, C:\kb or C:/kb.
		return "file://" + cleanOriginPath("/"+strings.ReplaceAll(raw, `\`, "/"))
	default:
		return strings.TrimSuffix(stripUserinfo(raw), "/")
	}
}

// cleanOriginPath cleans a URL path and drops a trailing ".git" and slash.
func cleanOriginPath(p string) string {
	if p == "" {
		return ""
	}
	p = path.Clean(p)
	p = strings.TrimSuffix(p, ".git")
	p = strings.TrimSuffix(p, "/")
	return p
}

func isDefaultPort(scheme, port string) bool {
	return scheme == "https" && port == "443" || scheme == "http" && port == "80" || scheme == "ssh" && port == "22"
}

// isScpLike reports the scp form, [user@]host:path: no scheme, a colon whose
// left side is a host (no slash or backslash, longer than a drive letter).
func isScpLike(raw string) bool {
	_, _, ok := scpSplit(raw)
	return ok
}

// scpSplit splits the scp form [user[:secret]@]host:path into its host and
// path, without the user info: everything up to the last "@" before the path
// is dropped first, so a secret in it is never kept.
func scpSplit(raw string) (host, p string, ok bool) {
	s := raw
	head := s
	if i := strings.IndexAny(s, `/\`); i >= 0 {
		head = s[:i]
	}
	if at := strings.LastIndex(head, "@"); at >= 0 {
		s = s[at+1:]
	}
	host, p, ok = strings.Cut(s, ":")
	if !ok || strings.ContainsAny(host, `/\`) || len(host) <= 1 {
		return "", "", false
	}
	return strings.ToLower(host), p, true
}

// stripUserinfo drops everything after the scheme up to the LAST "@" in the
// string, wherever it is, so a secret holding "/", "?", "#" or "@" is not
// kept (#544 re-review N1). Used where url.Parse refused the string.
func stripUserinfo(s string) string {
	prefix, rest := "", s
	if i := strings.Index(s, "://"); i >= 0 {
		prefix, rest = s[:i+3], s[i+3:]
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	return prefix + rest
}
