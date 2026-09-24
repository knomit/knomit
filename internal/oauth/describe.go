package oauth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// requestDigestTag separates this digest from every other use of SHA-256
// over knomit data; the /v1 is bumped if the field list ever changes.
const requestDigestTag = "knomit-oauth-request/v1\n"

// RequestDigest is SHA-256 (64 lowercase hex) over what a pending request
// asks for, as the REQUESTER supplied it: client_id, client_name,
// redirect_uri, resource and the requested scopes (F19 phase 3b, R4). The
// operator signs it in a master-key approval, and the instance recomputes it
// from its OWN row, never from bytes a client sent, so a statement signed
// over a description a relay invented does not verify.
//
// The fields are encoded as one JSON array after the tag, so no choice of
// field contents can move a boundary between two of them. The requester's
// address and user agent are NOT in it: they describe the browser, and the
// operator does not sign them.
func RequestDigest(p Pending) string {
	scopes := p.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	enc, _ := json.Marshal([]any{p.ClientID, p.ClientName, p.RedirectURI, p.Resource, scopes})
	sum := sha256.Sum256(append([]byte(requestDigestTag), enc...))
	return hex.EncodeToString(sum[:])
}

// Description is the public view of a waiting request.
type Description struct {
	ID          string    `json:"id"`
	ClientID    string    `json:"client_id"`
	ClientName  string    `json:"client_name"`
	RedirectURI string    `json:"redirect_uri"`
	Resource    string    `json:"resource"`
	Scopes      []string  `json:"scopes"`
	ExpiresAt   time.Time `json:"expires_at"`
	Digest      string    `json:"digest"`
}

// describe serves GET /oauth/pending/{id}: the requester-supplied fields and
// their digest, for a request that is still waiting. It is public because
// the id is random and already known to the requester, so it discloses
// nothing new; what it adds is a way for an operator on ANOTHER machine to
// see what they are about to sign (`knomit oauth approve --sign`).
//
// Unknown, malformed, decided and expired ids are all 404, and nothing is
// retained for any of them.
func (i *Issuer) describe(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// A description changes (it vanishes on decision) and must never be
	// served from a cache to someone about to sign it.
	w.Header().Set("Cache-Control", "no-store")
	notFound := func() {
		noStore(w)
		http.Error(w, "no such waiting authorization request", http.StatusNotFound)
	}
	if !pendingIDRE.MatchString(id) {
		notFound()
		return
	}
	p, err := i.store.GetPending(r.Context(), id)
	if errors.Is(err, ErrUnknownPending) {
		notFound()
		return
	}
	if err != nil {
		noStore(w)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if p.Decision != "" || p.Expired(i.store.now()) {
		notFound()
		return
	}
	scopes := p.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	noStore(w)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Description{
		ID: p.ID, ClientID: p.ClientID, ClientName: p.ClientName, RedirectURI: p.RedirectURI,
		Resource: p.Resource, Scopes: scopes, ExpiresAt: p.ExpiresAt.UTC(), Digest: RequestDigest(p),
	})
}

// redirectHost is the host a description's redirect_uri points at, for
// displays that must show it first.
func redirectHost(uri string) string {
	if i := strings.Index(uri, "://"); i >= 0 {
		rest := uri[i+3:]
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			rest = rest[:j]
		}
		return rest
	}
	return uri
}
