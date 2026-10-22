// Package idp is consent path 3's provider side (F19 phase 3c): knomit as
// an OAuth CLIENT of one external identity provider, used for one purpose —
// to learn which provider account is at the browser. knomit issues its own
// tokens for its own audience; nothing from the provider outlives the call
// that learns the identity.
package idp

import (
	"context"
	"errors"
)

var (
	// ErrProviderRefused: the provider said no, or answered something that
	// is not a usable identity. Its own words never travel further.
	ErrProviderRefused = errors.New("idp: the identity provider refused or answered unusably")
	// ErrNotAllowed: a real identity that is not on the allow list.
	ErrNotAllowed = errors.New("idp: that identity is not allowed to approve requests here")
	// ErrBadState: the callback's state is unknown, used, expired, or not
	// bound to this browser.
	ErrBadState = errors.New("idp: the sign-in is not valid for this browser or has expired")
	// ErrUnknownLogin: LookupLogin found no such account.
	ErrUnknownLogin = errors.New("idp: no such account at the provider")
)

// Subject is who the provider says is at the browser. ID is knomit's
// subject for them, "<provider>-<stable numeric id>", and the only thing
// allow lists and grants use; Login is for display and may change.
type Subject struct {
	ID    string
	Login string
}

// Provider is one external identity provider. Exchange, lookup and
// revocation are ONE call, Identify, so the provider's access token never
// leaves the implementation.
type Provider interface {
	// Name is the provider's name, which prefixes its subjects ("github").
	Name() string
	// AuthorizeURL is where the browser goes to sign in: state and the
	// PKCE S256 challenge are knomit's, redirectURI is knomit's callback.
	AuthorizeURL(state, codeChallenge, redirectURI string) string
	// Identify exchanges the code (with the client secret and the PKCE
	// verifier, in the request body), asks who the token belongs to once,
	// revokes the token best-effort, and returns the subject.
	Identify(ctx context.Context, code, codeVerifier, redirectURI string) (Subject, error)
}
