// Secrets: per-instance stored credentials that repo origins link to. A tenant
// of <home>/control.db sharing the Registry's handle and the Manager's one
// Crypt, the same one that encrypts repo_origins.auth_token.
//
// Values are write-only. Nothing here returns a value except Reveal, which the
// git-auth resolver calls at operation time. Metadata (name, type, hosts,
// expiry, fingerprint, link count) is readable without the Crypt.
//
// Unlike Origins.Get, a value that does not decrypt is an ERROR
// (ErrSecretUndecryptable), never a lenient fallback: a linked repo whose
// secret cannot be read must fail visibly, not send ciphertext as a password.
package control

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"knomit/internal/engine/store/model"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"
)

// Secret types. Each fits one transport: token and basic for http(s)
// remotes, ssh_key for ssh remotes.
const (
	SecretTypeToken  = "token"
	SecretTypeBasic  = "basic"
	SecretTypeSSHKey = "ssh_key"
)

// ExpiringWithin is how close to its expiry a secret reads as "expiring".
const ExpiringWithin = 14 * 24 * time.Hour

// Expiry statuses, computed server-side so the API and UI agree.
const (
	ExpiryNone     = "none"
	ExpiryOK       = "ok"
	ExpiryExpiring = "expiring"
	ExpiryExpired  = "expired"
)

const maxSecretNameLen = 128

var (
	ErrSecretNotFound      = errors.New("secret not found")
	ErrSecretNameTaken     = errors.New("a secret with this name already exists")
	ErrSecretInUse         = errors.New("secret is linked to repositories")
	ErrSecretHostInUse     = errors.New("host is used by repositories linked to this secret")
	ErrSecretInvalid       = errors.New("invalid secret")
	ErrSecretUndecryptable = errors.New("cannot decrypt: the instance key changed since it was stored")
	// ErrEncryptionUnavailable: the instance key is unreadable, so no secret
	// can be written or read. Metadata reads still work.
	ErrEncryptionUnavailable = errors.New("secret storage is unavailable: the instance key cannot be read")
)

const redacted = "[redacted]"

// SecretValue is PLAINTEXT. Every way of printing or encoding it yields
// "[redacted]", so a value in a struct that reaches a log line or a JSON body
// does not leak. Read the fields directly, and only to authenticate.
type SecretValue struct {
	Token      string // token
	Username   string // basic
	Password   string // basic
	PrivateKey string // ssh_key, OpenSSH or PEM
	Passphrase string // ssh_key, optional
}

func (SecretValue) String() string               { return redacted }
func (SecretValue) GoString() string             { return redacted }
func (SecretValue) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(redacted)) }
func (SecretValue) MarshalJSON() ([]byte, error) { return json.Marshal(redacted) }

// SecretInput is what Create takes.
type SecretInput struct {
	Name      string
	Type      string
	Hosts     []string
	Value     SecretValue
	ExpiresAt *time.Time
}

// SecretMeta is everything about a secret except its value.
type SecretMeta struct {
	ID          string
	Name        string
	Type        string
	Hosts       []string
	ExpiresAt   *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Fingerprint string // ssh_key only: SHA256 fingerprint of the public key
	LinkedRepos int
}

// LinkedRepo is a repo whose origin links to a secret.
type LinkedRepo struct {
	UID   string
	Name  string
	State string
	URL   string
}

// InUseError refuses a delete while repos link to the secret.
type InUseError struct{ Repos []LinkedRepo }

func (e *InUseError) Error() string {
	return fmt.Sprintf("%s: %s", ErrSecretInUse, strings.Join(repoNames(e.Repos), ", "))
}
func (e *InUseError) Is(target error) bool { return target == ErrSecretInUse }

// HostInUseError refuses a host-list change that would strand linked repos.
type HostInUseError struct {
	Hosts []string
	Repos []LinkedRepo
}

func (e *HostInUseError) Error() string {
	return fmt.Sprintf("%s: %s used by %s", ErrSecretHostInUse,
		strings.Join(e.Hosts, ", "), strings.Join(repoNames(e.Repos), ", "))
}
func (e *HostInUseError) Is(target error) bool { return target == ErrSecretHostInUse }

func repoNames(rs []LinkedRepo) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Name
	}
	return out
}

// Secrets persists stored credentials in control.db.
type Secrets struct {
	db    *sql.DB
	crypt *model.Crypt
}

// OpenSecrets returns an accessor over db, which migrate.Control has already
// brought up (see OpenOrigins). A nil crypt refuses every value write and
// read with ErrEncryptionUnavailable.
func OpenSecrets(db *sql.DB, crypt *model.Crypt) *Secrets {
	return &Secrets{db: db, crypt: crypt}
}

// CanStoreValues reports whether values can be written and read (the
// instance key is readable). Metadata works either way.
func (s *Secrets) CanStoreValues() bool { return s.crypt != nil }

// Create validates and stores a new secret.
func (s *Secrets) Create(in SecretInput, now time.Time) (SecretMeta, error) {
	if s.crypt == nil {
		return SecretMeta{}, ErrEncryptionUnavailable
	}
	name, err := validateName(in.Name)
	if err != nil {
		return SecretMeta{}, err
	}
	hosts, err := NormalizeHosts(in.Hosts)
	if err != nil {
		return SecretMeta{}, err
	}
	stored, err := s.sealValue(in.Type, in.Value)
	if err != nil {
		return SecretMeta{}, err
	}
	hostsJSON, _ := json.Marshal(hosts)
	id := uuid.NewString()
	_, err = s.db.Exec(
		`INSERT INTO secrets (id, name, type, hosts, value_enc, passphrase_enc, fingerprint, expires_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, name, in.Type, string(hostsJSON), stored.value, stored.passphrase, stored.fingerprint,
		unixOrNil(in.ExpiresAt), now.Unix(), now.Unix())
	if err != nil {
		return SecretMeta{}, mapWriteErr("secrets create", err)
	}
	return s.Get(id)
}

const metaSelect = `SELECT s.id, s.name, s.type, s.hosts, s.fingerprint, s.expires_at, s.created_at, s.updated_at,
       (SELECT COUNT(*) FROM repo_origin_secrets l WHERE l.secret_id = s.id)
  FROM secrets s`

type rowScanner interface{ Scan(dest ...any) error }

func scanMeta(row rowScanner) (SecretMeta, error) {
	var m SecretMeta
	var hosts string
	var exp sql.NullInt64
	var created, updated int64
	if err := row.Scan(&m.ID, &m.Name, &m.Type, &hosts, &m.Fingerprint, &exp, &created, &updated, &m.LinkedRepos); err != nil {
		return SecretMeta{}, err
	}
	if err := json.Unmarshal([]byte(hosts), &m.Hosts); err != nil {
		return SecretMeta{}, fmt.Errorf("secret %q: hosts column: %w", m.Name, err)
	}
	if exp.Valid {
		t := time.Unix(exp.Int64, 0).UTC()
		m.ExpiresAt = &t
	}
	m.CreatedAt = time.Unix(created, 0).UTC()
	m.UpdatedAt = time.Unix(updated, 0).UTC()
	return m, nil
}

// List returns every secret's metadata, ordered by name.
func (s *Secrets) List() ([]SecretMeta, error) {
	rows, err := s.db.Query(metaSelect + ` ORDER BY s.name`)
	if err != nil {
		return nil, fmt.Errorf("secrets list: %w", err)
	}
	defer rows.Close()
	var out []SecretMeta
	for rows.Next() {
		m, err := scanMeta(rows)
		if err != nil {
			return nil, fmt.Errorf("secrets list: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Get returns one secret's metadata, or ErrSecretNotFound.
func (s *Secrets) Get(id string) (SecretMeta, error) {
	m, err := scanMeta(s.db.QueryRow(metaSelect+` WHERE s.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return SecretMeta{}, ErrSecretNotFound
	}
	if err != nil {
		return SecretMeta{}, fmt.Errorf("secrets get: %w", err)
	}
	return m, nil
}

// Rename changes the friendly name. Links follow the ID, so they are untouched.
func (s *Secrets) Rename(id, name string, now time.Time) error {
	return s.Update(id, SecretUpdate{Name: &name}, now)
}

// SetExpiry sets (or, with nil, clears) the advisory expiry date.
func (s *Secrets) SetExpiry(id string, at *time.Time, now time.Time) error {
	return s.Update(id, SecretUpdate{SetExpiry: true, ExpiresAt: at}, now)
}

// ReplaceValue stores a new value, validated against the secret's stored type.
// It does not wake linked repos: Manager.ReplaceSecretValue does that.
func (s *Secrets) ReplaceValue(id string, v SecretValue, now time.Time) error {
	if s.crypt == nil {
		return ErrEncryptionUnavailable
	}
	meta, err := s.Get(id)
	if err != nil {
		return err
	}
	stored, err := s.sealValue(meta.Type, v)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(
		`UPDATE secrets SET value_enc = ?, passphrase_enc = ?, fingerprint = ?, updated_at = ? WHERE id = ?`,
		stored.value, stored.passphrase, stored.fingerprint, now.Unix(), id)
	if err != nil {
		return fmt.Errorf("secrets replace value: %w", err)
	}
	return requireSecretRow(res)
}

// SetHosts replaces the allowed host list (normalised, deduplicated, at least
// one). It refuses, writing nothing, when a linked repo's remote host is
// allowed by the current list and would not be by the new one: the error is a
// *HostInUseError naming the removed hosts that repo used and the repos.
func (s *Secrets) SetHosts(id string, hosts []string, now time.Time) error {
	return s.Update(id, SecretUpdate{Hosts: &hosts}, now)
}

// SecretUpdate is the metadata Update changes; a nil field (or SetExpiry
// false) is left as stored.
type SecretUpdate struct {
	Name      *string
	Hosts     *[]string
	SetExpiry bool
	ExpiresAt *time.Time // with SetExpiry: nil clears the expiry
}

// Update changes a secret's name, hosts and expiry in ONE transaction: every
// refusal (invalid input, a host in use as SetHosts describes, a taken name)
// writes nothing at all.
func (s *Secrets) Update(id string, u SecretUpdate, now time.Time) error {
	var name string
	if u.Name != nil {
		var err error
		if name, err = validateName(*u.Name); err != nil {
			return err
		}
	}
	var norm []string
	if u.Hosts != nil {
		var err error
		if norm, err = NormalizeHosts(*u.Hosts); err != nil {
			return err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("secrets update: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	var oldJSON string
	err = tx.QueryRow(`SELECT hosts FROM secrets WHERE id = ?`, id).Scan(&oldJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSecretNotFound
	}
	if err != nil {
		return fmt.Errorf("secrets update: %w", err)
	}
	if u.Hosts != nil {
		if err := checkHostsKeepLinks(tx, id, oldJSON, norm); err != nil {
			return err
		}
		hostsJSON, _ := json.Marshal(norm)
		if _, err := tx.Exec(`UPDATE secrets SET hosts = ? WHERE id = ?`, string(hostsJSON), id); err != nil {
			return fmt.Errorf("secrets update hosts: %w", err)
		}
	}
	if u.Name != nil {
		if _, err := tx.Exec(`UPDATE secrets SET name = ? WHERE id = ?`, name, id); err != nil {
			return mapWriteErr("secrets rename", err)
		}
	}
	if u.SetExpiry {
		if _, err := tx.Exec(`UPDATE secrets SET expires_at = ? WHERE id = ?`, unixOrNil(u.ExpiresAt), id); err != nil {
			return fmt.Errorf("secrets update expiry: %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE secrets SET updated_at = ? WHERE id = ?`, now.Unix(), id); err != nil {
		return fmt.Errorf("secrets update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("secrets update: commit: %w", err)
	}
	return nil
}

// checkHostsKeepLinks refuses a new host list that would stop a linked repo
// matching (see SetHosts).
func checkHostsKeepLinks(tx *sql.Tx, id, oldJSON string, norm []string) error {
	var old []string
	if err := json.Unmarshal([]byte(oldJSON), &old); err != nil {
		return fmt.Errorf("secrets set hosts: hosts column: %w", err)
	}
	linked, err := linkedRepos(tx, id)
	if err != nil {
		return err
	}
	var stranded []LinkedRepo
	usedRemoved := map[string]bool{}
	for _, r := range linked {
		h, herr := RemoteHost(r.URL)
		if herr != nil || !HostAllowed(old, h) || HostAllowed(norm, h) {
			continue
		}
		stranded = append(stranded, r)
		for _, o := range old {
			if hostMatches(o, h) {
				usedRemoved[o] = true
			}
		}
	}
	if len(stranded) == 0 {
		return nil
	}
	e := &HostInUseError{Repos: stranded}
	for h := range usedRemoved {
		e.Hosts = append(e.Hosts, h)
	}
	sort.Strings(e.Hosts)
	return e
}

// Delete removes a secret. While any repo links to it the answer is an
// *InUseError naming those repos (the FK RESTRICT backs this up).
func (s *Secrets) Delete(id string) error {
	if _, err := s.Get(id); err != nil {
		return err
	}
	linked, err := s.LinkedRepos(id)
	if err != nil {
		return err
	}
	if len(linked) > 0 {
		return &InUseError{Repos: linked}
	}
	if _, err := s.db.Exec(`DELETE FROM secrets WHERE id = ?`, id); err != nil {
		if strings.Contains(err.Error(), "FOREIGN KEY") {
			linked, _ = s.LinkedRepos(id)
			return &InUseError{Repos: linked}
		}
		return fmt.Errorf("secrets delete: %w", err)
	}
	return nil
}

type queryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// LinkedRepos lists the repos whose origin links to the secret, by name.
func (s *Secrets) LinkedRepos(id string) ([]LinkedRepo, error) {
	return linkedRepos(s.db, id)
}

func linkedRepos(q queryer, id string) ([]LinkedRepo, error) {
	rows, err := q.Query(
		`SELECT r.uid, r.name, r.state, o.url
		   FROM repo_origin_secrets l
		   JOIN repos r ON r.uid = l.repo_uid
		   JOIN repo_origins o ON o.repo_uid = l.repo_uid
		  WHERE l.secret_id = ?
		  ORDER BY r.name`, id)
	if err != nil {
		return nil, fmt.Errorf("secrets linked repos: %w", err)
	}
	defer rows.Close()
	var out []LinkedRepo
	for rows.Next() {
		var r LinkedRepo
		if err := rows.Scan(&r.UID, &r.Name, &r.State, &r.URL); err != nil {
			return nil, fmt.Errorf("secrets linked repos: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Reveal returns a secret's metadata and plaintext value. A value that does
// not decrypt is ErrSecretUndecryptable, naming the secret; it is never
// returned as-is.
func (s *Secrets) Reveal(id string) (SecretMeta, SecretValue, error) {
	if s.crypt == nil {
		return SecretMeta{}, SecretValue{}, ErrEncryptionUnavailable
	}
	meta, err := s.Get(id)
	if err != nil {
		return SecretMeta{}, SecretValue{}, err
	}
	var enc, ppEnc string
	if err := s.db.QueryRow(`SELECT value_enc, passphrase_enc FROM secrets WHERE id = ?`, id).Scan(&enc, &ppEnc); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SecretMeta{}, SecretValue{}, ErrSecretNotFound
		}
		return SecretMeta{}, SecretValue{}, fmt.Errorf("secrets reveal: %w", err)
	}
	plain, err := s.crypt.Decrypt(enc)
	if err != nil {
		return SecretMeta{}, SecretValue{}, fmt.Errorf("stored secret %q: %w", meta.Name, ErrSecretUndecryptable)
	}
	var v SecretValue
	switch meta.Type {
	case SecretTypeToken:
		v.Token = plain
	case SecretTypeBasic:
		user, pass, _ := strings.Cut(plain, ":")
		v.Username, v.Password = user, pass
	case SecretTypeSSHKey:
		v.PrivateKey = plain
		if ppEnc != "" {
			pp, err := s.crypt.Decrypt(ppEnc)
			if err != nil {
				return SecretMeta{}, SecretValue{}, fmt.Errorf("stored secret %q: %w", meta.Name, ErrSecretUndecryptable)
			}
			v.Passphrase = pp
		}
	default:
		return SecretMeta{}, SecretValue{}, fmt.Errorf("stored secret %q: unknown type %q", meta.Name, meta.Type)
	}
	return meta, v, nil
}

// ExpiryStatus classifies an expiry date: nil is "none", at or before now is
// "expired", within ExpiringWithin of now is "expiring", otherwise "ok".
func ExpiryStatus(at *time.Time, now time.Time) string {
	switch {
	case at == nil:
		return ExpiryNone
	case !at.After(now):
		return ExpiryExpired
	case at.Sub(now) <= ExpiringWithin:
		return ExpiryExpiring
	default:
		return ExpiryOK
	}
}

// NormalizeHost lowercases a host entry and checks it is a bare host with an
// optional :port: no scheme, path or userinfo.
func NormalizeHost(h string) (string, error) {
	h = strings.ToLower(strings.TrimSpace(h))
	if h == "" {
		return "", fmt.Errorf("%w: hosts: an entry is empty", ErrSecretInvalid)
	}
	if strings.ContainsAny(h, "/@?# \t") {
		return "", fmt.Errorf("%w: hosts: %q is not a bare host (write it like github.com or git.example.com:2222)", ErrSecretInvalid, h)
	}
	host, port := h, ""
	if strings.Contains(h, ":") {
		var err error
		host, port, err = net.SplitHostPort(h)
		if err != nil {
			return "", fmt.Errorf("%w: hosts: %q is not a bare host", ErrSecretInvalid, h)
		}
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("%w: hosts: %q has an invalid port", ErrSecretInvalid, h)
		}
	}
	if host == "" {
		return "", fmt.Errorf("%w: hosts: %q has no host name", ErrSecretInvalid, h)
	}
	return h, nil
}

// RemoteHost extracts the lowercased host (host:port when a port is given)
// from a remote URL: an http(s) or ssh:// URL, or scp-like user@host:path.
// A local path or file:// URL has no host and is an error.
func RemoteHost(remote string) (string, error) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return "", errors.New("remote url is empty")
	}
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil {
			return "", fmt.Errorf("remote url: %w", err)
		}
		if u.Scheme == "file" || u.Host == "" {
			return "", fmt.Errorf("remote url %q has no host", u.Redacted())
		}
		return strings.ToLower(u.Host), nil
	}
	// scp-like: [user@]host:path, with no slash before the colon.
	colon := strings.Index(remote, ":")
	if colon <= 0 || strings.Contains(remote[:colon], "/") {
		return "", errors.New("remote url has no host (a local path)")
	}
	host := remote[:colon]
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	if host == "" {
		return "", errors.New("remote url has no host")
	}
	return strings.ToLower(host), nil
}

// HostAllowed reports whether remoteHost (as RemoteHost returns it) is on the
// list: an entry equal to it, or a bare-host entry equal to its host name (a
// bare host allows every port, a host:port entry only that port).
func HostAllowed(hosts []string, remoteHost string) bool {
	for _, h := range hosts {
		if hostMatches(h, remoteHost) {
			return true
		}
	}
	return false
}

func hostMatches(entry, remoteHost string) bool {
	if entry == remoteHost {
		return true
	}
	if strings.Contains(entry, ":") {
		return false
	}
	name, _, err := net.SplitHostPort(remoteHost)
	return err == nil && name == entry
}

// ValidateSecretName checks a name as Create and Rename do.
func ValidateSecretName(name string) error {
	_, err := validateName(name)
	return err
}

// ValidateSecretValue checks v for typ as Create and ReplaceValue do,
// without storing anything (SSH keys are parsed).
func ValidateSecretValue(typ string, v SecretValue) error {
	_, err := checkValue(typ, v)
	return err
}

func validateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > maxSecretNameLen {
		return "", fmt.Errorf("%w: name must be 1 to %d characters", ErrSecretInvalid, maxSecretNameLen)
	}
	return name, nil
}

// NormalizeHosts normalises each entry (NormalizeHost), deduplicates, and
// requires at least one.
func NormalizeHosts(in []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, h := range in {
		n, err := NormalizeHost(h)
		if err != nil {
			return nil, err
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: hosts: at least one host is required", ErrSecretInvalid)
	}
	return out, nil
}

type sealed struct{ value, passphrase, fingerprint string }

// sealValue validates v for typ and encrypts it. Errors never quote the value.
func (s *Secrets) sealValue(typ string, v SecretValue) (sealed, error) {
	c, err := checkValue(typ, v)
	if err != nil {
		return sealed{}, err
	}
	enc, err := s.crypt.Encrypt(c.value)
	if err != nil {
		return sealed{}, fmt.Errorf("secrets encrypt: %w", err)
	}
	out := sealed{value: enc, fingerprint: c.fingerprint}
	if c.passphrase != "" {
		if out.passphrase, err = s.crypt.Encrypt(c.passphrase); err != nil {
			return sealed{}, fmt.Errorf("secrets encrypt: %w", err)
		}
	}
	return out, nil
}

// checkValue validates v for typ and returns the PLAINTEXT to store (in the
// sealed shape, unencrypted). Errors never quote the value.
func checkValue(typ string, v SecretValue) (sealed, error) {
	var plain, pp, fp string
	switch typ {
	case SecretTypeToken:
		if v.Token == "" {
			return sealed{}, fmt.Errorf("%w: value: a token is required", ErrSecretInvalid)
		}
		plain = v.Token
	case SecretTypeBasic:
		if v.Username == "" || strings.Contains(v.Username, ":") {
			return sealed{}, fmt.Errorf("%w: username: required, and must not contain ':'", ErrSecretInvalid)
		}
		if v.Password == "" {
			return sealed{}, fmt.Errorf("%w: password: required", ErrSecretInvalid)
		}
		plain = v.Username + ":" + v.Password
	case SecretTypeSSHKey:
		signer, err := parseSSHKey(v.PrivateKey, v.Passphrase)
		if err != nil {
			return sealed{}, err
		}
		plain, pp, fp = v.PrivateKey, v.Passphrase, ssh.FingerprintSHA256(signer.PublicKey())
	default:
		return sealed{}, fmt.Errorf("%w: type: must be %s, %s or %s", ErrSecretInvalid, SecretTypeToken, SecretTypeBasic, SecretTypeSSHKey)
	}
	return sealed{value: plain, passphrase: pp, fingerprint: fp}, nil
}

func parseSSHKey(key, passphrase string) (ssh.Signer, error) {
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("%w: private_key: required", ErrSecretInvalid)
	}
	if passphrase == "" {
		signer, err := ssh.ParsePrivateKey([]byte(key))
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return nil, fmt.Errorf("%w: passphrase: the key is encrypted, a passphrase is required", ErrSecretInvalid)
		}
		if err != nil {
			return nil, fmt.Errorf("%w: private_key: not an OpenSSH or PEM private key", ErrSecretInvalid)
		}
		return signer, nil
	}
	signer, err := ssh.ParsePrivateKeyWithPassphrase([]byte(key), []byte(passphrase))
	if err != nil {
		if _, plainErr := ssh.ParsePrivateKey([]byte(key)); plainErr == nil {
			return nil, fmt.Errorf("%w: passphrase: the key is not encrypted, leave the passphrase empty", ErrSecretInvalid)
		}
		return nil, fmt.Errorf("%w: passphrase: wrong passphrase, or not an OpenSSH or PEM private key", ErrSecretInvalid)
	}
	return signer, nil
}

func unixOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Unix()
}

func mapWriteErr(op string, err error) error {
	if strings.Contains(err.Error(), "UNIQUE constraint failed: secrets.name") {
		return ErrSecretNameTaken
	}
	return fmt.Errorf("%s: %w", op, err)
}

func requireSecretRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrSecretNotFound
	}
	return nil
}
