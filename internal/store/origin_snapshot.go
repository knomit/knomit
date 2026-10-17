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
// THE SNAPSHOT is one immutable pair, the last origin state this copy FULLY
// reconciled against: the origin's identity (originIdentity of its stored
// URL) and the commit its consensus branch held (the fetched
// refs/remotes/origin/<main>). It is one meta row, written in one statement
// at the end of a Sync round that fetched and reconciled without an error,
// and never deleted or rewritten anywhere else. A round that fails at any
// step leaves it as it was.
//
// THE CLASSIFICATION, recomputed every round from the snapshot (identity I0,
// commit S) and what was just fetched (identity I, local main M after the
// main reconcile), for the commits on the agent branch that M lacks:
//  1. not reachable from S: this copy's own unsynced work. Replayed and
//     pushed, whoever signed it.
//  2. reachable from S, I == I0: the origin removed it. The existing rewind
//     rules (the watermark, reconcileAgentRebase) decide, unchanged.
//  3. reachable from S, I != I0: it came from the OLD origin and the new one
//     lacks it. It is not replayed and not pushed; the branch is rebuilt
//     without it, the tip that held it is kept under leftOutRefPrefix, and
//     the count and the old origin are reported (AgentReconcileResult.LeftOut,
//     a Warn). Issue #541 will add the review-and-choose flow.
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

// originSnapshot is the recorded pair. Zero when none is recorded.
type originSnapshot struct {
	Identity string
	Commit   plumbing.Hash
}

func (s originSnapshot) ok() bool { return s.Identity != "" && s.Commit != plumbing.ZeroHash }

// readOriginSnapshot returns the recorded snapshot, the zero value when none
// is recorded or the row does not parse (then the existing rules apply).
func (rh *repoHandler) readOriginSnapshot() (originSnapshot, error) {
	var v string
	err := rh.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, originSnapshotKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return originSnapshot{}, nil
	}
	if err != nil {
		return originSnapshot{}, fmt.Errorf("read origin snapshot: %w", err)
	}
	id, hash, found := strings.Cut(v, "\n")
	if !found || !plumbing.IsHash(hash) || id == "" {
		log.Warn().Msg("origin snapshot: unreadable row; the existing rules apply until a round rewrites it")
		return originSnapshot{}, nil
	}
	return originSnapshot{Identity: id, Commit: plumbing.NewHash(hash)}, nil
}

// writeOriginSnapshot replaces the snapshot in ONE statement.
func (rh *repoHandler) writeOriginSnapshot(s originSnapshot) error {
	if _, err := rh.db.Exec(`INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`,
		originSnapshotKey, s.Identity+"\n"+s.Commit.String()); err != nil {
		return fmt.Errorf("write origin snapshot: %w", err)
	}
	return nil
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
	leftOut, tip, mainHash, err := rh.classifyLeftOut(agentBranch, upstreamMain, snap.Commit)
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
	if err := rh.gits.SetReference(plumbing.NewHashReference(keep, tip)); err != nil {
		return AgentReconcileResult{}, fmt.Errorf("reconcileAgentAcrossOrigins: keep the left-out tip: %w", err)
	}
	if err := rh.markExperimentsRewound(ctx, agentBranch, tip); err != nil {
		return AgentReconcileResult{}, fmt.Errorf("reconcileAgentAcrossOrigins: %w", err)
	}
	held := map[plumbing.Hash]bool{}
	if err := walkHistory(rh.gits, snap.Commit, nil, func(c *object.Commit) { held[c.Hash] = true }); err != nil {
		return AgentReconcileResult{}, fmt.Errorf("reconcileAgentAcrossOrigins: snapshot history: %w", err)
	}
	res, err := rh.replayOntoUpstreamOpts(ctx, agentBranch, mainHash, snap.Commit, strategy,
		replayOpts{extraHeld: held, rebuild: true})
	if err != nil {
		return res, err
	}
	if err := rh.writeAgentBase(agentBranch, mainHash); err != nil {
		return res, fmt.Errorf("reconcileAgentAcrossOrigins: write watermark: %w", err)
	}
	res.LeftOut = leftOut
	res.LeftOutOrigin = snap.Identity
	res.LeftOutRef = keep.String()
	log.Warn().Str("branch", agentBranch).Int("left_out", leftOut).
		Str("previous_origin", snap.Identity).Str("kept_at", keep.String()).
		Msg("reconcile: commits the previous origin held and this origin lacks were left out of the branch and are not pushed (see issue #541)")
	return res, nil
}

// classifyLeftOut counts the class-3 commits: on the agent branch, reachable
// from the snapshot commit, not reachable from local main. Returns the count,
// the agent tip and local main.
func (rh *repoHandler) classifyLeftOut(agentBranch, upstreamMain string, snapCommit plumbing.Hash) (int, plumbing.Hash, plumbing.Hash, error) {
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
	inSnap := map[plumbing.Hash]bool{}
	if err := walkHistory(rh.gits, snapCommit, onMain, func(c *object.Commit) { inSnap[c.Hash] = true }); err != nil {
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
