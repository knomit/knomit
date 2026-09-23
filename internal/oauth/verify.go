package oauth

import (
	"context"
	"fmt"

	"knomit/internal/auth"
)

// Verifier judges a bearer token on the OAuth listener: known, unexpired,
// unrevoked, and issued for a resource the request sits under. It answers
// the principal and the ceiling; what the principal may DO is still
// auth.Allowed's question (grants ∩ ceiling, auth.TokenGrants).
type Verifier struct {
	issuer string
	store  *Store
}

func NewVerifier(issuer string, store *Store) *Verifier {
	return &Verifier{issuer: issuer, store: store}
}

// Verify checks token for a request whose path, AS THE OAUTH LISTENER SEES
// IT, is path — already decoded and canonical (the caller refuses anything
// else). The request's canonical URL is issuer + path: a reverse proxy in
// front strips the issuer's own path before forwarding. It is in the token's
// audience when it equals the token's resource or extends it at a "/"
// boundary, so a token for one MCP endpoint does not work on its neighbour.
func (v *Verifier) Verify(ctx context.Context, token, path string) (auth.Principal, auth.Set, error) {
	fam, err := v.store.LookupAccess(ctx, token)
	if err != nil {
		return auth.Principal{}, nil, err
	}
	if !inAudience(fam.Resource, v.issuer+path) {
		return auth.Principal{}, nil, ErrWrongAudience
	}
	ceiling, err := auth.ParseSet(fam.Scopes)
	if err != nil {
		return auth.Principal{}, nil, fmt.Errorf("oauth: token ceiling: %w", err)
	}
	return TokenPrincipal(fam.Subject), ceiling, nil
}

// inAudience: url equals resource, or extends it at a "/" boundary.
func inAudience(resource, url string) bool {
	return url == resource || len(url) > len(resource) && url[:len(resource)] == resource && url[len(resource)] == '/'
}
