package oauth

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/pki"
)

// Consent path 2 (F19 phase 3b, Task 3): the operator approves or denies a
// waiting request from their OWN machine by signing a statement with the
// fleet master key; anyone may carry the signed statement to the instance's
// public POST /oauth/approve. The instance verifies it against the fleet
// root public key it holds (<[tls].dir>/root.crt). The master private key
// never touches the instance.

// Verbs, and the tag each statement's first line carries. The verb is IN the
// signed bytes, so an approve signature never verifies as a deny, nor the
// reverse.
const (
	VerbApprove = "approve"
	VerbDeny    = "deny"
)

// Named refusals.
var (
	ErrMalformedStatement = errors.New("oauth: malformed approval statement")
	ErrBadSignature       = errors.New("oauth: the statement's signature does not verify against this instance's fleet root")
	ErrWrongInstance      = errors.New("oauth: the statement names another instance")
	ErrStatementExpired   = errors.New("oauth: the statement is outside its validity window")
	ErrStatementMismatch  = errors.New("oauth: the statement was signed over a different request than the one waiting under that id")
	ErrReplayed           = errors.New("oauth: this statement was already applied")
	ErrReplayTableFull    = errors.New("oauth: too many recent signed statements; try again shortly")
	ErrNoFleetRoot        = errors.New("oauth: signed approval is unavailable: this instance holds no fleet root (not enrolled with `knomit identity install`)")
)

// maxStatementLifetime bounds how far ahead a statement may expire: a signed
// approval is meant to be delivered now, for a request that itself expires
// in pendingTTL.
const maxStatementLifetime = pendingTTL

// signedApprovalSkew tolerates the operator's machine and the instance
// disagreeing about the time. Same value and the same reasoning as pki's
// certificate backdating: a defensive bound, not a measured property of any
// fleet. Window: now − skew < expires ≤ now + maxStatementLifetime + skew.
const signedApprovalSkew = 5 * time.Minute

// replayTableMax bounds the replay table. Inserting needs a VALID signature,
// so this is not a defence against unauthenticated growth; it keeps a leaked
// master key from also being a memory lever.
const replayTableMax = 1024

var (
	fingerprintRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
	digestRE      = fingerprintRE
)

// ApprovalStatement is what the master key signs. Bytes is its ONE encoding:
//
//	knomit-oauth-approve/v1          knomit-oauth-deny/v1
//	<instance fingerprint>           <instance fingerprint>
//	<pending id>                     <pending id>
//	<request digest>                 <request digest>
//	<subject>                        <expires, unix seconds>
//	<scopes, comma-joined, sorted>
//	<expires, unix seconds>
//
// newline-separated, with no trailing newline. Every field is validated
// before it is joined, so no field can contain a separator: the fingerprint
// is 64 lowercase hex (pki.Fingerprint), the id a pending id's shape, the
// digest RequestDigest's 64 hex, the subject subjectRE, each scope a
// grantable permission name (never admin) with no repeats, and expires a
// positive decimal (strconv gives no sign and no leading zero).
//
// The first line is the domain separation (S2): the same key signs X.509
// certificates and CRLs, which are DER and begin with 0x30; every non-X.509
// use of the master key signs a message whose first line is a
// knomit-<purpose>/v<n> tag, and every verifier requires its exact tag.
type ApprovalStatement struct {
	Verb     string   `json:"verb"`
	Instance string   `json:"instance"`
	ID       string   `json:"id"`
	Digest   string   `json:"digest"`
	Subject  string   `json:"subject,omitempty"`
	Scopes   []string `json:"scopes,omitempty"`
	Expires  int64    `json:"expires"`
}

// SignedStatement is the JSON a signer prints and /oauth/approve accepts.
// It carries the FIELDS; the verifier rebuilds the bytes from them and
// verifies over what it rebuilt — it never verifies a client-supplied byte
// string and then parses it. Issuer is where to deliver it; it is not
// signed (the instance fingerprint is what binds the statement to one
// instance).
type SignedStatement struct {
	ApprovalStatement
	Signature string `json:"signature"`
	Issuer    string `json:"issuer,omitempty"`
}

// Bytes validates s and returns its canonical encoding.
func (s ApprovalStatement) Bytes() ([]byte, error) {
	bad := func(why string) ([]byte, error) { return nil, fmt.Errorf("%w: %s", ErrMalformedStatement, why) }
	if !fingerprintRE.MatchString(s.Instance) {
		return bad("instance must be 64 lowercase hex")
	}
	if !pendingIDRE.MatchString(s.ID) {
		return bad("id is not a pending request id")
	}
	if !digestRE.MatchString(s.Digest) {
		return bad("digest must be 64 lowercase hex")
	}
	if s.Expires <= 0 {
		return bad("expires must be a positive unix time")
	}
	exp := strconv.FormatInt(s.Expires, 10)
	lines := []string{"", s.Instance, s.ID, s.Digest}
	switch s.Verb {
	case VerbApprove:
		if !subjectRE.MatchString(s.Subject) {
			return bad(ErrInvalidSubject.Error())
		}
		if len(s.Scopes) == 0 {
			return bad("an approval grants at least one scope")
		}
		scopes := slices.Clone(s.Scopes)
		slices.Sort(scopes)
		if len(slices.Compact(slices.Clone(scopes))) != len(scopes) {
			return bad("a scope is repeated")
		}
		for _, sc := range scopes {
			if !slices.Contains(ScopesSupported, sc) {
				return bad("scope " + strconv.Quote(sc) + " is not grantable")
			}
		}
		lines[0] = "knomit-oauth-approve/v1"
		lines = append(lines, s.Subject, strings.Join(scopes, ","), exp)
	case VerbDeny:
		if s.Subject != "" || len(s.Scopes) != 0 {
			return bad("a denial carries no subject or scopes")
		}
		lines[0] = "knomit-oauth-deny/v1"
		lines = append(lines, exp)
	default:
		return bad("verb must be approve or deny")
	}
	return []byte(strings.Join(lines, "\n")), nil
}

// SignStatement signs s with the master key. It runs on the operator's
// machine (`knomit oauth approve --sign`), never on an instance.
func SignStatement(s ApprovalStatement, key ed25519.PrivateKey) (SignedStatement, error) {
	if s.Verb == VerbApprove {
		s.Scopes = slices.Clone(s.Scopes)
		slices.Sort(s.Scopes)
	}
	b, err := s.Bytes()
	if err != nil {
		return SignedStatement{}, err
	}
	return SignedStatement{ApprovalStatement: s, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, b))}, nil
}

// FleetRootLabel names a fleet root in decided_by / granted_by: the first 8
// hex of its pki.Fingerprint. "master:<label>" is deliberately not a
// principal spelling (auth.ParsePrincipal refuses it), so it can never
// match a grants row.
func FleetRootLabel(pub ed25519.PublicKey) string { return pki.Fingerprint(pub)[:8] }

// ApplySigned verifies a signed statement and applies its decision. The
// order is: rebuild the bytes (malformed), verify the signature against the
// fleet root (no root / bad signature), then this instance, the validity
// window, the replay table, and finally the pending row — which is the real
// replay guard: Approve and Deny decide only an undecided row, so a
// statement replayed after a restart emptied the table gets ErrNotPending.
func (i *Issuer) ApplySigned(ctx context.Context, ss SignedStatement) (Pending, error) {
	b, err := ss.Bytes()
	if err != nil {
		return Pending{}, err
	}
	if i.fleetRoot == nil {
		return Pending{}, ErrNoFleetRoot
	}
	root, err := i.fleetRoot()
	if errors.Is(err, fs.ErrNotExist) {
		return Pending{}, ErrNoFleetRoot
	}
	if err != nil {
		return Pending{}, fmt.Errorf("oauth: load fleet root: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(ss.Signature)
	if err != nil || !ed25519.Verify(root, b, sig) {
		return Pending{}, ErrBadSignature
	}
	if ss.Instance != i.instanceFP {
		return Pending{}, ErrWrongInstance
	}
	now := i.store.now()
	exp := time.Unix(ss.Expires, 0)
	if !exp.After(now.Add(-signedApprovalSkew)) || exp.After(now.Add(maxStatementLifetime+signedApprovalSkew)) {
		return Pending{}, ErrStatementExpired
	}
	sum := sha256.Sum256(b)
	if err := i.replays.add(hex.EncodeToString(sum[:]), exp.Add(signedApprovalSkew), now); err != nil {
		return Pending{}, err
	}
	p, err := i.store.GetPending(ctx, ss.ID)
	if err != nil {
		return Pending{}, err
	}
	if RequestDigest(p) != ss.Digest {
		return Pending{}, ErrStatementMismatch
	}
	by := "master:" + FleetRootLabel(root)
	if ss.Verb == VerbDeny {
		if err := i.Deny(ctx, ss.ID, by); err != nil {
			return Pending{}, err
		}
		return i.store.GetPending(ctx, ss.ID)
	}
	return i.Approve(ctx, ss.ID, ss.Subject, ss.Scopes, by)
}

// signedApprove serves POST /oauth/approve.
func (i *Issuer) signedApprove(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	w.Header().Set("Cache-Control", "no-store")
	var ss SignedStatement
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&ss); err != nil {
		http.Error(w, ErrMalformedStatement.Error()+": body must be the JSON `knomit oauth approve --sign` prints", http.StatusBadRequest)
		return
	}
	p, err := i.ApplySigned(r.Context(), ss)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, ErrMalformedStatement), errors.Is(err, ErrInvalidScope), errors.Is(err, ErrInvalidSubject):
			status = http.StatusBadRequest
		case errors.Is(err, ErrBadSignature), errors.Is(err, ErrWrongInstance), errors.Is(err, ErrStatementExpired):
			status = http.StatusForbidden
		case errors.Is(err, ErrUnknownPending):
			status = http.StatusNotFound
		case errors.Is(err, ErrReplayed), errors.Is(err, ErrStatementMismatch), errors.Is(err, ErrNotPending), errors.Is(err, ErrExpired):
			status = http.StatusConflict
		case errors.Is(err, ErrNoFleetRoot), errors.Is(err, ErrReplayTableFull):
			status = http.StatusServiceUnavailable
		}
		log.Info().Err(err).Str("id", ss.ID).Str("verb", ss.Verb).Msg("oauth: signed statement refused")
		msg := err.Error()
		if status == http.StatusInternalServerError {
			msg = "internal error"
		}
		http.Error(w, msg, status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"id": p.ID, "decision": p.Decision})
}

// replayTable remembers applied statements (by the hash of their bytes)
// until they could no longer verify anyway.
type replayTable struct {
	mu   sync.Mutex
	max  int
	seen map[string]time.Time // hash -> forget after
}

func newReplayTable(max int) *replayTable {
	return &replayTable{max: max, seen: map[string]time.Time{}}
}

func (t *replayTable) add(key string, forgetAfter, now time.Time) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, until := range t.seen {
		if now.After(until) {
			delete(t.seen, k)
		}
	}
	if _, ok := t.seen[key]; ok {
		return ErrReplayed
	}
	if len(t.seen) >= t.max {
		return ErrReplayTableFull
	}
	t.seen[key] = forgetAfter
	return nil
}

func (t *replayTable) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.seen)
}
