// Remote origins: the machine-local record of where each repo syncs from and
// with what credential. A tenant of <home>/control.db, keyed by registry uid.
//
// This is what makes a lost repo recoverable: the .db file holds the knowledge
// base, control.db holds the address to re-clone it from. Sync/push STATUS
// deliberately stays in the repo's own remotes row — it describes the local
// replica and is meaningless after a rehydrate.
//
// Origins shares the Registry's *sql.DB handle rather than opening its own, so
// the foreign key into repos(uid) is enforced on the same connection.
package repos

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/rs/zerolog/log"

	"knomit/internal/store"
)

const originsSchema = `
CREATE TABLE IF NOT EXISTS repo_origins (
    repo_uid    TEXT PRIMARY KEY REFERENCES repos(uid) ON DELETE CASCADE,
    url         TEXT NOT NULL,
    branch      TEXT NOT NULL,
    auth_method TEXT NOT NULL DEFAULT '',
    auth_token  TEXT NOT NULL DEFAULT ''
);
`

// OriginsSchemaSQL exposes the repo_origins DDL to `knomit migrate-registry`
// for the same reason RegistrySchemaSQL is exported: the tool builds the table
// inside its own transaction, and must not carry a hand-copied duplicate of
// this schema.
const OriginsSchemaSQL = originsSchema

// Origin is a repo's remote connection: where it syncs from, which branch is
// the consensus upstream, and how to authenticate. AuthToken is PLAINTEXT in
// this struct and encrypted at rest.
type Origin struct {
	URL        string
	Branch     string
	AuthMethod string
	AuthToken  string
}

// Origins persists per-repo remote connection config in control.db.
type Origins struct {
	db    *sql.DB
	crypt *store.Crypt
}

// OpenOrigins returns an accessor over db (the Registry's handle, already
// migrated by migrate.Control). A nil crypt disables credential storage: Set
// refuses any non-empty AuthToken rather than writing a secret in the clear.
func OpenOrigins(db *sql.DB, crypt *store.Crypt) (*Origins, error) {
	return &Origins{db: db, crypt: crypt}, nil
}

// Get returns the origin for uid, or nil when the repo has none. A repo with no
// origin is an ordinary state, not an error — the same contract
// store.GetRemote has always had.
func (o *Origins) Get(uid string) (*Origin, error) {
	var org Origin
	err := o.db.QueryRow(
		`SELECT url, branch, auth_method, auth_token FROM repo_origins WHERE repo_uid = ?`, uid,
	).Scan(&org.URL, &org.Branch, &org.AuthMethod, &org.AuthToken)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("origins get: %w", err)
	}
	if o.crypt != nil && org.AuthToken != "" {
		dec, decErr := o.crypt.Decrypt(org.AuthToken)
		if decErr != nil {
			// Cannot distinguish "legacy plaintext" from "ciphertext we can no
			// longer decrypt" (rotated key, corruption) without a schema flag.
			// Log loudly and use as-is, mirroring GetRemote: a silent failure
			// resurfaces as a confusing 401 from the remote.
			log.Warn().Err(decErr).Str("repo_uid", uid).
				Msg("origins: token decrypt failed; using stored value as plaintext")
		} else {
			org.AuthToken = dec
		}
	}
	return &org, nil
}

// Set upserts the full origin record for uid, encrypting a non-empty
// AuthToken. Without a Crypt it REFUSES to store a credential — credentials are
// never written in plaintext.
func (o *Origins) Set(uid string, org Origin) error {
	if uid == "" {
		return fmt.Errorf("origins set: uid required")
	}
	if org.Branch == "" {
		org.Branch = "main"
	}
	stored := org.AuthToken
	if org.AuthToken != "" {
		if o.crypt == nil {
			return fmt.Errorf("refusing to store credential for repo %q: encryption unavailable (agent key unreadable); credentials are never stored in plaintext", uid)
		}
		enc, err := o.crypt.Encrypt(org.AuthToken)
		if err != nil {
			return fmt.Errorf("origins encrypt token: %w", err)
		}
		stored = enc
	}
	_, err := o.db.Exec(
		`INSERT INTO repo_origins (repo_uid, url, branch, auth_method, auth_token)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(repo_uid) DO UPDATE SET
		     url = excluded.url, branch = excluded.branch,
		     auth_method = excluded.auth_method, auth_token = excluded.auth_token`,
		uid, org.URL, org.Branch, org.AuthMethod, stored,
	)
	if err != nil {
		return fmt.Errorf("origins set: %w", err)
	}
	return nil
}

// SetBranch changes the consensus upstream branch WITHOUT touching stored auth.
// It is the control.db half of an upstream change: the caller
// (SetOriginUpstream) rewrites the git fetch refspec FIRST and only calls this
// on success, so the stored branch and the refspec can never be left
// permanently inconsistent.
func (o *Origins) SetBranch(uid, branch string) error {
	if branch == "" {
		branch = "main"
	}
	res, err := o.db.Exec(`UPDATE repo_origins SET branch = ? WHERE repo_uid = ?`, branch, uid)
	if err != nil {
		return fmt.Errorf("origins set branch: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("origins set branch: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("origins set branch: no origin configured for %q", uid)
	}
	return nil
}

// Delete removes a repo's origin. Deleting an absent origin is not an error.
func (o *Origins) Delete(uid string) error {
	if _, err := o.db.Exec(`DELETE FROM repo_origins WHERE repo_uid = ?`, uid); err != nil {
		return fmt.Errorf("origins delete: %w", err)
	}
	return nil
}

// ActiveRepoWithURL returns the NAME of the active repo whose origin URL equals
// url, or "" if none. It replaces the old scan that opened every registered
// repo to ask the same question.
//
// This is a cheap PREFLIGHT, not the real uniqueness guard: a mirror of the
// same repository has a different URL and would pass here. Identity uniqueness
// is Registry.RecordRepoID's job.
func (o *Origins) ActiveRepoWithURL(url string) (string, error) {
	var name string
	err := o.db.QueryRow(
		`SELECT r.name FROM repos r
		   JOIN repo_origins o ON o.repo_uid = r.uid
		  WHERE r.state = 'active' AND o.url = ?
		  LIMIT 1`, url).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("origins active repo with url: %w", err)
	}
	return name, nil
}
