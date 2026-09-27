package repos

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-git/go-git/v5/plumbing"

	"knomit/internal/store"
)

// VerifyAccepts is this instance's F09 accept list, a tenant of control.db
// (verify_accepted, control migration 000012). See store.AcceptList for what
// an accept waives, and does not.
type VerifyAccepts struct{ db *sql.DB }

// OpenVerifyAccepts borrows control.db's handle.
func OpenVerifyAccepts(db *sql.DB) *VerifyAccepts { return &VerifyAccepts{db: db} }

// Add records a waiver for commit. repoUID "" makes it apply in any repository.
func (a *VerifyAccepts) Add(commit plumbing.Hash, repoUID, note string) error {
	var uid any
	if repoUID != "" {
		uid = repoUID
	}
	_, err := a.db.Exec(
		`INSERT OR REPLACE INTO verify_accepted(commit_hash, repo_uid, note, accepted_at) VALUES (?, ?, ?, ?)`,
		commit.String(), uid, note, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("verify accept: %w", err)
	}
	return nil
}

// List returns every waiver, newest first.
func (a *VerifyAccepts) List() ([]store.Accept, error) {
	rows, err := a.db.Query(`SELECT commit_hash, COALESCE(repo_uid, ''), note FROM verify_accepted ORDER BY accepted_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("verify accept list: %w", err)
	}
	defer rows.Close()
	var out []store.Accept
	for rows.Next() {
		var x store.Accept
		if err := rows.Scan(&x.Commit, &x.RepoUID, &x.Note); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// For binds the list to one repository: a waiver matches when it names that
// repository or no repository. A nil receiver is the empty list.
func (a *VerifyAccepts) For(repoUID string) store.AcceptList {
	if a == nil {
		return nil
	}
	return acceptsFor{db: a.db, uid: repoUID}
}

type acceptsFor struct {
	db  *sql.DB
	uid string
}

func (f acceptsFor) Lookup(commit plumbing.Hash) (store.Accept, bool) {
	var x store.Accept
	err := f.db.QueryRow(
		`SELECT commit_hash, COALESCE(repo_uid, ''), note FROM verify_accepted
		  WHERE commit_hash = ? AND (repo_uid IS NULL OR repo_uid = ?)`,
		commit.String(), f.uid).Scan(&x.Commit, &x.RepoUID, &x.Note)
	if err != nil {
		// No row, or any read failure: not accepted. A waiver that cannot be
		// read waives nothing.
		return store.Accept{}, false
	}
	return x, true
}

// ErrUnwaivable refuses an accept the fold would never honour.
var ErrUnwaivable = errors.New("an accept never waives a policy change or an author claim")

// CheckWaivable refuses to accept commit when the repository's own dry run
// (store.Service.SignatureReport) already shows it as an unauthorised policy
// change or an author-claim mismatch: the fold would ignore the waiver, and a
// silent no-op waiver is how an operator comes to believe a refusal is fixed.
func CheckWaivable(rep store.SignatureReport, commit plumbing.Hash) error {
	for _, rs := range [][]store.Refusal{rep.Refused, rep.Reported} {
		for _, r := range rs {
			if r.Commit != commit.String() {
				continue
			}
			if r.Rule == store.RulePolicyChange || r.Rule == store.RuleAuthorClaim {
				return fmt.Errorf("%w: %s is refused for %q (%s)", ErrUnwaivable, commit, r.Rule, r.Reason)
			}
		}
	}
	return nil
}
