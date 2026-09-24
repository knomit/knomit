package knomitapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// bearerTransport sends `Authorization: Bearer` on every request to a host
// `kb login` holds credentials for, and on a 401 invalid_token refreshes
// ONCE and replays the request. Hosts without credentials get no header.
//
// The credentials are read from disk per request rather than cached: several
// kb processes share the file, and one of them may have rotated the pair.
//
// Over the local listener the server ignores the header (it knows the caller
// from the kernel); only the OAuth listener judges it.
type bearerTransport struct{ base http.RoundTripper }

func withBearer(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &bearerTransport{base: base}
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	creds, err := LoadCredentials(req.URL)
	if err != nil {
		return t.base.RoundTrip(req)
	}
	resp, err := t.base.RoundTrip(withToken(req, creds.AccessToken, req.Body))
	if err != nil || resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "invalid_token") {
		return resp, err
	}
	// A body that cannot be re-read cannot be replayed; the caller gets the 401.
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		return resp, nil
	}
	fresh, rerr := refreshShared(req.Context(), req.URL, creds.AccessToken)
	if rerr != nil {
		log.Warn().Err(rerr).Str("host", req.URL.Host).Msg("bridge: token refresh failed; run `kb login` again")
		return resp, nil
	}
	body := req.Body
	if req.GetBody != nil {
		if body, err = req.GetBody(); err != nil {
			return resp, nil
		}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return t.base.RoundTrip(withToken(req, fresh.AccessToken, body))
}

func withToken(req *http.Request, token string, body io.ReadCloser) *http.Request {
	r := req.Clone(req.Context())
	r.Body = body
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

// refreshShared refreshes u's credentials under the cross-process lock. If
// the pair on disk is no longer the one this process sent — another kb
// already rotated it — that pair is used as it is: presenting the old
// refresh token would make the server revoke the whole family.
func refreshShared(ctx context.Context, u *url.URL, sentAccess string) (*Credentials, error) {
	var out *Credentials
	err := withCredentialsLock(ctx, u, func() error {
		cur, err := LoadCredentials(u)
		if err != nil {
			return err
		}
		if cur.AccessToken != sentAccess {
			out = cur
			return nil
		}
		fresh, err := refreshWith(ctx, &http.Client{Timeout: 30 * time.Second}, cur)
		if err != nil {
			return err
		}
		if err := SaveCredentials(u, fresh); err != nil {
			return err
		}
		out = fresh
		return nil
	})
	return out, err
}

// tokenResponse is RFC 6749 §5.1 (and §5.2 for errors).
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

// refreshWith exchanges cur's refresh token. It names the same resource, so
// the rotated token has the same audience.
func refreshWith(ctx context.Context, c *http.Client, cur *Credentials) (*Credentials, error) {
	tr, err := postToken(ctx, c, cur.TokenEndpoint, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {cur.RefreshToken},
		"client_id":     {cur.ClientID},
		"resource":      {cur.Resource},
	})
	if err != nil {
		return nil, err
	}
	next := *cur
	next.AccessToken, next.RefreshToken, next.Scope = tr.AccessToken, tr.RefreshToken, tr.Scope
	next.Expiry = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return &next, nil
}

func postToken(ctx context.Context, c *http.Client, endpoint string, form url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var tr tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&tr); err != nil {
		return nil, fmt.Errorf("token endpoint: HTTP %d: %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token endpoint: %s: %s", tr.Error, tr.Description)
	}
	if tr.AccessToken == "" || tr.RefreshToken == "" {
		return nil, errors.New("token endpoint: response lacks a token")
	}
	return &tr, nil
}
