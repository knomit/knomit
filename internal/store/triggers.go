// Triggers (F07): the store half of the trigger dispatcher. Everything here is
// a git READ, a read of the index's liveness join (DueCandidates), or a write
// to the dispatcher's OWN three tables (trigger_watermarks, trigger_fires,
// migration 000029; trigger_due_fires, migration 000030). Nothing here writes
// a ref, an object or a fact, and nothing here reads a clock except to stamp
// fired_at for the operator — the ONE clock comparison in F07 (the due
// sweep's `expires_at <= now`) is the dispatcher's, which reads its clock once
// per run as UTC and passes the instant in. All times are UTC: Unix seconds in
// the tables, RFC 3339 with an explicit Z wherever a row is rendered. The
// dispatcher itself is internal/repos/triggers.go; this file is what it calls
// under Acquire.
package store

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/crypto/ssh"

	"knomit/internal/fact"
	"knomit/internal/pki"
)

// TriggerFireRetention is how many fire-log rows a repo keeps: the newest
// 10,000. The first count-capped table in the store — fires come in bursts, so
// an age cap would not bound them. The log is operator cache, not the audit
// trail (the trail is the trace trailers in git).
const TriggerFireRetention = 10000

// Fire outcomes. `if-false` and `self-caused` are COUNTED in the dispatcher's
// statistics and never written as a row, so the cap holds real fires. `run`
// marks the one summary row each dispatcher run writes. The `script-*` and
// `rate-limited` outcomes are a `do: script` trigger's (F07 PR 3): `ran` is
// its `emitted`; `self-caused` is the loop guard (the toucher of the path
// carries `Knomit-Trigger: <this trigger>`, so the fire is skipped before
// `if`); `rate-limited` is a fire DROPPED by the per-minute cap. `kicked` is
// a `do: push` fire (F07 PR 4): it asked this machine's sync loop for a round
// and says nothing about whether that round then succeeded — that is the
// remote's sync/push status. The column is free text; adding a kind needs no
// migration.
//
// The `do: run` / knomit.run kinds (F07 PR 5). `started` is the fire: the
// recipe was resolved and handed to the runner with a fresh run id, and
// nothing waited for it (counted as a fire). `busy` is a fire DROPPED because
// the recipe is at its `concurrent` limit (no queue). `recipe-error` is a
// recipe that failed to resolve (does not compile, bad header) or, as the
// result row, one that threw, returned `status: "error"` or an invalid value,
// or was stopped; `recipe-timeout` one that ran past its budget. `done`,
// `spawned`, `delivered` and `unreachable` are the recipe's own reported
// status on the result row. A started fire's result row carries the SAME
// run_id; an unbound fire (no recipe anywhere) writes no row.
const (
	TriggerOutcomeEmitted       = "emitted"
	TriggerOutcomeIfFalse       = "if-false"
	TriggerOutcomeIfError       = "if-error"
	TriggerOutcomeIfTimeout     = "if-timeout"
	TriggerOutcomeUnparseable   = "unparseable"
	TriggerOutcomeRun           = "run"
	TriggerOutcomeRan           = "ran"
	TriggerOutcomeScriptError   = "script-error"
	TriggerOutcomeScriptTimeout = "script-timeout"
	TriggerOutcomeRateLimited   = "rate-limited"
	TriggerOutcomeSelfCaused    = "self-caused"
	TriggerOutcomeKicked        = "kicked"

	TriggerOutcomeStarted       = "started"
	TriggerOutcomeBusy          = "busy"
	TriggerOutcomeRecipeError   = "recipe-error"
	TriggerOutcomeRecipeTimeout = "recipe-timeout"
	TriggerOutcomeDone          = "done"
	TriggerOutcomeSpawned       = "spawned"
	TriggerOutcomeDelivered     = "delivered"
	TriggerOutcomeUnreachable   = "unreachable"
)

// Recipe sources of a `do: run` row (recipe_source).
const (
	RecipeSourceRepo  = "repo"
	RecipeSourceLocal = "local"
)

// ErrNoScriptAtCommit: the commit's tree holds no `.knomit/triggers/<name>.js`
// for the trigger's script name. The trigger is `invalid` on the endpoint and
// every fire is a `script-error` until the file exists at the head.
var ErrNoScriptAtCommit = errors.New("script not found at the head")

// ErrNoRecipeAtCommit: the commit's tree holds no `.knomit/recipes/<name>.js`.
// The dispatcher then falls through to the machine-local recipe.
var ErrNoRecipeAtCommit = errors.New("recipe not found at the commit")

// ErrNoGuidanceAtCommit: the commit's tree holds no file at the guidance path
// the ontology names (F23). The reader skips that key with a warning.
var ErrNoGuidanceAtCommit = errors.New("guidance file not found at the commit")

// ErrGuidanceUnusable: the guidance path names something that is not a
// regular UTF-8 text file of at most fact.MaxGuidanceBytes (a directory, a
// symlink, a submodule, a larger file, binary or invalid UTF-8 content). The
// reader skips that key with a warning, exactly as for a missing file.
var ErrGuidanceUnusable = errors.New("guidance file unusable")

// ErrNoOntologyAtCommit: the commit's tree holds no ontology file at any of the
// known paths. The dispatcher keeps its last good trigger set.
var ErrNoOntologyAtCommit = errors.New("no ontology file in the commit's tree")

// TriggerFire is one row of trigger_fires: a fire (or an error outcome) of one
// trigger on one path, or, with Trigger == "" and Outcome == "run", the run
// row that summarises one dispatcher run.
type TriggerFire struct {
	ID             int64  `json:"id"`
	Trigger        string `json:"trigger"`
	Branch         string `json:"branch"`
	Path           string `json:"path,omitempty"`
	Episode        string `json:"episode,omitempty"`
	Source         string `json:"source,omitempty"`
	Commit         string `json:"commit,omitempty"`
	Trace          string `json:"trace,omitempty"`
	Outcome        string `json:"outcome"`
	Error          string `json:"error,omitempty"`
	Nonlinear      bool   `json:"nonlinear,omitempty"`
	RangeFrom      string `json:"range_from"`
	RangeTo        string `json:"range_to"`
	Evaluated      int    `json:"evaluated,omitempty"`
	Paths          int    `json:"paths,omitempty"`
	Fires          int    `json:"fires,omitempty"`
	FiresNotLogged int    `json:"fires_not_logged,omitempty"`
	DurationMS     int64  `json:"duration_ms,omitempty"`
	DiffMS         int64  `json:"diff_ms,omitempty"`
	ChangeMS       int64  `json:"change_ms,omitempty"`
	// RecipeSource, RecipeRev and RunID are a `do: run` / knomit.run row's
	// (F07 PR 5; "" on every other row): where the recipe came from (repo =
	// main's tip, local = <home>/recipes), which exact code ran (the blob
	// hash at main, or sha256[:12] of the local file), and the run id the
	// `started` row and its result row share.
	RecipeSource string `json:"recipe_source,omitempty"`
	RecipeRev    string `json:"recipe_rev,omitempty"`
	RunID        string `json:"run_id,omitempty"`
	// Message is a NON-error result text (migration 000033): what a recipe's
	// result row reports when its outcome is a success (done, spawned,
	// delivered), e.g. "exit 0 cost=1.03". Error holds failures only
	// (IsRecipeFailure); before 000033 this text was stored in Error for
	// every outcome.
	Message string `json:"message,omitempty"`
	// FiredAt is the row's stamp rendered as RFC 3339 UTC with an explicit Z
	// (all times are UTC); stored as Unix seconds. For the operator, never
	// read for ordering.
	FiredAt string `json:"fired_at"`
}

// IsRecipeFailure reports whether a recipe result row's outcome is a failure,
// so its text belongs in Error: the run did not complete (recipe-error: a
// throw, `status: "error"`, an invalid result, a knomit stop; recipe-timeout)
// or the recipe reported it could not deliver (unreachable). The successful
// outcomes (done, spawned, delivered) are the recipe's own report, and their
// text belongs in Message.
func IsRecipeFailure(outcome string) bool {
	switch outcome {
	case TriggerOutcomeRecipeError, TriggerOutcomeRecipeTimeout, TriggerOutcomeUnreachable:
		return true
	}
	return false
}

// DueCandidate is one row of the sweep's liveness join: a dated fact live on
// the branch at the index's view of its head, with its expiry as Unix seconds
// (UTC). The dispatcher confirms each against the head's tree before firing.
type DueCandidate struct {
	Path      string
	ExpiresAt int64
}

// DueKey identifies a due mark: one trigger's processing of one path.
type DueKey struct {
	Trigger string
	Path    string
}

// DueMark is one row of trigger_due_fires: trigger has processed path at the
// due instant ExpiresAt (whatever `if` said), stamped FiredAt (Unix seconds,
// UTC, the run's clock).
type DueMark struct {
	Trigger   string
	Path      string
	ExpiresAt int64
	FiredAt   int64
}

// TriggerRun is what one dispatcher run hands to RecordTriggerRun: the
// coalesced range, its counts and the fire rows to log (newest paths last).
type TriggerRun struct {
	Branch     string
	RangeFrom  string
	RangeTo    string
	Nonlinear  bool
	Paths      int // diff rows in the range
	Evaluated  int // (trigger, path) evaluations across every trigger
	Fires      int // emitted, before the retention cap
	DurationMS int64
	DiffMS     int64
	ChangeMS   int64
	Rows       []TriggerFire
}

// TouchResult is the answer of the treesame toucher walk (Toucher).
type TouchResult struct {
	Commit plumbing.Hash
	// Found is false when the walk reached a commit with no parent without
	// finding a toucher — a scrubbed path after a rewind. The caller takes
	// the defined fallback (commit = the advance's head, author unknown).
	Found bool
	// MergeToucher marks the one case where a merge IS the toucher: none of
	// its parents carries the path's blob, so the merge itself produced it (a
	// textual three-way merge, e.g. by GitHub). knomit's own merges resolve
	// per file, one side wins, so they always have a same-blob parent.
	MergeToucher bool
	Steps        int
}

// CommitInfo is what the dispatcher reads from a commit to build `change`.
type CommitInfo struct {
	Hash        plumbing.Hash
	AuthorName  string
	AuthorEmail string
	Message     string
	Parents     []plumbing.Hash
}

// TriggerIndex is the store surface of the trigger dispatcher.
type TriggerIndex interface {
	// DiffFacts is the two-tree diff of the whole ontology root between two
	// commits with NO ancestry check (changes.go): the dispatcher's advance
	// may be a rewind replay, which ChangesUnder refuses.
	DiffFacts(ctx context.Context, from, to plumbing.Hash) ([]PathChange, error)
	// OntologyAtCommit reads the ontology file from the commit's own tree,
	// choosing the path exactly as the store does (the first of
	// fact.OntologyPathsNewestFirst that exists as a file), and returns its
	// path, blob hash and content.
	OntologyAtCommit(ctx context.Context, commit plumbing.Hash) (path, blob string, data []byte, err error)
	// ScriptAt reads `.knomit/triggers/<name>.js` from the commit's own tree
	// (the same rule as OntologyAtCommit: a directory or an unreadable blob is
	// an error, never "absent"), returning its blob hash — the script cache's
	// key — and content. ErrNoScriptAtCommit when the file is not there.
	ScriptAt(ctx context.Context, commit plumbing.Hash, name string) (blob string, data []byte, err error)
	// RecipeAt reads `.knomit/recipes/<name>.js` from the commit's own tree —
	// the dispatcher passes the tip of main, never the agent branch (user
	// ruling D1) — with the same rules as ScriptAt. ErrNoRecipeAtCommit when
	// the file is not there.
	RecipeAt(ctx context.Context, commit plumbing.Hash, name string) (blob string, data []byte, err error)
	// GuidanceAt reads one guidance file (F23), file being the repository
	// path fact.GuidanceFile returns, from the commit's own tree — callers
	// pass the tip of the consensus branch, the commit they read the ontology
	// at, never the agent branch. ErrNoGuidanceAtCommit when absent;
	// ErrGuidanceUnusable (wrapped, naming why) unless it is a regular UTF-8
	// text file of at most fact.MaxGuidanceBytes.
	GuidanceAt(ctx context.Context, commit plumbing.Hash, file string) (blob string, data []byte, err error)
	// Toucher finds the commit that introduced the blob path carries at head:
	// git's own history simplification, with no clock. See the method.
	Toucher(ctx context.Context, head plumbing.Hash, path string) (TouchResult, error)
	// TreeReader returns a reader that caches decoded trees across calls, for
	// a run that reads many paths of the same few commits (see TriggerTrees).
	TreeReader() TriggerTrees
	CommitInfo(ctx context.Context, hash plumbing.Hash) (CommitInfo, error)
	// CommitSignerOf returns the verified SSHSIG signer of a commit; the
	// fingerprint is set ONLY when the signature verifies over the payload.
	CommitSignerOf(ctx context.Context, hash plumbing.Hash) (CommitSigner, error)
	// BlobAt reads path's content at commit; ok is false when absent.
	BlobAt(ctx context.Context, commit plumbing.Hash, path string) (content string, ok bool, err error)
	IsAncestor(ctx context.Context, a, b plumbing.Hash) (bool, error)
	// UpstreamTip is the local consensus branch's head (ZeroHash when the
	// branch does not exist). Since F09 PR 5 verification happens ONCE at the
	// gate that advances main, and main is trusted afterwards: a commit is
	// "verified" when it is reachable from that tip. There is no anchor ref
	// and no fold cache any more.
	UpstreamTip(ctx context.Context, upstream string) (plumbing.Hash, error)
	// AncestorSet is every commit reachable from tip, tip included. The
	// dispatcher walks it once per run per distinct tip and reuses it.
	AncestorSet(ctx context.Context, tip plumbing.Hash) (map[plumbing.Hash]bool, error)

	// TriggerWatermarks returns trigger name → commit hash for branch.
	TriggerWatermarks(ctx context.Context, branch string) (map[string]string, error)
	// RecordTriggerRun is tx1 for one run: the run's fire rows, capped at
	// TriggerFireRetention (the newest paths kept, the rest counted in the run
	// row's fires_not_logged), plus exactly ONE run row. Returns how many fire
	// rows were written.
	RecordTriggerRun(ctx context.Context, run TriggerRun) (int, error)
	// RecordTriggerRuns is tx1 for several buffered runs in ONE transaction,
	// each with its own cap and its own run row. The dispatcher defers tx1
	// while the writer is busy and flushes the runs together.
	RecordTriggerRuns(ctx context.Context, runs []TriggerRun) (int, error)
	// AdvanceTriggerWatermarks is tx2: upserts set, deletes del (their
	// bookmarks AND their due marks), upserts the due marks (chunked, never
	// capped by TriggerFireRetention), then prunes trigger_fires to the newest
	// TriggerFireRetention rows.
	AdvanceTriggerWatermarks(ctx context.Context, branch string, set map[string]string, del []string, due []DueMark) error
	// RecordTriggerResults is the write of a recipe's LATE result rows (F07
	// PR 5): each row as given, with its own branch and range (copied from the
	// fire that started it), and NO run row — a result is not a dispatcher run.
	// One transaction; the retention prune runs in the same transaction.
	RecordTriggerResults(ctx context.Context, rows []TriggerFire) error
	// RecentTriggerFires returns the newest limit rows for branch, newest first.
	RecentTriggerFires(ctx context.Context, branch string, limit int) ([]TriggerFire, error)
	// TriggerFiresByRun returns branch's rows carrying run_id id, oldest
	// first: the `started` row, then the result row once it is logged.
	TriggerFiresByRun(ctx context.Context, branch, id string) ([]TriggerFire, error)

	// DueCandidates is the `on: due` sweep's candidate set: the dated facts
	// LIVE on branch (branch_facts ⋈ fact_expires) whose expires_at is at or
	// before nowUnix (F03's inclusive rule). A fact retracted, deleted,
	// merged away, rewound or replaced has no branch_facts row for that
	// version and cannot be a candidate; fact_expires alone (one row per dated
	// fact VERSION, shared by every branch, orphans kept) is never read alone.
	DueCandidates(ctx context.Context, branch string, nowUnix int64) ([]DueCandidate, error)
	// DueMarks returns (trigger, path) → expires_at already processed on
	// branch (trigger_due_fires); an index range on the primary key.
	DueMarks(ctx context.Context, branch string) (map[DueKey]int64, error)
}

// Triggers returns the trigger dispatcher's store surface.
func (s *Service) Triggers() TriggerIndex { return s.rh }

// Compile-time assertion: repoHandler must implement TriggerIndex.
var _ TriggerIndex = (*repoHandler)(nil)

// SignerFingerprint is the full pki.Fingerprint of the key this store signs
// authored commits with (its signer, or inside a test binary the test
// fallback), or "" when it has none. `change.source` is `local` exactly when
// the firing commit's verified signer carries this fingerprint.
func (s *Service) SignerFingerprint() string {
	signer, err := s.rh.commitSigner()
	if err != nil {
		return ""
	}
	cpk, ok := signer.PublicKey().(ssh.CryptoPublicKey)
	if !ok {
		return ""
	}
	ed, ok := cpk.CryptoPublicKey().(ed25519.PublicKey)
	if !ok {
		return ""
	}
	return pki.Fingerprint(ed)
}

// OntologyAtCommit implements TriggerIndex. A path present as a directory or a
// blob that cannot be read is an error, never "try the next path", so a
// shadowing directory cannot fall through (the same rule as verify_fold's
// settingsAt).
func (rh *repoHandler) OntologyAtCommit(ctx context.Context, commit plumbing.Hash) (string, string, []byte, error) {
	c, err := rh.repo.CommitObject(commit)
	if err != nil {
		return "", "", nil, fmt.Errorf("triggers: ontology at %s: %w", commit, err)
	}
	tree, err := c.Tree()
	if err != nil {
		return "", "", nil, fmt.Errorf("triggers: tree of %s: %w", commit, err)
	}
	for _, p := range fact.OntologyPathsNewestFirst() {
		entry, err := tree.FindEntry(p)
		if errors.Is(err, object.ErrEntryNotFound) || errors.Is(err, object.ErrDirectoryNotFound) {
			continue
		}
		if err != nil {
			return "", "", nil, fmt.Errorf("triggers: ontology entry %q at %s: %w", p, commit, err)
		}
		if !entry.Mode.IsFile() {
			return "", "", nil, fmt.Errorf("triggers: ontology path %q at %s is not a file", p, commit)
		}
		f, err := tree.TreeEntryFile(entry)
		if err != nil {
			return "", "", nil, fmt.Errorf("triggers: ontology blob %q at %s: %w", p, commit, err)
		}
		body, err := f.Contents()
		if err != nil {
			return "", "", nil, fmt.Errorf("triggers: ontology contents %q at %s: %w", p, commit, err)
		}
		return p, entry.Hash.String(), []byte(body), nil
	}
	return "", "", nil, ErrNoOntologyAtCommit
}

// ScriptAt implements TriggerIndex. The path is fact.TriggerScriptPath(name);
// the name was validated kebab-case at compile time, so it names a file
// directly under .knomit/triggers/ and nothing else.
func (rh *repoHandler) ScriptAt(ctx context.Context, commit plumbing.Hash, name string) (string, []byte, error) {
	return rh.privateFileAt(commit, "script", fact.TriggerScriptPath(name), ErrNoScriptAtCommit)
}

// RecipeAt implements TriggerIndex. The path is fact.TriggerRecipePath(name);
// the name is validated kebab-case by every caller.
func (rh *repoHandler) RecipeAt(ctx context.Context, commit plumbing.Hash, name string) (string, []byte, error) {
	return rh.privateFileAt(commit, "recipe", fact.TriggerRecipePath(name), ErrNoRecipeAtCommit)
}

// GuidanceAt implements TriggerIndex. Unlike privateFileAt it refuses a
// symlink (whose blob is only its target's name) and checks the size BEFORE
// reading the blob, so an oversized file is never loaded.
func (rh *repoHandler) GuidanceAt(ctx context.Context, commit plumbing.Hash, file string) (string, []byte, error) {
	if !strings.HasPrefix(file, fact.GuidanceDir+"/") || path.Clean(file) != file || slices.Contains(strings.Split(file, "/"), "..") {
		return "", nil, fmt.Errorf("%w: %q is not under %s/", ErrGuidanceUnusable, file, fact.GuidanceDir)
	}
	c, err := rh.repo.CommitObject(commit)
	if err != nil {
		return "", nil, fmt.Errorf("guidance: commit %s: %w", commit, err)
	}
	tree, err := c.Tree()
	if err != nil {
		return "", nil, fmt.Errorf("guidance: tree of %s: %w", commit, err)
	}
	entry, err := tree.FindEntry(file)
	if errors.Is(err, object.ErrEntryNotFound) || errors.Is(err, object.ErrDirectoryNotFound) {
		return "", nil, ErrNoGuidanceAtCommit
	}
	if err != nil {
		return "", nil, fmt.Errorf("guidance: entry %q at %s: %w", file, commit, err)
	}
	if entry.Mode != filemode.Regular && entry.Mode != filemode.Executable && entry.Mode != filemode.Deprecated {
		return "", nil, fmt.Errorf("%w: %q is not a regular file (mode %s)", ErrGuidanceUnusable, file, entry.Mode)
	}
	f, err := tree.TreeEntryFile(entry)
	if err != nil {
		return "", nil, fmt.Errorf("guidance: blob %q at %s: %w", file, commit, err)
	}
	if f.Size > fact.MaxGuidanceBytes {
		return "", nil, fmt.Errorf("%w: %q is %d bytes, over the %d-byte limit", ErrGuidanceUnusable, file, f.Size, fact.MaxGuidanceBytes)
	}
	body, err := f.Contents()
	if err != nil {
		return "", nil, fmt.Errorf("guidance: contents %q at %s: %w", file, commit, err)
	}
	if !utf8.ValidString(body) || strings.ContainsRune(body, 0) {
		return "", nil, fmt.Errorf("%w: %q is not UTF-8 text", ErrGuidanceUnusable, file)
	}
	return entry.Hash.String(), []byte(body), nil
}

// privateFileAt reads one file of a commit's own tree: absent is notFound; a
// directory or an unreadable blob is an error, never "absent".
func (rh *repoHandler) privateFileAt(commit plumbing.Hash, what, p string, notFound error) (string, []byte, error) {
	c, err := rh.repo.CommitObject(commit)
	if err != nil {
		return "", nil, fmt.Errorf("triggers: %s at %s: %w", what, commit, err)
	}
	tree, err := c.Tree()
	if err != nil {
		return "", nil, fmt.Errorf("triggers: tree of %s: %w", commit, err)
	}
	entry, err := tree.FindEntry(p)
	if errors.Is(err, object.ErrEntryNotFound) || errors.Is(err, object.ErrDirectoryNotFound) {
		return "", nil, notFound
	}
	if err != nil {
		return "", nil, fmt.Errorf("triggers: %s entry %q at %s: %w", what, p, commit, err)
	}
	if !entry.Mode.IsFile() {
		return "", nil, fmt.Errorf("triggers: %s path %q at %s is not a file", what, p, commit)
	}
	f, err := tree.TreeEntryFile(entry)
	if err != nil {
		return "", nil, fmt.Errorf("triggers: %s blob %q at %s: %w", what, p, commit, err)
	}
	body, err := f.Contents()
	if err != nil {
		return "", nil, fmt.Errorf("triggers: %s contents %q at %s: %w", what, p, commit, err)
	}
	return entry.Hash.String(), []byte(body), nil
}

// TriggerTrees reads paths out of commit trees, caching every decoded tree
// for its lifetime. One is made per dispatcher run: an advance touches the
// same few commits for every matched path, and go-git decodes a commit's root
// tree afresh on each Commit.Tree() call (a 10,000-entry folder is decoded
// and indexed again per lookup), so without the cache a large advance costs
// minutes. A Tree keeps its own subtree cache once decoded, so the second
// lookup of a sibling path is a couple of map hits.
type TriggerTrees interface {
	Toucher(ctx context.Context, head plumbing.Hash, path string) (TouchResult, error)
	BlobAt(ctx context.Context, commit plumbing.Hash, path string) (content string, ok bool, err error)
}

type triggerTrees struct {
	rh      *repoHandler
	commits map[plumbing.Hash]*object.Commit
	trees   map[plumbing.Hash]*object.Tree
}

// TreeReader implements TriggerIndex.
func (rh *repoHandler) TreeReader() TriggerTrees {
	return &triggerTrees{rh: rh, commits: map[plumbing.Hash]*object.Commit{}, trees: map[plumbing.Hash]*object.Tree{}}
}

func (tt *triggerTrees) commit(h plumbing.Hash) (*object.Commit, error) {
	if c, ok := tt.commits[h]; ok {
		return c, nil
	}
	c, err := tt.rh.repo.CommitObject(h)
	if err != nil {
		return nil, fmt.Errorf("triggers: commit %s: %w", h, err)
	}
	tt.commits[h] = c
	return c, nil
}

func (tt *triggerTrees) tree(c *object.Commit) (*object.Tree, error) {
	if t, ok := tt.trees[c.Hash]; ok {
		return t, nil
	}
	t, err := c.Tree()
	if err != nil {
		return nil, fmt.Errorf("triggers: tree of %s: %w", c.Hash, err)
	}
	tt.trees[c.Hash] = t
	return t, nil
}

// blobHashAt returns the blob hash path carries in c's tree, or ZeroHash when
// the path is absent there.
func (tt *triggerTrees) blobHashAt(c *object.Commit, path string) (plumbing.Hash, error) {
	tree, err := tt.tree(c)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	entry, err := tree.FindEntry(path)
	if errors.Is(err, object.ErrEntryNotFound) || errors.Is(err, object.ErrDirectoryNotFound) {
		return plumbing.ZeroHash, nil
	}
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("triggers: entry %q at %s: %w", path, c.Hash, err)
	}
	return entry.Hash, nil
}

// Toucher implements TriggerTrees: the commit that introduced the blob `path`
// carries at head (or, for a path absent at head, the commit that removed it).
//
// The rule is git's own history simplification, restricted to one path and
// with no clock:
//   - start at head; at each commit compare the path's blob with its parents';
//   - follow the FIRST parent, in parent order, that carries the SAME blob
//     (the change came through that side, so merges are skipped);
//   - a commit with NO same-blob parent is the toucher: a non-merge whose
//     parent differs (the usual case), or a merge that produced the blob
//     itself (MergeToucher);
//   - a commit with no parent and no toucher found means the path never had
//     an origin in this history (a scrubbed path after a rewind): Found=false.
//
// It is NOT bounded with IsAncestor(candidate, W): that cost ~30 ms per path
// in the reviewer's probe. In a linear advance every learn/update/retract
// ends at a real toucher inside the advance; the dispatcher does not call it
// for a retract in a NONLINEAR advance, where the same-absent-blob walk would
// run to the root.
func (tt *triggerTrees) Toucher(ctx context.Context, head plumbing.Hash, path string) (TouchResult, error) {
	cur, err := tt.commit(head)
	if err != nil {
		return TouchResult{}, err
	}
	curBlob, err := tt.blobHashAt(cur, path)
	if err != nil {
		return TouchResult{}, err
	}
	res := TouchResult{}
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		n := cur.NumParents()
		if n == 0 {
			return res, nil // Found stays false
		}
		var same *object.Commit
		for i := 0; i < n; i++ {
			p, err := tt.commit(cur.ParentHashes[i])
			if err != nil {
				return res, err
			}
			pb, err := tt.blobHashAt(p, path)
			if err != nil {
				return res, err
			}
			if pb == curBlob {
				same = p
				break
			}
		}
		if same == nil {
			res.Commit, res.Found, res.MergeToucher = cur.Hash, true, n > 1
			return res, nil
		}
		res.Steps++
		cur = same
	}
}

// BlobAt implements TriggerTrees.
func (tt *triggerTrees) BlobAt(ctx context.Context, commit plumbing.Hash, path string) (string, bool, error) {
	c, err := tt.commit(commit)
	if err != nil {
		return "", false, err
	}
	tree, err := tt.tree(c)
	if err != nil {
		return "", false, err
	}
	f, err := tree.File(path)
	if errors.Is(err, object.ErrFileNotFound) || errors.Is(err, object.ErrEntryNotFound) || errors.Is(err, object.ErrDirectoryNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("triggers: file %q at %s: %w", path, commit, err)
	}
	content, err := f.Contents()
	if err != nil {
		return "", false, fmt.Errorf("triggers: contents %q at %s: %w", path, commit, err)
	}
	return content, true, nil
}

// Toucher implements TriggerIndex with a one-shot reader (see TriggerTrees).
func (rh *repoHandler) Toucher(ctx context.Context, head plumbing.Hash, path string) (TouchResult, error) {
	return rh.TreeReader().Toucher(ctx, head, path)
}

// CommitInfo implements TriggerIndex.
func (rh *repoHandler) CommitInfo(ctx context.Context, hash plumbing.Hash) (CommitInfo, error) {
	c, err := rh.repo.CommitObject(hash)
	if err != nil {
		return CommitInfo{}, fmt.Errorf("triggers: commit %s: %w", hash, err)
	}
	return CommitInfo{
		Hash:        c.Hash,
		AuthorName:  c.Author.Name,
		AuthorEmail: c.Author.Email,
		Message:     c.Message,
		Parents:     append([]plumbing.Hash(nil), c.ParentHashes...),
	}, nil
}

// CommitSignerOf implements TriggerIndex.
func (rh *repoHandler) CommitSignerOf(ctx context.Context, hash plumbing.Hash) (CommitSigner, error) {
	c, err := rh.repo.CommitObject(hash)
	if err != nil {
		return CommitSigner{}, fmt.Errorf("triggers: commit %s: %w", hash, err)
	}
	return verifyCommitSignature(c)
}

// BlobAt implements TriggerIndex with a one-shot reader (see TriggerTrees).
func (rh *repoHandler) BlobAt(ctx context.Context, commit plumbing.Hash, path string) (string, bool, error) {
	return rh.TreeReader().BlobAt(ctx, commit, path)
}

// IsAncestor implements TriggerIndex: a is b or an ancestor of b.
func (rh *repoHandler) IsAncestor(ctx context.Context, a, b plumbing.Hash) (bool, error) {
	if a == b {
		return true, nil
	}
	if a == plumbing.ZeroHash || b == plumbing.ZeroHash {
		return false, nil
	}
	ac, err := object.GetCommit(rh.gits, a)
	if err != nil {
		return false, fmt.Errorf("triggers: commit %s: %w", a, err)
	}
	bc, err := object.GetCommit(rh.gits, b)
	if err != nil {
		return false, fmt.Errorf("triggers: commit %s: %w", b, err)
	}
	return ac.IsAncestor(bc)
}

// UpstreamTip implements TriggerIndex.
func (rh *repoHandler) UpstreamTip(ctx context.Context, upstream string) (plumbing.Hash, error) {
	ref, err := rh.gits.Reference(plumbing.NewBranchReferenceName(upstream))
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return plumbing.ZeroHash, nil
	}
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("triggers: read %s: %w", upstream, err)
	}
	return ref.Hash(), nil
}

// AncestorSet implements TriggerIndex, with F09's own history walk
// (verify_walk.go). Nothing is re-verified here: the gate that advanced main
// did that, and reachability from main IS acceptance.
func (rh *repoHandler) AncestorSet(ctx context.Context, tip plumbing.Hash) (map[plumbing.Hash]bool, error) {
	set := map[plumbing.Hash]bool{}
	if err := walkHistory(rh.gits, tip, nil, func(c *object.Commit) { set[c.Hash] = true }); err != nil {
		return nil, fmt.Errorf("triggers: walk from %s: %w", tip, err)
	}
	return set, nil
}

// TriggerWatermarks implements TriggerIndex.
func (rh *repoHandler) TriggerWatermarks(ctx context.Context, branch string) (map[string]string, error) {
	rows, err := conn(ctx, rh.db).QueryContext(ctx,
		`SELECT trigger, commit_hash FROM trigger_watermarks WHERE branch = ?`, branch)
	if err != nil {
		return nil, fmt.Errorf("TriggerWatermarks: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, hash string
		if err := rows.Scan(&name, &hash); err != nil {
			return nil, fmt.Errorf("TriggerWatermarks: %w", err)
		}
		out[name] = hash
	}
	return out, rows.Err()
}

// triggerFireColumns is the column list of one trigger_fires row, in the
// order fireRowArgs produces the values.
const triggerFireColumns = `(trigger, branch, path, episode, source, commit_hash, trace, outcome, error, nonlinear,
	 range_from, range_to, evaluated, paths, fires, fires_not_logged, duration_ms, diff_ms, change_ms, fired_at,
	 recipe_source, recipe_rev, run_id, message)`

// triggerFirePlaceholder is one row's placeholders (24 columns).
const triggerFirePlaceholder = "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"

// triggerFireSelect is the SELECT list the readers scan with scanTriggerFire.
const triggerFireSelect = `SELECT id, trigger, branch, path, episode, source, commit_hash, trace,
		outcome, error, nonlinear, range_from, range_to, evaluated, paths, fires, fires_not_logged,
		duration_ms, diff_ms, change_ms, fired_at, recipe_source, recipe_rev, run_id, message FROM trigger_fires`

// fireRowBatch is how many rows one multi-row INSERT carries: 24 columns ×
// 400 rows = 9,600 bound parameters, well under SQLite's limit. Fewer, larger
// statements hold the process-wide write lock for less time than one
// statement per row, and that lock is the one fact writes wait on.
const fireRowBatch = 400

// RecordTriggerRun implements TriggerIndex (tx1 for one run).
func (rh *repoHandler) RecordTriggerRun(ctx context.Context, run TriggerRun) (int, error) {
	return rh.RecordTriggerRuns(ctx, []TriggerRun{run})
}

// RecordTriggerRuns implements TriggerIndex (tx1). One transaction, so the
// process-wide write lock (_txlock=immediate) is taken once, for a few
// multi-row inserts, never across `if` or emit.
func (rh *repoHandler) RecordTriggerRuns(ctx context.Context, runs []TriggerRun) (int, error) {
	tx, err := rh.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("RecordTriggerRun: begin: %w", err)
	}
	defer tx.Rollback()
	now := time.Now().UTC().Unix()
	placeholder := triggerFirePlaceholder
	logged := 0
	for _, run := range runs {
		rows := run.Rows
		notLogged := 0
		if len(rows) > TriggerFireRetention {
			notLogged = len(rows) - TriggerFireRetention
			rows = rows[notLogged:] // newest paths last: keep the last N
		}
		for start := 0; start < len(rows); start += fireRowBatch {
			end := start + fireRowBatch
			if end > len(rows) {
				end = len(rows)
			}
			var sb strings.Builder
			sb.WriteString("INSERT INTO trigger_fires " + triggerFireColumns + " VALUES ")
			args := make([]any, 0, (end-start)*24)
			for i, r := range rows[start:end] {
				if i > 0 {
					sb.WriteString(", ")
				}
				sb.WriteString(placeholder)
				args = append(args, r.Trigger, run.Branch, r.Path, r.Episode, r.Source, r.Commit, r.Trace,
					r.Outcome, r.Error, boolInt(r.Nonlinear), run.RangeFrom, run.RangeTo, 0, 0, 0, 0, 0, 0, 0, now,
					r.RecipeSource, r.RecipeRev, r.RunID, r.Message)
			}
			if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
				return 0, fmt.Errorf("RecordTriggerRun: fire rows: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO trigger_fires "+triggerFireColumns+" VALUES "+placeholder,
			"", run.Branch, "", "", "", "", "", TriggerOutcomeRun, "", boolInt(run.Nonlinear),
			run.RangeFrom, run.RangeTo, run.Evaluated, run.Paths, run.Fires, notLogged,
			run.DurationMS, run.DiffMS, run.ChangeMS, now, "", "", "", ""); err != nil {
			return 0, fmt.Errorf("RecordTriggerRun: run row: %w", err)
		}
		logged += len(rows)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("RecordTriggerRun: commit: %w", err)
	}
	return logged, nil
}

// RecordTriggerResults implements TriggerIndex (F07 PR 5 [M1]): the late
// result rows of recipes, each with its own branch and range, no run row, and
// the retention prune in the same transaction (the prune otherwise rides tx2,
// which a result-only flush does not run).
func (rh *repoHandler) RecordTriggerResults(ctx context.Context, rows []TriggerFire) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := rh.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("RecordTriggerResults: begin: %w", err)
	}
	defer tx.Rollback()
	now := time.Now().UTC().Unix()
	for start := 0; start < len(rows); start += fireRowBatch {
		end := min(start+fireRowBatch, len(rows))
		var sb strings.Builder
		sb.WriteString("INSERT INTO trigger_fires " + triggerFireColumns + " VALUES ")
		args := make([]any, 0, (end-start)*24)
		for i, r := range rows[start:end] {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(triggerFirePlaceholder)
			args = append(args, r.Trigger, r.Branch, r.Path, r.Episode, r.Source, r.Commit, r.Trace,
				r.Outcome, r.Error, boolInt(r.Nonlinear), r.RangeFrom, r.RangeTo, 0, 0, 0, 0, r.DurationMS, 0, 0, now,
				r.RecipeSource, r.RecipeRev, r.RunID, r.Message)
		}
		if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("RecordTriggerResults: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM trigger_fires WHERE id <= (SELECT COALESCE(MAX(id), 0) FROM trigger_fires) - ?`,
		TriggerFireRetention); err != nil {
		return fmt.Errorf("RecordTriggerResults: prune: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("RecordTriggerResults: commit: %w", err)
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// AdvanceTriggerWatermarks implements TriggerIndex (tx2).
func (rh *repoHandler) AdvanceTriggerWatermarks(ctx context.Context, branch string, set map[string]string, del []string, due []DueMark) error {
	tx, err := rh.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("AdvanceTriggerWatermarks: begin: %w", err)
	}
	defer tx.Rollback()
	// The due marks: one multi-row INSERT OR REPLACE per fireRowBatch rows (5
	// columns × 500 = 2,500 bound parameters), like the bookmarks below. Every
	// mark is written — the fire-log cap trims fire ROWS, never the record of
	// what was processed: a run that evaluated 12,000 due facts logs 10,000
	// rows and marks 12,000 facts.
	for start := 0; start < len(due); start += fireRowBatch {
		end := start + fireRowBatch
		if end > len(due) {
			end = len(due)
		}
		var sb strings.Builder
		sb.WriteString("INSERT OR REPLACE INTO trigger_due_fires(branch, trigger, path, expires_at, fired_at) VALUES ")
		args := make([]any, 0, (end-start)*5)
		for i, m := range due[start:end] {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("(?, ?, ?, ?, ?)")
			args = append(args, branch, m.Trigger, m.Path, m.ExpiresAt, m.FiredAt)
		}
		if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("AdvanceTriggerWatermarks: due marks: %w", err)
		}
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	// One multi-row upsert (chunked) rather than one statement per trigger:
	// every active trigger's bookmark moves on every run, and the write lock
	// is held for the whole transaction.
	for start := 0; start < len(names); start += fireRowBatch {
		end := start + fireRowBatch
		if end > len(names) {
			end = len(names)
		}
		var sb strings.Builder
		sb.WriteString("INSERT OR REPLACE INTO trigger_watermarks(trigger, branch, commit_hash) VALUES ")
		args := make([]any, 0, (end-start)*3)
		for i, n := range names[start:end] {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("(?, ?, ?)")
			args = append(args, n, branch, set[n])
		}
		if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("AdvanceTriggerWatermarks: set: %w", err)
		}
	}
	// A NAME that left the ontology forgets both its bookmark and its due
	// marks: one rule for both bookkeeping tables, so a renamed or
	// removed-then-re-added trigger is a new trigger (it fires the currently
	// overdue set once, like a first appearance).
	for _, n := range del {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM trigger_watermarks WHERE trigger = ? AND branch = ?`, n, branch); err != nil {
			return fmt.Errorf("AdvanceTriggerWatermarks: delete %q: %w", n, err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM trigger_due_fires WHERE branch = ? AND trigger = ?`, branch, n); err != nil {
			return fmt.Errorf("AdvanceTriggerWatermarks: delete due marks %q: %w", n, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM trigger_fires WHERE id <= (SELECT COALESCE(MAX(id), 0) FROM trigger_fires) - ?`,
		TriggerFireRetention); err != nil {
		return fmt.Errorf("AdvanceTriggerWatermarks: prune: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("AdvanceTriggerWatermarks: commit: %w", err)
	}
	return nil
}

// RecentTriggerFires implements TriggerIndex.
func (rh *repoHandler) RecentTriggerFires(ctx context.Context, branch string, limit int) ([]TriggerFire, error) {
	if limit <= 0 {
		return []TriggerFire{}, nil
	}
	return rh.queryTriggerFires(ctx, "RecentTriggerFires",
		triggerFireSelect+` WHERE branch = ? ORDER BY id DESC LIMIT ?`, branch, limit)
}

// TriggerFiresByRun implements TriggerIndex.
func (rh *repoHandler) TriggerFiresByRun(ctx context.Context, branch, id string) ([]TriggerFire, error) {
	if id == "" {
		return []TriggerFire{}, nil
	}
	return rh.queryTriggerFires(ctx, "TriggerFiresByRun",
		triggerFireSelect+` WHERE branch = ? AND run_id = ? ORDER BY id`, branch, id)
}

func (rh *repoHandler) queryTriggerFires(ctx context.Context, who, query string, args ...any) ([]TriggerFire, error) {
	rows, err := conn(ctx, rh.db).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", who, err)
	}
	defer rows.Close()
	out := []TriggerFire{}
	for rows.Next() {
		var f TriggerFire
		var nonlinear int
		var firedAt int64
		if err := rows.Scan(&f.ID, &f.Trigger, &f.Branch, &f.Path, &f.Episode, &f.Source, &f.Commit, &f.Trace,
			&f.Outcome, &f.Error, &nonlinear, &f.RangeFrom, &f.RangeTo, &f.Evaluated, &f.Paths, &f.Fires,
			&f.FiresNotLogged, &f.DurationMS, &f.DiffMS, &f.ChangeMS, &firedAt,
			&f.RecipeSource, &f.RecipeRev, &f.RunID, &f.Message); err != nil {
			return nil, fmt.Errorf("%s: %w", who, err)
		}
		f.Nonlinear = nonlinear != 0
		f.FiredAt = UTCStamp(firedAt)
		out = append(out, f)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%s: %w", who, err)
	}
	return out, nil
}

// UTCStamp renders Unix seconds as RFC 3339 in UTC with an explicit Z — the
// one rendering every F07 timestamp uses, whatever the process's local zone.
func UTCStamp(unix int64) string {
	return time.Unix(unix, 0).UTC().Format(time.RFC3339)
}

// DueCandidates implements TriggerIndex. The plan is a range on the covering
// index fact_expires_at then a probe of branch_facts_fact per row, so the cost
// is linear in the number of PAST-DATED fact versions in the table (every
// branch's, orphans included), independent of the KB's size: measured 5–11 µs
// with none due, ~5 ms with 5,000, ~22 ms with 25,000 (proposal, Apple
// M-series). Rows are ordered by path so a run's work is deterministic.
func (rh *repoHandler) DueCandidates(ctx context.Context, branch string, nowUnix int64) ([]DueCandidate, error) {
	branchID, err := rh.branchID(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("DueCandidates: %w", err)
	}
	// CROSS JOIN, not JOIN: SQLite honours the written order for CROSS JOIN,
	// and the order is the whole cost model. Left to itself the planner (with
	// or without ANALYZE) drives from branch_facts by branch_id — linear in
	// the LIVE facts on the branch on every run, even with nothing due
	// (measured: ~0.5 ms per 10,000 live facts). Driven from the covering
	// index fact_expires_at the query is linear in the PAST-DATED rows only,
	// and an idle sweep is microseconds.
	rows, err := conn(ctx, rh.db).QueryContext(ctx,
		`SELECT bf.path, fe.expires_at
		   FROM fact_expires fe
		   CROSS JOIN branch_facts bf ON bf.fact_id = fe.fact_id
		  WHERE fe.expires_at <= ?
		    AND bf.branch_id = ?
		  ORDER BY bf.path`, nowUnix, branchID)
	if err != nil {
		return nil, fmt.Errorf("DueCandidates: %w", err)
	}
	defer rows.Close()
	out := []DueCandidate{}
	for rows.Next() {
		var c DueCandidate
		if err := rows.Scan(&c.Path, &c.ExpiresAt); err != nil {
			return nil, fmt.Errorf("DueCandidates: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DueMarks implements TriggerIndex.
func (rh *repoHandler) DueMarks(ctx context.Context, branch string) (map[DueKey]int64, error) {
	rows, err := conn(ctx, rh.db).QueryContext(ctx,
		`SELECT trigger, path, expires_at FROM trigger_due_fires WHERE branch = ?`, branch)
	if err != nil {
		return nil, fmt.Errorf("DueMarks: %w", err)
	}
	defer rows.Close()
	out := map[DueKey]int64{}
	for rows.Next() {
		var k DueKey
		var at int64
		if err := rows.Scan(&k.Trigger, &k.Path, &at); err != nil {
			return nil, fmt.Errorf("DueMarks: %w", err)
		}
		out[k] = at
	}
	return out, rows.Err()
}
