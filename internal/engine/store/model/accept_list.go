package model

// Accept is one entry of the operator's accept list.
type Accept struct {
	Commit  string `json:"commit"`
	RepoUID string `json:"repo_uid,omitempty"` // "" = any repository
	Note    string `json:"note,omitempty"`
}

// The operator's accept list is a per-INSTANCE lookup in control.db, handed to
// a store as a func(plumbing.Hash) (Accept, bool) (Service.SetAcceptList;
// repos.VerifyAccepts.For builds it, bound to one repository). Its only reader
// is E4: an accept waives the own-lineage refusal for one commit on origin's
// copy of this instance's agent branch (for example commits made before
// signing existed). The repo-database table verify_accepted (repo migration
// 000027) is never read or written.
