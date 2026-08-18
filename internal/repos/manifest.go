package repos

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
	"knomit/internal/store"
)

var (
	// ErrRepoDescriptionTooLong is returned by WriteReadme when content
	// exceeds MaxRepoDescriptionBytes. Mirrors ErrLensDescriptionTooLong: the
	// cap is a pure input check, enforced here rather than at the HTTP edge so
	// every writer of README.md is bound by it.
	ErrRepoDescriptionTooLong = errors.New("repo description too long")
	// ErrAgentBranchUnset is returned when the repo has no agent branch yet, so
	// there is no ref to read the manifest from or commit it to.
	ErrAgentBranchUnset = errors.New("repo has no agent branch")
	// ErrLicenseTooLargeToReplace is returned by WriteLicense when the LICENSE
	// already on the agent branch exceeds MaxRepoDescriptionBytes. ReadFact has
	// no size cap of its own, so that read succeeds; without this guard
	// WriteLicense would treat "exists but too large to show" as ordinary
	// content, and a blank "Add license" draft (offered because the UI could
	// not display the oversize file and looked empty) would silently commit
	// over it. Refusing beats guessing: there is no safe rewrite of a file this
	// call never actually read.
	ErrLicenseTooLargeToReplace = errors.New("existing LICENSE exceeds the size cap; refusing to replace it blindly")
)

// MaxRepoDescriptionBytes caps the README.md root manifest. Byte length, not
// rune count: it bounds stored + wire size. Deliberately far larger than
// MaxLensDescriptionBytes — a lens description is a one-line note about a read
// union, whereas README.md is a repo's root manifest and routinely runs to
// several pages of guidance for the agents reading it.
const MaxRepoDescriptionBytes = 64 * 1024

// ReadmePath is the repo's root manifest, at the root of the tree rather than
// under the ontology root: it is not a fact. The search indexer skips it (the
// ontology-root location rule) and the commits list filters to ontologyRoot, so
// writing it moves the file and its commit-log row, never the fact index or the
// history UI.
//
// The name is not knomit's choice — every major git provider renders README.md
// at the repo root as the repository description, which is exactly what this
// file is. Case matters to that lookup, so writes go through WriteRootFile.
const ReadmePath = "README.md"

// readmeCommitMsg is the commit subject for every manifest edit, so the git log
// reads uniformly regardless of which client made the change.
const readmeCommitMsg = "docs: update README.md"

// ReadReadme returns the verbatim content of README.md at the tip of the
// repo's agent branch. A missing manifest is not an error — it returns "" with
// a nil error, because "this repo has no description" is an ordinary state.
func (ri *RepoInstance) ReadReadme(ctx context.Context) (string, error) {
	branch := ri.agentBranch
	if branch == "" {
		return "", ErrAgentBranchUnset
	}
	var content string
	// WithRead's contract: fn does not run unless a live service is available,
	// and the error says why — so there is no nil svc to guard against here.
	err := ri.WithRead(func(svc *store.Service) {
		res, rerr := svc.Facts().ReadFact(ctx, branch, ReadmePath, nil)
		if rerr != nil {
			return // absent (or unreadable) — no description to report
		}
		content = res.Content
	})
	return content, err
}

// WriteReadme commits content to README.md on the repo's agent branch — the
// exact file and branch ReadReadme reads, so an edit round-trips. It reports
// whether a commit was made: a byte-identical manifest is skipped, because the
// store's write path always builds a fresh commit object and re-saving
// unchanged text would otherwise append an empty commit to the agent branch
// (and push it to the remote).
//
// The read-compare-write is not atomic. That is the same last-write-wins
// contract the fact-write path already has: a racing writer can turn this call
// into a no-op, and the loser's content is what a subsequent read returns.
func (ri *RepoInstance) WriteReadme(ctx context.Context, content string) (committed bool, err error) {
	if len(content) > MaxRepoDescriptionBytes {
		return false, fmt.Errorf("%w: %d bytes exceeds the maximum of %d",
			ErrRepoDescriptionTooLong, len(content), MaxRepoDescriptionBytes)
	}
	branch := ri.agentBranch
	if branch == "" {
		return false, ErrAgentBranchUnset
	}
	var writeErr error
	// Capture WithRead's own error separately: it reports a closed or detached
	// store, in which case fn never ran at all. Dropping it would turn "the
	// write never happened" into a silent success.
	acquireErr := ri.WithRead(func(svc *store.Service) {
		if cur, rerr := svc.Facts().ReadFact(ctx, branch, ReadmePath, nil); rerr == nil && cur.Content == content {
			return
		}
		if _, werr := svc.Facts().WriteRootFile(ctx, branch, ReadmePath,
			content, readmeCommitMsg, "update"); werr != nil {
			writeErr = werr
			return
		}
		committed = true
	})
	if acquireErr != nil {
		return false, acquireErr
	}
	return committed, writeErr
}

// OntologyPath is the ontology definition, inside knomit's own private
// namespace: it is configuration, not knowledge, so it sits outside fact
// discovery entirely (see fact.IsPrivatePath). knomit reads it by name, which
// the private rule explicitly permits.
//
// It is a loose file at the ROOT of the namespace, which is what makes it
// server-owned: fact.IsWritablePrivatePath requires at least one subdirectory
// AND a dotless <area>, so no agent can rewrite the ontology through the fact
// tools — neither by naming it directly (depth) nor by reusing its name as a
// DIRECTORY, which would replace the blob with a tree and silently drop the
// repo onto the embedded default taxonomy (dotless area).
//
// Defined in terms of fact.OntologyFile (not redeclared) so every existing
// caller of repos.OntologyPath keeps working unchanged.
const OntologyPath = fact.OntologyFile

// LegacyOntologyPath is where the ontology lived before knomit's private data
// was consolidated under fact.PrivateRoot, and PreDotOntologyPath is where it
// lived before that. Read-only, read after the canonical path in that order:
// no migration is provided (repos are updated by hand), so an unmigrated repo
// must keep validating against ITS ontology rather than silently falling back
// to the embedded default — which would validate new facts against the wrong
// taxonomy, with nothing in the logs tying the bad facts to the cause.
//
// The pre-dot rung is not vestigial. .domains/ existed for six days before
// .knomit/ replaced it, so a repo that skipped the hand-migration is far more
// likely to sit on domains/ than on .domains/.
const (
	LegacyOntologyPath = fact.LegacyOntologyFile
	PreDotOntologyPath = fact.PreDotOntologyFile
)

// serverOwnedPaths is every file knomit owns and writes through its own
// dedicated code — never through the fact endpoints, which take their path
// verbatim from the caller and perform no fact-shape check.
//
// The dot-prefixed ontology rungs are already refused by the private-path
// guard; they are listed anyway so this reads as the whole set rather than as
// "the leftovers". The others are NOT private and get no protection from that
// guard at all: README.md and LICENSE are resolved by exact name at the tree
// root, and domains/ontology.yaml is the one ontology rung with no dot in it.
var serverOwnedPaths = []string{
	ReadmePath,
	LicensePath,
	OntologyPath,
	LegacyOntologyPath,
	PreDotOntologyPath,
}

// IsServerOwnedPath reports whether path names a file knomit owns, or reuses
// such a name as a DIRECTORY.
//
// Matched case-INSENSITIVELY, because that is how these files are actually
// reachable. A fact path is lowercased on its way to git (store.writeFile), so
// a PUT to "README.md" does not overwrite the manifest — it plants a separate
// root file "readme.md", which GitHub and GitLab, resolving that name
// case-insensitively, would render as the repository's README while knomit
// goes on reporting the real one. "LICENSE" plants "license" the same way, and
// there is no legitimate write path for a licence at all: it is authored by
// whoever owns the repo, and knomit only reports it. Neither passes the size
// cap and exact-case WriteRootFile door that WriteReadme goes through.
//
// The directory form is refused for the same reason .knomit/<area> must be
// dotless: git replaces a same-named blob with a tree, so writing
// "domains/ontology.yaml/x.md" destroys that ontology as surely as overwriting
// it would.
func IsServerOwnedPath(path string) bool {
	lower := strings.ToLower(path)
	for _, p := range serverOwnedPaths {
		owned := strings.ToLower(p)
		if lower == owned || strings.HasPrefix(lower, owned+"/") {
			return true
		}
	}
	return false
}

// LicensePath is the terms under which the KB's content is published, at the
// tree root beside README.md. Like the manifest it is not a fact, and like the
// manifest git providers look for it by this exact name.
//
// Readable AND writable, but never GENERATED: knomit round-trips whatever terms
// the repo owner supplies and offers no template picker, so it stays out of the
// business of producing legal text. The editor starts blank by design.
//
// Deliberately this one spelling only. ReadLicense below resolves it through
// ReadFact, which bottoms out in go-git's Tree.FindEntry — an EXACT tree-entry
// lookup, not a case-insensitive one. So "license" and "License" are not found
// either, nor are "LICENSE.md", "LICENSE.txt" and "COPYING". A known limit, not
// an oversight; widening it is a real feature and out of scope here.
const LicensePath = "LICENSE"

// ReadLicense returns the verbatim content of LICENSE at the tip of the repo's
// agent branch. A missing licence is not an error — it returns "", false, nil
// error, because "this KB states no terms" is an ordinary state.
//
// oversize reports whether a LICENSE exists but exceeds MaxRepoDescriptionBytes
// — content is "" in that case too, but a caller MUST be able to tell "no
// LICENSE" from "a LICENSE that is too large to show": those are different
// states with different safe UIs. Treating the second as the first is exactly
// the bug this return value closes — see WriteLicense's ErrLicenseTooLargeToReplace
// guard for the write-side half. WriteLicense rejects oversized input at the
// door, so an oversize LICENSE here only arrives some other way — a clone, or
// a hand-edited working tree.
func (ri *RepoInstance) ReadLicense(ctx context.Context) (content string, oversize bool, err error) {
	branch := ri.agentBranch
	if branch == "" {
		return "", false, ErrAgentBranchUnset
	}
	err = ri.WithRead(func(svc *store.Service) {
		res, rerr := svc.Facts().ReadFact(ctx, branch, LicensePath, nil)
		if rerr != nil {
			return // absent (or unreadable) — no terms to report
		}
		if len(res.Content) > MaxRepoDescriptionBytes {
			log.Warn().Str("repo", ri.Name()).Int("bytes", len(res.Content)).
				Msg("LICENSE exceeds the read guard; reporting oversize rather than content")
			oversize = true
			return
		}
		content = res.Content
	})
	return content, oversize, err
}

// licenseCommitMsg is the commit subject for every licence edit, so the git log
// reads uniformly regardless of which client made the change.
const licenseCommitMsg = "docs: update LICENSE"

// WriteLicense commits content to LICENSE on the repo's agent branch — the
// exact file and branch ReadLicense reads, so an edit round-trips. It reports
// whether a commit was made: a byte-identical licence is skipped, because the
// store's write path always builds a fresh commit object and re-saving
// unchanged text would otherwise append an empty commit to the agent branch
// (and push it to the remote).
//
// Reuses MaxRepoDescriptionBytes and ErrRepoDescriptionTooLong rather than
// minting licence-specific twins: it is the same cap on the same kind of root
// manifest, and a second sentinel would need a second mapping arm at the HTTP
// edge saying exactly the same thing. GPL-3 is ~35KB against a 64KB cap.
//
// The read-compare-write is not atomic — the same last-write-wins contract
// WriteReadme and the fact-write path already have.
//
// An empty content is skipped too, but only when LICENSE does not already
// exist: ReadFact then errors, and the byte-identical check above never
// fires, so without this a blank "Add license" draft would commit an empty
// file — one that ReadLicense reads back as "" and the UI treats as no
// licence at all, i.e. a junk commit for a file that appears not to exist.
// Saving "" over an EXISTING licence is the legitimate "clear it" action and
// must still write, so the distinguishing signal is whether ReadFact
// succeeded, not the content by itself.
//
// ReadFact enforces NO size cap of its own — unlike ReadLicense, which reports
// an over-cap LICENSE as oversize rather than streaming its content (see its
// doc comment). So ReadFact below still succeeds for a >64KiB LICENSE, and
// without the explicit check first, this method would fall through to either
// the byte-identical branch (content will never match, since the caller never
// saw the real bytes) or the WriteRootFile call at the bottom — meaning a
// blank "Add license" draft, offered because the UI could not display a file
// it never read, would silently commit an empty LICENSE over the original.
// Refusing the write and reporting ErrLicenseTooLargeToReplace closes that:
// there is no safe rewrite of content this call cannot see.
func (ri *RepoInstance) WriteLicense(ctx context.Context, content string) (committed bool, err error) {
	if len(content) > MaxRepoDescriptionBytes {
		return false, fmt.Errorf("%w: %d bytes exceeds the maximum of %d",
			ErrRepoDescriptionTooLong, len(content), MaxRepoDescriptionBytes)
	}
	branch := ri.agentBranch
	if branch == "" {
		return false, ErrAgentBranchUnset
	}
	var writeErr error
	// Capture WithRead's own error separately: it reports a closed or detached
	// store, in which case fn never ran at all. Dropping it would turn "the
	// write never happened" into a silent success.
	acquireErr := ri.WithRead(func(svc *store.Service) {
		cur, rerr := svc.Facts().ReadFact(ctx, branch, LicensePath, nil)
		if rerr == nil && len(cur.Content) > MaxRepoDescriptionBytes {
			writeErr = fmt.Errorf("%w: existing LICENSE is %d bytes",
				ErrLicenseTooLargeToReplace, len(cur.Content))
			return
		}
		if rerr == nil && cur.Content == content {
			return // byte-identical to what is already there
		}
		if rerr != nil && content == "" {
			return // nothing to clear: no licence exists, and none is being added
		}
		if _, werr := svc.Facts().WriteRootFile(ctx, branch, LicensePath,
			content, licenseCommitMsg, "update"); werr != nil {
			writeErr = werr
			return
		}
		committed = true
	})
	if acquireErr != nil {
		return false, acquireErr
	}
	return committed, writeErr
}
