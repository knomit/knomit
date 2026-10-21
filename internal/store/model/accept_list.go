package model

import (
	"github.com/go-git/go-git/v5/plumbing"
)

// Accept is one entry of the operator's accept list.
type Accept struct {
	Commit  string `json:"commit"`
	RepoUID string `json:"repo_uid,omitempty"` // "" = any repository
	Note    string `json:"note,omitempty"`
}

// AcceptList is the operator's per-INSTANCE accept list (control.db). Its only
// reader is E4: an accept waives the own-lineage refusal for one commit on
// origin's copy of this instance's agent branch (for example commits made
// before signing existed). The repo-database table verify_accepted (repo
// migration 000027) is never read or written.
type AcceptList interface {
	Lookup(commit plumbing.Hash) (Accept, bool)
}
