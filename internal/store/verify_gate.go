package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// VerifyReport is what one upstream advance's verification found. It rides on
// MainReconcileResult (and so on the sync SSE event); nil means verification
// is off and there was nothing to say.
type VerifyReport struct {
	Mode     string    `json:"mode"`
	Anchor   string    `json:"anchor,omitempty"`
	Held     bool      `json:"held,omitempty"` // the local upstream stopped below origin: refs advance up to the last good commit and stop there
	Unrooted bool      `json:"unrooted,omitempty"`
	Rewind   bool      `json:"rewind,omitempty"` // origin no longer contains the anchor
	Checks   int       `json:"checks"`           // signature and M3 evaluations
	Refused  []Refusal `json:"refused,omitempty"`
	Reported []Refusal `json:"reported,omitempty"`
}

// Blocking reports whether this advance was REFUSED (enforce, or an
// unjudgeable policy change), as opposed to logged.
func (r *VerifyReport) Blocking() bool {
	if r == nil {
		return false
	}
	closed := r.Mode == VerifyEnforce || r.Unrooted
	return closed && (len(r.Refused) > 0 || r.Rewind)
}

// ErrVerifyFailed is returned by Sync when verification refused part of an
// upstream advance. Match with errors.Is; errors.As a *VerifyFailedError for
// the report.
var ErrVerifyFailed = errors.New("upstream verification refused commits")

// VerifyFailedError carries the report of a refused advance.
type VerifyFailedError struct{ Report *VerifyReport }

func (e *VerifyFailedError) Error() string {
	switch {
	case e.Report == nil:
		return ErrVerifyFailed.Error()
	case e.Report.Rewind:
		return "upstream verification: origin was rewound past the verified anchor; the local upstream is held (operator: knomit verify accept, or re-anchor)"
	case len(e.Report.Refused) > 0:
		r := e.Report.Refused[0]
		return fmt.Sprintf("upstream verification refused %d commit(s); first %s: %s (%s)",
			len(e.Report.Refused), shortRefHash(plumbing.NewHash(r.Commit)), r.Reason, r.Rule)
	}
	return ErrVerifyFailed.Error()
}

func (e *VerifyFailedError) Is(target error) bool { return target == ErrVerifyFailed }

// verifyAdvance decides where the local upstream may move for a fetched
// origin tip. It returns the target (origin itself unless verification closes
// the advance) and the report.
//
//   - off: the target is origin; the off-scan watermark advances (tree reads
//     only, no signature or M3 evaluation).
//   - log: the target is origin; the anchor stops before the first failure.
//   - enforce, or unrooted: the target is the new anchor. Refs advance up to
//     the last good commit and stop there.
//   - Never backwards: a target that is an ancestor of the local upstream
//     leaves it where it is.
//   - A rewind (origin no longer contains the anchor) holds the upstream in
//     enforce and moves it in log, WITHOUT moving the anchor, in every mode.
//
// Caller holds lockBranch(upstream).
func (rh *repoHandler) verifyAdvance(ctx context.Context, upstream string, local, origin plumbing.Hash) (plumbing.Hash, *VerifyReport, error) {
	start, actx, err := rh.verifyStart(ctx, upstream)
	if err != nil {
		return plumbing.ZeroHash, nil, err
	}
	if start == origin {
		return origin, nil, nil
	}
	v := rh.verifier(ctx)
	if start != plumbing.ZeroHash {
		v.below = rh.cachedBelow(upstream, start)
	}
	res, err := v.fold(start, actx, origin)
	if err != nil {
		return plumbing.ZeroHash, nil, err
	}
	if res.Rewind && actx.Mode == VerifyOff {
		// An off repo has no anchor to protect: re-scan from the root.
		if res, err = v.fold(plumbing.ZeroHash, actx, origin); err != nil {
			return plumbing.ZeroHash, nil, err
		}
	}
	rep := &VerifyReport{
		Mode: res.Final.Mode, Unrooted: res.Unrooted, Rewind: res.Rewind, Checks: res.SigChecks,
		Refused: res.Refused, Reported: res.Reported,
	}
	if res.Rewind {
		rep.Mode = actx.Mode
		rep.Anchor = start.String()
		if actx.Mode == VerifyEnforce {
			rep.Held = true
			return local, rep, nil
		}
		return origin, rep, nil
	}
	if err := rh.saveVerify(ctx, upstream, res); err != nil {
		return plumbing.ZeroHash, nil, err
	}
	rh.extendBelow(upstream, v.below, res)
	if res.NewAnchor != plumbing.ZeroHash && res.NewAnchorCtx.Mode != VerifyOff {
		rep.Anchor = res.NewAnchor.String()
	}
	target := origin
	if res.Closed() {
		target = res.NewAnchor
	}
	if target == plumbing.ZeroHash {
		target = local // nothing acceptable at all: hold
	}
	if local != plumbing.ZeroHash && target != local {
		back, err := isAncestorCommit(rh, target, local)
		if err != nil {
			return plumbing.ZeroHash, nil, err
		}
		if back {
			target = local // never move the upstream backwards
		}
	}
	rep.Held = target != origin
	if rep.Mode == VerifyOff && len(rep.Reported) == 0 && !rep.Unrooted {
		return target, nil, nil
	}
	return target, rep, nil
}

// isAncestorCommit reports whether a is an ancestor of (or equal to) b.
func isAncestorCommit(rh *repoHandler, a, b plumbing.Hash) (bool, error) {
	if a == b {
		return true, nil
	}
	if a == plumbing.ZeroHash || b == plumbing.ZeroHash {
		return false, nil
	}
	ac, err := object.GetCommit(rh.gits, a)
	if err != nil {
		return false, err
	}
	bc, err := object.GetCommit(rh.gits, b)
	if err != nil {
		return false, err
	}
	return ac.IsAncestor(bc)
}

// verifyBelowCache is the set of commits reachable from one anchor.
type verifyBelowCache struct {
	anchor plumbing.Hash
	set    map[plumbing.Hash]bool
}

// cachedBelow returns the cached history of anchor for upstream, or nil.
func (rh *repoHandler) cachedBelow(upstream string, anchor plumbing.Hash) map[plumbing.Hash]bool {
	rh.verifyBelowMu.Lock()
	defer rh.verifyBelowMu.Unlock()
	if c := rh.verifyBelow[upstream]; c != nil && c.anchor == anchor {
		return c.set
	}
	return nil
}

// extendBelow records the new anchor's history: the old anchor's plus the
// range commits the anchor advanced over. below is the set the fold used (the
// cache, or the one it walked).
func (rh *repoHandler) extendBelow(upstream string, below map[plumbing.Hash]bool, res foldResult) {
	if res.NewAnchor == plumbing.ZeroHash || below == nil {
		return
	}
	for _, h := range res.Advanced {
		below[h] = true
	}
	rh.verifyBelowMu.Lock()
	defer rh.verifyBelowMu.Unlock()
	if rh.verifyBelow == nil {
		rh.verifyBelow = map[string]*verifyBelowCache{}
	}
	rh.verifyBelow[upstream] = &verifyBelowCache{anchor: res.NewAnchor, set: below}
}
