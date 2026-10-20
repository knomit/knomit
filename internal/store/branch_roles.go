package store

import (
	"errors"
	"strings"
)

// generatedRefPrefix is the branch-name prefix the removed server-side OKF
// export wrote under refs/heads/. Nothing creates these any more (the producer
// went in bf9becbe, and TestVerify_NoGeneratedRefs pins that), so a ref matching
// it on a live home is residue from before that commit.
//
// Deliberately narrow. knomit-okf's own fetched source history lives at
// refs/knomit-okf/source/*, OUTSIDE refs/heads, and is live, deliberate and not
// matched here — see kb/invariants/okf/source-refs-stay-local.
const generatedRefPrefix = "okf/"

// isGeneratedRef reports whether a BRANCH NAME (no refs/heads/ prefix) is
// export residue.
func isGeneratedRef(branch string) bool {
	return strings.HasPrefix(branch, generatedRefPrefix)
}

// ErrNoConsensusBranch reports that no consensus branch could be established:
// none was requested, and the remote (or the stored origin) names none that
// can be adopted without guessing. The caller must name one.
var ErrNoConsensusBranch = errors.New("no consensus branch: none was named and none can be determined")

// IsRoleBranch reports whether branch already has a role that is not
// consensus: an agent branch (this machine's or a peer's), an experiment, or a
// generated okf branch. Such a branch is never adopted as a consensus branch
// on its own authority, whatever a remote's HEAD says.
func IsRoleBranch(branch string) bool {
	return strings.HasPrefix(branch, "agent/") || strings.HasPrefix(branch, "exp/") || isGeneratedRef(branch)
}

// ChooseConsensusBranch is the consensus-branch rule for a repository that has
// branches (a remote after the fetch, or a probe's ref listing): the requested
// name; else the remote's symbolic HEAD when it names one of branches and has
// no other role (IsRoleBranch); else the one branch that has no other role.
// Otherwise "" (several candidates, or none) and the caller refuses with
// ErrNoConsensusBranch rather than picking one by its name.
//
// It never prefers a branch because of what it is called. A remote whose HEAD
// is an agent branch (the #82 case) falls through to the single remaining
// candidate; a remote holding main and trunk with HEAD on trunk answers trunk.
func ChooseConsensusBranch(requested, head string, branches []string) string {
	if requested != "" {
		return requested
	}
	if head != "" && !IsRoleBranch(head) {
		for _, b := range branches {
			if b == head {
				return head
			}
		}
	}
	var cand []string
	for _, b := range branches {
		if !IsRoleBranch(b) {
			cand = append(cand, b)
		}
	}
	if len(cand) == 1 {
		return cand[0]
	}
	return ""
}
