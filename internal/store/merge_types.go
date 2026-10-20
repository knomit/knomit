package store

// Mode classifies the outcome of a reconcile step. The same vocabulary is
// used by both MainReconcileResult (main side) and AgentReconcileResult
// (agent side); each side's doc lists which modes it can return.
type Mode string

const ModeNoop Mode = "noop"

const ModeFF Mode = "ff"

const ModeMerge Mode = "merge"

const ModeRebase Mode = "rebase"

const ModeRewound Mode = "rewound"

// AgentReconcileResult reports the outcome of reconcileAgent.
//
// Mode values:
//   - ModeNoop:   agent ref unchanged.
//   - ModeFF:     agent fast-forwarded to local main (no new commit synthesized).
//   - ModeMerge:  one merge commit synthesized (steady-state path).
//   - ModeRebase: rebase-fallback path ran (origin/main rewind only).
//
// NumReplayed is populated only when Mode == ModeRebase.
//
// Settled is every conflicting path a three-way merge settled instead of
// refusing (a `conflicts` setting, a whole-set side, a caller's side
// resolution), read back from the Knomit-Merge / Knomit-Conflict lines the
// merge commit carries, so the result and the commit cannot disagree.
type AgentReconcileResult struct {
	Mode        Mode          `json:"mode"`
	NumReplayed int           `json:"num_replayed,omitempty"`
	NewTip      string        `json:"new_tip,omitempty"`
	Settled     []SettledPath `json:"settled,omitempty"`
}

// SettledPath is how one conflicting path was settled.
//
//   - Kept is "merged" (the two versions were field-merged), or "src" / "dst"
//     (that side's version landed — its deletion included).
//   - Dropped is the losing change as the trailer names it,
//     "<src|dst>-<modify|delete|add>", or "" for a field merge.
//   - Deleted is true when what landed is the path's ABSENCE: the kept side
//     had deleted it.
//   - Chosen is true when the caller resolved the path itself.
type SettledPath struct {
	Path    string `json:"path"`
	Kept    string `json:"kept"`
	Dropped string `json:"dropped,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
	Chosen  bool   `json:"chosen,omitempty"`
}

// ConflictStrategy determines how shared-path conflicts are resolved during replay.
type ConflictStrategy string

const StrategyLocalWins ConflictStrategy = "local_wins"

const StrategyRemoteWins ConflictStrategy = "remote_wins"

// StrategyRefuse resolves nothing: it collects every conflicting path and
// aborts the merge with a *MergeConflictError before a ref is touched.
// For a merge a human has to adjudicate — landing an experiment on the
// agent branch — where picking either side silently would discard work
// nobody chose to discard.
//
// The VALUE is non-empty on purpose. mergeIntoBranchLocked rewrites an
// empty strategy to StrategyLocalWins, so a zero-value "refuse" would be
// silently downgraded to the very resolution it exists to prevent.
const StrategyRefuse ConflictStrategy = "refuse"
