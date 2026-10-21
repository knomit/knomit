package remotesync

// The repo's consensus branch, recorded (F08 PR D, reviewer blocker B1).
//
// A repo WITH an origin takes its consensus branch from the origin row
// (Remote.Branch). A repo WITHOUT one used to answer the literal "main", so a
// repo cloned from a `trunk` or `master` repository and then detached from its
// origin (the mission template's knomit-hosted route) switched to a "main" it
// did not have: a zero tip, skills and recipes read from nothing, peers unable
// to clone. The user's standing ruling: "main" is the repo's configured
// consensus branch, never a hardcoded name.
//
// So the name is RECORDED in the repo database's `meta` table, the same
// per-repo key/value home as the agent-branch owner, whenever it is
// established: local init records the branch it creates, a clone or a
// subscription the branch it resolved, and every non-nil SetOrigin the
// origin's branch (which keeps it current when the origin's branch is
// changed, and records it for repos cloned before this key existed). Removing
// the origin removes only the origin: the recorded name stays, and
// UpstreamBranch keeps answering it.
//
// The repo database is the place because the name belongs to the repo's git
// data (it names one of its refs) and must survive exactly what the refs
// survive; control.db's origin row is deleted on detach, which is the bug.

import (
	"context"
	"database/sql"
	"errors"
	"sort"

	"knomit/internal/store/model"

	"github.com/rs/zerolog/log"
)

// ConsensusBranchKey is the meta key holding the recorded consensus branch.
const ConsensusBranchKey = "consensus_branch"

// DefaultConsensusBranch is the ONE name knomit gives a consensus branch it
// has to CREATE when nobody named one and there is nothing to detect it from:
// a local create (its modes carry no branch) and the empty-remote seed path (an
// empty remote advertises no HEAD). It is defined here and nowhere else; every
// other path takes the name from a request, from the remote, or from the
// record below (TestNoHardcodedBranchLiterals holds that).
//
// A constant rather than git's init.defaultBranch: the name matters only at
// birth, because it is recorded and read back from then on. Reading the
// running account's gitconfig would make two hosts created by the same request
// disagree, and a daemon or desktop app often has no gitconfig at all.
const DefaultConsensusBranch = "main"

// UpstreamBranch is the repo's consensus branch name: the origin row's
// Remote.Branch when the repo has an origin, else the recorded one. Served
// HEAD, the local reconcile, skills, recipes and the consensus merger all key
// off this.
func (ri *Index) UpstreamBranch() string {
	if r, err := ri.GetRemote("origin"); err == nil && r != nil && r.Branch != "" {
		return r.Branch
	}
	return ri.localConsensusBranch()
}

// localConsensusBranch is the consensus branch of a repo with no origin: the
// cached name, else the recorded one, else (a repo written before the name was
// recorded) the one local branch that is not an agent, experiment or generated
// branch, which is then recorded. "" only when there is no such branch at all
// (a store not yet initialised); several candidates are warned about once and
// the first by name is used, unrecorded.
func (ri *Index) localConsensusBranch() string {
	if b := ri.cachedConsensus(); b != "" {
		return b
	}
	if b := ri.RecordedConsensusBranch(); b != "" {
		ri.setConsensus(b)
		return b
	}
	cands := ri.consensusCandidates()
	switch len(cands) {
	case 0:
		return ""
	case 1:
		ri.RecordConsensusBranch(cands[0])
		return cands[0]
	default:
		ri.warnAmbiguousOnceDo(func() {
			log.Warn().Strs("branches", cands).Str("using", cands[0]).
				Msg("consensus branch: none recorded and the repo has no origin; several candidate branches, using the first")
		})
		return cands[0]
	}
}

// RecordedConsensusBranch is the name stored in the meta table ("" when none
// is recorded or the read fails).
func (ri *Index) RecordedConsensusBranch() string {
	if ri.rh == nil || ri.rh.vr.DB() == nil {
		return ""
	}
	var b string
	err := ri.rh.vr.DB().QueryRow(`SELECT value FROM meta WHERE key = ?`, ConsensusBranchKey).Scan(&b)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Warn().Err(err).Msg("consensus branch: meta read failed")
	}
	return b
}

// RecordConsensusBranch persists name as this repo's consensus branch and
// caches it. A failed write is logged: the cache still answers for this
// process, and the next SetOrigin or open records it again.
func (ri *Index) RecordConsensusBranch(name string) {
	if name == "" {
		return
	}
	ri.setConsensus(name)
	if ri.rh == nil || ri.rh.vr.DB() == nil {
		return
	}
	if _, err := ri.rh.vr.DB().ExecContext(context.Background(),
		`INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`, ConsensusBranchKey, name); err != nil {
		log.Warn().Err(err).Str("branch", name).Msg("consensus branch: record failed")
	}
}

// consensusCandidates is every local branch that could be the consensus
// branch of a repo written before the name was recorded: refs/heads/* minus
// agent branches (this machine's and pushed peers'), experiments and
// generated branches.
func (ri *Index) consensusCandidates() []string {
	if ri.rh == nil || ri.rh.vr.Storer() == nil {
		return nil
	}
	iter, err := ri.rh.vr.Storer().IterReferences()
	if err != nil {
		return nil
	}
	defer iter.Close()
	var out []string
	for {
		ref, err := iter.Next()
		if err != nil {
			break
		}
		if !ref.Name().IsBranch() {
			continue
		}
		b := ref.Name().Short()
		if model.IsRoleBranch(b) {
			continue
		}
		out = append(out, b)
	}
	sort.Strings(out)
	return out
}
