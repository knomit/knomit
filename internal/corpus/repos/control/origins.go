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
package control

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"knomit/internal/engine/store/model"

	"github.com/rs/zerolog/log"
)

// Origin modes. Sync is the default and what every clone/initialize repo has.
// Subscribe means the repo follows its upstream read-only with no agent branch
// and never pushes — see Manager.initSubscribe.
const (
	OriginModeSync      = "sync"
	OriginModeSubscribe = "subscribe"
)

// Origin is a repo's remote connection: where it syncs from, which branch is
// the consensus upstream, and how to authenticate. AuthToken is PLAINTEXT in
// this struct and encrypted at rest.
type Origin struct {
	URL        string
	Branch     string
	AuthMethod string
	AuthToken  string
	// Mode is OriginModeSync or OriginModeSubscribe. Empty is read as sync.
	Mode string
	// SecretID is the stored secret this origin links to (a row in
	// repo_origin_secrets), or "" when it uses its own AuthToken or none.
	// A linked origin keeps AuthMethod as the transport and no AuthToken.
	SecretID string
}

// Origins persists per-repo remote connection config in control.db.
type Origins struct {
	db    *sql.DB
	crypt *model.Crypt
}

// OpenOrigins returns an accessor over db (the Registry's handle, already
// migrated by migrate.Control). A nil crypt disables credential storage: Set
// refuses any non-empty AuthToken rather than writing a secret in the clear.
//
// No error return, deliberately: this is a wrapper around a handle someone else
// opened and someone else migrated, so there is nothing here that can fail. It
// used to return one when it created repo_origins itself. A caller that sees no
// error must NOT read that as "the table is there" — that guarantee comes from
// ControlUp having run on db, which is why Manager.Start calls this only after
// the registry is up.
func OpenOrigins(db *sql.DB, crypt *model.Crypt) *Origins {
	return &Origins{db: db, crypt: crypt}
}

// Get returns the origin for uid, or nil when the repo has none. A repo with no
// origin is an ordinary state, not an error — the same contract
// store.GetRemote has always had.
func (o *Origins) Get(uid string) (*Origin, error) {
	return o.get(o.db, uid)
}

type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func (o *Origins) get(q rowQuerier, uid string) (*Origin, error) {
	var org Origin
	err := q.QueryRow(
		`SELECT o.url, o.branch, o.auth_method, o.auth_token,
		        CASE WHEN s.repo_uid IS NULL THEN ? ELSE ? END,
		        COALESCE(l.secret_id, '')
		   FROM repo_origins o
		   LEFT JOIN repo_subscriptions s ON s.repo_uid = o.repo_uid
		   LEFT JOIN repo_origin_secrets l ON l.repo_uid = o.repo_uid
		  WHERE o.repo_uid = ?`, OriginModeSync, OriginModeSubscribe, uid,
	).Scan(&org.URL, &org.Branch, &org.AuthMethod, &org.AuthToken, &org.Mode, &org.SecretID)
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
//
// It writes Mode and SecretID too: Set is a full replacement, not a patch. An
// Origin with an EMPTY Mode DELETES any subscription row for uid (empty means
// sync), and one with an EMPTY SecretID DELETES any secret link (empty means
// not linked). So a read-modify-write caller must carry BOTH Mode and
// SecretID through from Get: constructing a fresh Origin{URL, Branch,
// AuthMethod, AuthToken} to change a credential or a URL silently demotes a
// subscribed repo to sync and silently unlinks a linked one. There is no error
// and no log, because at this layer an explicit value and an unset one are the
// same request. To change only the credential use SetCredential. The mode
// half, the link half and the repo_origins upsert share one transaction, so a
// rejected Mode or an unknown SecretID leaves none of them behind.
func (o *Origins) Set(uid string, org Origin) error {
	if uid == "" {
		return fmt.Errorf("origins set: uid required")
	}
	// The branch is the repo's consensus branch, which the caller resolved (a
	// clone's adopted branch, a request, the repo's recorded branch). A row
	// without one would make every later sync reconcile against a name nobody
	// chose, so it is refused rather than filled in.
	if org.Branch == "" {
		return fmt.Errorf("origins set: %w", model.ErrNoConsensusBranch)
	}
	tx, err := o.db.Begin()
	if err != nil {
		return fmt.Errorf("origins set: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit
	if err := o.setTx(tx, uid, org); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("origins set: commit: %w", err)
	}
	return nil
}

// setTx is Set's writes inside tx.
func (o *Origins) setTx(tx *sql.Tx, uid string, org Origin) error {
	if org.Branch == "" {
		return fmt.Errorf("origins set: %w", model.ErrNoConsensusBranch)
	}
	stored, err := o.sealToken(uid, org.AuthToken)
	if err != nil {
		return err
	}
	_, err = tx.Exec(
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

	switch org.Mode {
	case OriginModeSubscribe:
		_, err = tx.Exec(`INSERT OR IGNORE INTO repo_subscriptions (repo_uid, created_at) VALUES (?, ?)`,
			uid, time.Now().UTC().Unix())
	case "", OriginModeSync:
		_, err = tx.Exec(`DELETE FROM repo_subscriptions WHERE repo_uid = ?`, uid)
	default:
		return fmt.Errorf("origins set: unknown mode %q", org.Mode)
	}
	if err != nil {
		return fmt.Errorf("origins set mode: %w", err)
	}
	return setLink(tx, uid, org.SecretID)
}

// Replace is Set that also returns the row it replaced (nil when there was
// none), read in the SAME transaction as the write. A caller that may have to
// undo its write (applySwap) restores exactly what it replaced, not a copy it
// read earlier that a credential change may have overtaken.
func (o *Origins) Replace(uid string, org Origin) (*Origin, error) {
	if uid == "" {
		return nil, fmt.Errorf("origins set: uid required")
	}
	tx, err := o.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("origins replace: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit
	prev, err := o.get(tx, uid)
	if err != nil {
		return nil, err
	}
	if err := o.setTx(tx, uid, org); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("origins replace: commit: %w", err)
	}
	return prev, nil
}

// Restore undoes a Replace that wrote `written` over `prev`, in one
// transaction. With prev nil the row is deleted. Otherwise prev is written
// back, EXCEPT its credential when the current row's credential (method,
// token, secret link) no longer equals written's: a credential change landed
// after the Replace, and it is kept rather than reverted.
func (o *Origins) Restore(uid string, written Origin, prev *Origin) error {
	tx, err := o.db.Begin()
	if err != nil {
		return fmt.Errorf("origins restore: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit
	if prev == nil {
		if _, err := tx.Exec(`DELETE FROM repo_origins WHERE repo_uid = ?`, uid); err != nil {
			return fmt.Errorf("origins restore: %w", err)
		}
		if _, err := tx.Exec(`DELETE FROM repo_subscriptions WHERE repo_uid = ?`, uid); err != nil {
			return fmt.Errorf("origins restore: %w", err)
		}
	} else {
		cur, err := o.get(tx, uid)
		if err != nil {
			return err
		}
		target := *prev
		if cur != nil && !sameCredential(*cur, written) {
			target.AuthMethod, target.AuthToken, target.SecretID = cur.AuthMethod, cur.AuthToken, cur.SecretID
		}
		if err := o.setTx(tx, uid, target); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("origins restore: commit: %w", err)
	}
	return nil
}

func sameCredential(a, b Origin) bool {
	return a.AuthMethod == b.AuthMethod && a.AuthToken == b.AuthToken && a.SecretID == b.SecretID
}

// sealToken encrypts a non-empty token, refusing without a Crypt.
func (o *Origins) sealToken(uid, token string) (string, error) {
	if token == "" {
		return "", nil
	}
	if o.crypt == nil {
		return "", fmt.Errorf("refusing to store credential for repo %q: encryption unavailable (agent key unreadable); credentials are never stored in plaintext", uid)
	}
	enc, err := o.crypt.Encrypt(token)
	if err != nil {
		return "", fmt.Errorf("origins encrypt token: %w", err)
	}
	return enc, nil
}

// setLink writes (secretID != "") or removes (secretID == "") uid's secret
// link inside tx. A secretID with no secrets row fails on the foreign key.
func setLink(tx *sql.Tx, uid, secretID string) error {
	var err error
	if secretID != "" {
		_, err = tx.Exec(`INSERT OR REPLACE INTO repo_origin_secrets (repo_uid, secret_id) VALUES (?, ?)`, uid, secretID)
	} else {
		_, err = tx.Exec(`DELETE FROM repo_origin_secrets WHERE repo_uid = ?`, uid)
	}
	if err != nil {
		return fmt.Errorf("origins set secret link: %w", err)
	}
	return nil
}

// SetCredential changes ONLY the credential of uid's origin: auth_method,
// auth_token and the secret link, in one transaction, and only while the
// stored url is still url (the caller's vetted url). Otherwise it is
// ErrOriginChanged and nothing is written. URL, branch and Mode are untouched,
// which is what lets a credential change skip the attach lifecycle.
func (o *Origins) SetCredential(uid, url, authMethod, authToken, secretID string) error {
	stored, err := o.sealToken(uid, authToken)
	if err != nil {
		return err
	}
	tx, err := o.db.Begin()
	if err != nil {
		return fmt.Errorf("origins set credential: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	res, err := tx.Exec(`UPDATE repo_origins SET auth_method = ?, auth_token = ? WHERE repo_uid = ? AND url = ?`,
		authMethod, stored, uid, url)
	if err != nil {
		return fmt.Errorf("origins set credential: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("origins set credential: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("origins set credential: %w: no origin with the checked url is configured for %q", ErrOriginChanged, uid)
	}
	if err := setLink(tx, uid, secretID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("origins set credential: commit: %w", err)
	}
	return nil
}

// ErrOriginChanged is SetBranch's refusal when no origin with the url the
// caller vetted is stored any more: another write re-pointed or removed the
// origin in between, so the branch change would apply to a remote nobody
// checked. Nothing is written.
var ErrOriginChanged = errors.New("the origin changed")

// SetBranch changes the consensus upstream branch WITHOUT touching stored auth,
// and only while the stored url is still url: the compare-and-set is the WHERE
// of the one UPDATE that makes the change, so an attach that re-points the
// origin between the caller's check and this write cannot have the branch
// applied to its new remote. It is the control.db half of an upstream change:
// the caller (setOriginUpstream) rewrites the git fetch refspec FIRST and only
// calls this on success, so the stored branch and the refspec can never be
// left permanently inconsistent.
func (o *Origins) SetBranch(uid, url, branch string) error {
	if branch == "" {
		return fmt.Errorf("origins set branch: %w", model.ErrNoConsensusBranch)
	}
	res, err := o.db.Exec(`UPDATE repo_origins SET branch = ? WHERE repo_uid = ? AND url = ?`, branch, uid, url)
	if err != nil {
		return fmt.Errorf("origins set branch: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("origins set branch: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("origins set branch: %w: no origin with the checked url is configured for %q", ErrOriginChanged, uid)
	}
	return nil
}

// Delete removes a repo's origin. Deleting an absent origin is not an error.
func (o *Origins) Delete(uid string) error {
	if _, err := o.db.Exec(`DELETE FROM repo_origins WHERE repo_uid = ?`, uid); err != nil {
		return fmt.Errorf("origins delete: %w", err)
	}
	// repo_origins deletion alone would leave a stale subscriptions row for a
	// repo that is refused origin deletion anyway (Task 9); delete it here too
	// to keep the presence table honest rather than relying on that refusal.
	if _, err := o.db.Exec(`DELETE FROM repo_subscriptions WHERE repo_uid = ?`, uid); err != nil {
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
