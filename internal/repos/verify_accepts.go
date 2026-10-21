package repos

import (
	"database/sql"
	"fmt"
	"time"

	"knomit/internal/store/model"

	"github.com/go-git/go-git/v5/plumbing"
)

// VerifyAccepts is this instance's F09 accept list, a tenant of control.db
// (verify_accepted, control migration 000013). See model.AcceptList for what
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
func (a *VerifyAccepts) List() ([]model.Accept, error) {
	rows, err := a.db.Query(`SELECT commit_hash, COALESCE(repo_uid, ''), note FROM verify_accepted ORDER BY accepted_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("verify accept list: %w", err)
	}
	defer rows.Close()
	var out []model.Accept
	for rows.Next() {
		var x model.Accept
		if err := rows.Scan(&x.Commit, &x.RepoUID, &x.Note); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// For binds the list to one repository: a waiver matches when it names that
// repository or no repository. A nil receiver is the empty list.
func (a *VerifyAccepts) For(repoUID string) model.AcceptList {
	if a == nil {
		return nil
	}
	return acceptsFor{db: a.db, uid: repoUID}
}

type acceptsFor struct {
	db  *sql.DB
	uid string
}

func (f acceptsFor) Lookup(commit plumbing.Hash) (model.Accept, bool) {
	var x model.Accept
	err := f.db.QueryRow(
		`SELECT commit_hash, COALESCE(repo_uid, ''), note FROM verify_accepted
		  WHERE commit_hash = ? AND (repo_uid IS NULL OR repo_uid = ?)`,
		commit.String(), f.uid).Scan(&x.Commit, &x.RepoUID, &x.Note)
	if err != nil {
		// No row, or any read failure: not accepted. A waiver that cannot be
		// read waives nothing.
		return model.Accept{}, false
	}
	return x, true
}
