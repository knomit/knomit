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
	"encoding/json"
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
	return immediateTx(o.db, "origins set", func(tx dbTx) error { return o.setTx(tx, uid, org) })
}

// ErrOriginLinked refuses a write that would drop a stored-secret link the
// caller did not see: the stored origin is linked, the new origin is not, and
// the caller did not pass replaceLink (it did not go through a gate that may
// unlink).
var ErrOriginLinked = errors.New("this repository's origin is linked to a stored secret, and this change would unlink it: unlink it first with PUT /repos/{repo}/origin/credential (\"secret_id\": null), then retry")

// SetAttached is Set for an attach: in the SAME transaction it refuses
// (ErrOriginLinked, nothing written) when the stored origin is linked to a
// stored secret and org is not, unless replaceLink. A spec built from a read
// taken before a concurrent link therefore never drops that link.
func (o *Origins) SetAttached(uid string, org Origin, replaceLink bool) error {
	if uid == "" {
		return fmt.Errorf("origins set: uid required")
	}
	if org.Branch == "" {
		return fmt.Errorf("origins set: %w", model.ErrNoConsensusBranch)
	}
	return immediateTx(o.db, "origins set", func(tx dbTx) error {
		if err := refuseUnseenUnlink(tx, uid, org, replaceLink); err != nil {
			return err
		}
		return o.setTx(tx, uid, org)
	})
}

// refuseUnseenUnlink is SetAttached's and Replace's guard inside tx.
func refuseUnseenUnlink(tx dbTx, uid string, org Origin, replaceLink bool) error {
	if replaceLink || org.SecretID != "" {
		return nil
	}
	var linked string
	err := tx.QueryRow(`SELECT secret_id FROM repo_origin_secrets WHERE repo_uid = ?`, uid).Scan(&linked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("origins set: read secret link: %w", err)
	}
	return ErrOriginLinked
}

// setTx is Set's writes inside tx.
func (o *Origins) setTx(tx dbTx, uid string, org Origin) error {
	if org.Branch == "" {
		return fmt.Errorf("origins set: %w", model.ErrNoConsensusBranch)
	}
	if org.SecretID != "" && org.AuthToken != "" {
		return fmt.Errorf("origins set: %w", errLinkWithToken)
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

// Replace is SetAttached that also returns the row it replaced (nil when
// there was none), read in the SAME transaction as the write. A caller that
// may have to undo its write (applySwap) restores exactly what it replaced,
// not a copy it read earlier that a credential change may have overtaken.
// Like SetAttached it refuses to drop an unseen secret link (ErrOriginLinked)
// unless replaceLink.
func (o *Origins) Replace(uid string, org Origin, replaceLink bool) (*Origin, error) {
	if uid == "" {
		return nil, fmt.Errorf("origins set: uid required")
	}
	var prev *Origin
	err := immediateTx(o.db, "origins replace", func(tx dbTx) error {
		var gerr error
		if prev, gerr = o.get(tx, uid); gerr != nil {
			return gerr
		}
		if err := refuseUnseenUnlink(tx, uid, org, replaceLink); err != nil {
			return err
		}
		return o.setTx(tx, uid, org)
	})
	if err != nil {
		return nil, err
	}
	return prev, nil
}

// Restore undoes a Replace that wrote `written` over `prev`, in one
// transaction. With prev nil the row is deleted. Otherwise prev is written
// back whole, with ONE exception: when prev and written have the SAME URL and
// the current row's credential (method, token, secret link) no longer equals
// written's, a credential change landed after the Replace and is kept rather
// than reverted. Across different URLs prev is always restored whole: a
// credential set after the Replace was vetted against written's URL, and
// carrying it to prev's URL would send it to another host.
func (o *Origins) Restore(uid string, written Origin, prev *Origin) error {
	return immediateTx(o.db, "origins restore", func(tx dbTx) error {
		if prev == nil {
			if _, err := tx.Exec(`DELETE FROM repo_origins WHERE repo_uid = ?`, uid); err != nil {
				return fmt.Errorf("origins restore: %w", err)
			}
			if _, err := tx.Exec(`DELETE FROM repo_subscriptions WHERE repo_uid = ?`, uid); err != nil {
				return fmt.Errorf("origins restore: %w", err)
			}
			return nil
		}
		cur, err := o.get(tx, uid)
		if err != nil {
			return err
		}
		target := *prev
		if cur != nil && prev.URL == written.URL && cur.URL == written.URL && !sameCredential(*cur, written) {
			target.AuthMethod, target.AuthToken, target.SecretID = cur.AuthMethod, cur.AuthToken, cur.SecretID
		}
		return o.setTx(tx, uid, target)
	})
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
// link inside tx. Writing a link re-checks, in the same transaction, that the
// secret exists and that its hosts list allows the host of the url stored in
// repo_origins for uid (written earlier in tx): ErrSecretNotFound or
// ErrSecretHostNotAllowed, naming the secret, and nothing is written. Every
// write that links runs under immediateTx, as does every host-list change, so
// a host removed concurrently either sees this link (and refuses) or this
// link sees the new list (and refuses).
func setLink(tx dbTx, uid, secretID string) error {
	var err error
	if secretID != "" {
		if err := checkLinkHost(tx, uid, secretID); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT OR REPLACE INTO repo_origin_secrets (repo_uid, secret_id) VALUES (?, ?)`, uid, secretID)
	} else {
		_, err = tx.Exec(`DELETE FROM repo_origin_secrets WHERE repo_uid = ?`, uid)
	}
	if err != nil {
		return fmt.Errorf("origins set secret link: %w", err)
	}
	return nil
}

// checkLinkHost is setLink's host check: the secret's hosts list must allow
// the host of uid's stored origin url.
func checkLinkHost(tx dbTx, uid, secretID string) error {
	var name, hostsJSON string
	err := tx.QueryRow(`SELECT name, hosts FROM secrets WHERE id = ?`, secretID).Scan(&name, &hostsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSecretNotFound
	}
	if err != nil {
		return fmt.Errorf("origins set secret link: %w", err)
	}
	var hosts []string
	if err := json.Unmarshal([]byte(hostsJSON), &hosts); err != nil {
		return fmt.Errorf("stored secret %q: hosts column: %w", name, err)
	}
	var url string
	if err := tx.QueryRow(`SELECT url FROM repo_origins WHERE repo_uid = ?`, uid).Scan(&url); err != nil {
		return fmt.Errorf("origins set secret link: %w", err)
	}
	host, herr := RemoteHost(url)
	if herr != nil {
		return fmt.Errorf("stored secret %q: %w: %v", name, ErrSecretHostNotAllowed, herr)
	}
	if !HostAllowed(hosts, host) {
		return fmt.Errorf("stored secret %q: %w: %s", name, ErrSecretHostNotAllowed, host)
	}
	return nil
}

// SetCredential changes ONLY the credential of uid's origin: auth_method,
// auth_token and the secret link, in one transaction, and only while the
// stored url is still url (the caller's vetted url). Otherwise it is
// ErrOriginChanged and nothing is written. URL, branch and Mode are untouched,
// which is what lets a credential change skip the attach lifecycle.
func (o *Origins) SetCredential(uid, url, authMethod, authToken, secretID string) error {
	if secretID != "" && authToken != "" {
		return fmt.Errorf("origins set credential: %w", errLinkWithToken)
	}
	stored, err := o.sealToken(uid, authToken)
	if err != nil {
		return err
	}
	return immediateTx(o.db, "origins set credential", func(tx dbTx) error {
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
		return setLink(tx, uid, secretID)
	})
}

// ErrOriginChanged is SetBranch's refusal when no origin with the url the
// caller vetted is stored any more: another write re-pointed or removed the
// origin in between, so the branch change would apply to a remote nobody
// checked. Nothing is written.
var ErrOriginChanged = errors.New("the origin changed")

// errLinkWithToken: an origin linked to a stored secret keeps no token of
// its own (a kept token would be a silent fallback if the link were dropped).
var errLinkWithToken = errors.New("an origin linked to a stored secret carries no auth token of its own")

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

// Delete removes a repo's origin, its subscription row and its secret link.
// Deleting an absent origin is not an error. It drops a link unconditionally;
// a detach that must not drop a link it did not see uses DeleteAttached.
func (o *Origins) Delete(uid string) error {
	return o.DeleteAttached(uid, true)
}

// DeleteAttached is Delete for a detach: in the SAME transaction as the
// delete it refuses (ErrOriginLinked, nothing deleted) when the origin is
// linked to a stored secret, unless replaceLink. A detach decided before a
// concurrent link therefore never drops that link. Pass replaceLink only
// after the admin gate (or on the local-only fleet routes).
func (o *Origins) DeleteAttached(uid string, replaceLink bool) error {
	return immediateTx(o.db, "origins delete", func(tx dbTx) error {
		if err := refuseUnseenUnlink(tx, uid, Origin{}, replaceLink); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM repo_origin_secrets WHERE repo_uid = ?`, uid); err != nil {
			return fmt.Errorf("origins delete: %w", err)
		}
		if _, err := tx.Exec(`DELETE FROM repo_origins WHERE repo_uid = ?`, uid); err != nil {
			return fmt.Errorf("origins delete: %w", err)
		}
		// Keep the presence table honest: no subscription row without an origin.
		if _, err := tx.Exec(`DELETE FROM repo_subscriptions WHERE repo_uid = ?`, uid); err != nil {
			return fmt.Errorf("origins delete: %w", err)
		}
		return nil
	})
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
