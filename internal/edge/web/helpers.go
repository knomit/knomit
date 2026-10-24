package web

import (
	"strings"
)

// detailWithoutTitlePrefix renders err as a problem+json `detail`, dropping a
// leading sentinel prefix that the problem's TITLE already carries.
//
// Named for what it does rather than `problemDetail`, which this package's
// lens tests already use for reading a detail back OUT of a response.
//
// A wrapped sentinel reads well in a log — "origin not allowed: local origin
// %q is outside the allowed root %q" — and badly in a problem document, where
// the title is already "Origin not allowed" and the reader gets the same
// phrase twice before reaching the part that tells them which path and which
// root. The title says WHAT KIND of refusal; the detail should say only which
// one.
//
// Prefix-stripping rather than a second message on the error, so there stays
// exactly ONE wording to keep correct. An err that does not carry the prefix
// is returned unchanged, so this is safe to apply wherever the sentinel is
// merely possible.
func detailWithoutTitlePrefix(err error, sentinel error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if sentinel == nil {
		return msg
	}
	return strings.TrimPrefix(msg, sentinel.Error()+": ")
}

// setOriginRequest is the expected JSON body for PUT /repos/{repo}/origin.
//
// Branch is the upstream consensus branch on the remote. When omitted, the
// handler keeps the existing remote's value, else the repo's own recorded
// consensus branch (remotesync.Index.UpstreamBranch); it never supplies a name of its
// own. Callers that have already discovered the right name (via the
// connectivity-test flow) should send it explicitly.
type setOriginRequest struct {
	URL        string `json:"url"`
	Branch     string `json:"branch,omitempty"`
	AuthMethod string `json:"auth_method"`
	Token      string `json:"token"`
	User       string `json:"user"`
	Password   string `json:"password"`
}
