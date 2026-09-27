package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// The verified anchor of an upstream: the furthest commit whose history the
// verifier fully accepted. A git ref beside the agent-base watermark, so no
// fetch refspec, /git advertisement or push ever touches it. Written only
// while verification is on; an off repo keeps an off-scan watermark in meta
// instead (verify_scan:<upstream>), so "off never writes the anchor ref".
func verifiedRefName(upstream string) plumbing.ReferenceName {
	return plumbing.ReferenceName("refs/knomit/verified/" + upstream)
}

func verifyScanKey(upstream string) string { return "verify_scan:" + upstream }

// SetRootOfTrust installs F09's root of trust (the operator key). Like
// SetSigner it must be re-applied on every store reopen (rewireStore).
func (s *Service) SetRootOfTrust(r RootOfTrust) { s.rh.verifyRoot = r }

func (rh *repoHandler) rootOfTrust() RootOfTrust {
	if rh.verifyRoot == nil {
		return StaticRoot{}
	}
	return rh.verifyRoot
}

// verifier builds the fold over this store's objects, root and waivers.
func (rh *repoHandler) verifier(ctx context.Context) *verifier {
	return &verifier{
		st:   rh.gits,
		root: rh.rootOfTrust(),
		accepted: func(h plumbing.Hash) bool {
			if rh.acceptList == nil {
				return false
			}
			_, ok := rh.acceptList.Lookup(h)
			return ok
		},
	}
}

// verifyStart is where the next fold for upstream begins: the anchor and the
// context in force at it, the off-scan watermark (context off), or the root
// (ZeroHash, off) on first contact.
//
// The anchor's context comes from the verify_context cache. On a miss it is
// RECOMPUTED by folding the history from the root to the anchor, never read
// from the anchor commit's ontology file (V5-2): that file may carry a policy
// change the fold rejected.
func (rh *repoHandler) verifyStart(ctx context.Context, upstream string) (plumbing.Hash, verifyContext, error) {
	off := verifyContext{Mode: VerifyOff}
	if ref, err := rh.gits.Reference(verifiedRefName(upstream)); err == nil {
		anchor := ref.Hash()
		if c, ok, err := rh.cachedContext(ctx, anchor); err != nil {
			return plumbing.ZeroHash, off, err
		} else if ok {
			return anchor, c, nil
		}
		res, err := rh.verifier(ctx).fold(plumbing.ZeroHash, off, anchor)
		if err != nil {
			return plumbing.ZeroHash, off, fmt.Errorf("verify: recompute anchor context: %w", err)
		}
		if err := rh.storeContext(ctx, anchor, res.Final); err != nil {
			return plumbing.ZeroHash, off, err
		}
		return anchor, res.Final, nil
	} else if !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return plumbing.ZeroHash, off, fmt.Errorf("verify: read anchor: %w", err)
	}
	var scan string
	err := conn(ctx, rh.db).QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, verifyScanKey(upstream)).Scan(&scan)
	switch {
	case err == nil && plumbing.IsHash(scan):
		return plumbing.NewHash(scan), off, nil
	case err == nil, errors.Is(err, sql.ErrNoRows):
		return plumbing.ZeroHash, off, nil
	default:
		return plumbing.ZeroHash, off, fmt.Errorf("verify: read scan watermark: %w", err)
	}
}

// saveVerify records where the fold ended. With verification on, the anchor
// ref moves to res.NewAnchor and its context is cached; the off-scan watermark
// is dropped. With it off (never enabled, or a verified disable), the anchor
// ref is retired and the watermark records how far the history was scanned.
//
// It never moves the anchor for a rewind (the caller does not call it then).
func (rh *repoHandler) saveVerify(ctx context.Context, upstream string, res foldResult) error {
	if res.NewAnchor == plumbing.ZeroHash {
		return nil
	}
	if res.NewAnchorCtx.Mode != VerifyOff {
		if err := rh.storeContext(ctx, res.NewAnchor, res.NewAnchorCtx); err != nil {
			return err
		}
		if err := rh.gits.SetReference(plumbing.NewHashReference(verifiedRefName(upstream), res.NewAnchor)); err != nil {
			return fmt.Errorf("verify: write anchor: %w", err)
		}
		_, err := conn(ctx, rh.db).ExecContext(ctx, `DELETE FROM meta WHERE key = ?`, verifyScanKey(upstream))
		return err
	}
	if err := rh.gits.RemoveReference(verifiedRefName(upstream)); err != nil && !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return fmt.Errorf("verify: retire anchor: %w", err)
	}
	_, err := conn(ctx, rh.db).ExecContext(ctx,
		`INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`, verifyScanKey(upstream), res.NewAnchor.String())
	return err
}

func (rh *repoHandler) cachedContext(ctx context.Context, h plumbing.Hash) (verifyContext, bool, error) {
	var mode, signers string
	err := conn(ctx, rh.db).QueryRowContext(ctx,
		`SELECT mode, signers FROM verify_context WHERE commit_hash = ?`, h.String()).Scan(&mode, &signers)
	if errors.Is(err, sql.ErrNoRows) {
		return verifyContext{}, false, nil
	}
	if err != nil {
		return verifyContext{}, false, fmt.Errorf("verify: read context: %w", err)
	}
	c := verifyContext{Mode: mode}
	if err := json.Unmarshal([]byte(signers), &c.Signers); err != nil {
		return verifyContext{}, false, nil // a corrupt row is a miss: recompute
	}
	return c, true, nil
}

func (rh *repoHandler) storeContext(ctx context.Context, h plumbing.Hash, c verifyContext) error {
	signers, err := json.Marshal(c.Signers)
	if err != nil {
		return err
	}
	_, err = conn(ctx, rh.db).ExecContext(ctx,
		`INSERT OR REPLACE INTO verify_context(commit_hash, mode, signers, computed_at) VALUES (?, ?, ?, ?)`,
		h.String(), c.Mode, string(signers), time.Now().Unix())
	if err != nil {
		return fmt.Errorf("verify: write context: %w", err)
	}
	return nil
}

// Accept is one entry of the operator's accept list.
type Accept struct {
	Commit  string `json:"commit"`
	RepoUID string `json:"repo_uid,omitempty"` // "" = any repository
	Note    string `json:"note,omitempty"`
}

// AcceptList is the operator's per-INSTANCE accept list (control.db, user
// ruling for PR 4). An accept waives a failing signature on one commit, or an
// unsigned merge that fails merge rule M3. It never waives an unaccepted
// parent, a policy change or an author claim: the fold enforces that scope.
// The repo-database table verify_accepted (repo migration 000027) is never
// read or written.
type AcceptList interface {
	Lookup(commit plumbing.Hash) (Accept, bool)
}

// CommitExists reports whether this repository's object store holds commit h.
func (s *Service) CommitExists(h plumbing.Hash) bool {
	_, err := object.GetCommit(s.rh.gits, h)
	return err == nil
}

// SetAcceptList installs the accept list, bound to this repository. Like
// SetRootOfTrust it must be re-applied on every store reopen.
func (s *Service) SetAcceptList(a AcceptList) { s.rh.acceptList = a }
