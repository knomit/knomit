package idp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"golang.org/x/net/http/httpproxy"

	"knomit/internal/config"
)

const (
	// maxBody caps every provider response. A larger body is refused, not
	// truncated into something that parses.
	maxBody = 64 << 10
	// callTimeout bounds each outbound call: DNS, connect, TLS, headers and
	// body under one deadline.
	callTimeout = 10 * time.Second
	// maxErrorCode caps the provider's error code before it is quoted into
	// an error or a log line.
	maxErrorCode = 64
)

// loginRE is GitHub's username alphabet (alphanumerics and single hyphens,
// at most 39), checked before a login is put in a request path.
var loginRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)

// GitHub is the GitHub OAuth-app provider. Its hosts are pinned in code
// (github.com for the browser and the token exchange, api.github.com for
// the user and revocation endpoints); nothing a requester sends can move
// them, so the CIMD fetcher's SSRF address checks do not apply here. Tests
// point it at a fake with SetEndpoints.
type GitHub struct {
	clientID string
	secret   config.IDPSecret
	web      string // https://github.com
	api      string // https://api.github.com
	proxy    func(*url.URL) (*url.URL, error)
	client   *http.Client
}

// NewGitHub builds the provider. The proxy is read from HTTPS_PROXY /
// NO_PROXY now (ruling W5: honoured for this pinned host only).
func NewGitHub(clientID string, secret config.IDPSecret) *GitHub {
	g := &GitHub{clientID: clientID, secret: secret, web: "https://github.com", api: "https://api.github.com",
		proxy: httpproxy.FromEnvironment().ProxyFunc()}
	g.client = &http.Client{
		Timeout:       callTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			Proxy:               g.ProxyFor,
			ForceAttemptHTTP2:   true,
			TLSHandshakeTimeout: callTimeout,
		},
	}
	return g
}

// SetEndpoints points the provider at another web and API base (tests).
func (g *GitHub) SetEndpoints(web, api string) {
	g.web, g.api = strings.TrimSuffix(web, "/"), strings.TrimSuffix(api, "/")
}

// ProxyFor is the transport's proxy decision for req.
func (g *GitHub) ProxyFor(req *http.Request) (*url.URL, error) { return g.proxy(req.URL) }

func (g *GitHub) Name() string { return "github" }

// AuthorizeURL asks for NO scope: /user returns the public profile, which
// carries the id, to a token with none. allow_signup=false: someone without
// an account cannot be on the allow list.
func (g *GitHub) AuthorizeURL(state, codeChallenge, redirectURI string) string {
	q := url.Values{
		"client_id":             {g.clientID},
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
		"allow_signup":          {"false"},
	}
	return g.web + "/login/oauth/authorize?" + q.Encode()
}

// Identify: exchange, one lookup, revoke, forget. The access token (and any
// refresh token GitHub sends unasked) lives only in this call's locals.
func (g *GitHub) Identify(ctx context.Context, code, codeVerifier, redirectURI string) (Subject, error) {
	token, err := g.exchange(ctx, code, codeVerifier, redirectURI)
	if err != nil {
		return Subject{}, err
	}
	defer g.revoke(token)
	return g.user(ctx, token)
}

func (g *GitHub) exchange(ctx context.Context, code, verifier, redirectURI string) (string, error) {
	// Everything secret goes in the BODY: a URL ends up in *url.Error
	// strings and in proxies' logs.
	form := url.Values{
		"client_id":     {g.clientID},
		"client_secret": {g.secret.Reveal()},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirectURI},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.web+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrProviderRefused, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var resp struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		// refresh_token, error_description and error_uri are deliberately
		// not decoded: never kept, never shown.
	}
	if err := g.doJSON(req, "token", &resp); err != nil {
		return "", err
	}
	// GitHub reports a refused code as HTTP 200 with an error field.
	if resp.Error != "" || resp.AccessToken == "" {
		code := resp.Error
		if len(code) > maxErrorCode {
			code = code[:maxErrorCode]
		}
		log.Info().Str("provider", "github").Str("error", strconv.Quote(code)).Msg("oauth idp: token exchange refused")
		return "", fmt.Errorf("%w: token exchange: %q", ErrProviderRefused, code)
	}
	return resp.AccessToken, nil
}

func (g *GitHub) user(ctx context.Context, token string) (Subject, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.api+"/user", nil)
	if err != nil {
		return Subject{}, fmt.Errorf("%w: %v", ErrProviderRefused, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return g.subjectFrom(req, "user")
}

// LookupLogin resolves a login to its stable id with the public, unauthenticated
// users endpoint — the `knomit oauth idp resolve` helper. The operator pastes
// the id; knomit never resolves logins at run time.
func (g *GitHub) LookupLogin(ctx context.Context, login string) (Subject, error) {
	if !loginRE.MatchString(login) {
		return Subject{}, fmt.Errorf("idp: %q is not a GitHub login", login)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.api+"/users/"+url.PathEscape(login), nil)
	if err != nil {
		return Subject{}, err
	}
	return g.subjectFrom(req, "users")
}

func (g *GitHub) subjectFrom(req *http.Request, what string) (Subject, error) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	var raw struct {
		ID    json.RawMessage `json:"id"`
		Login string          `json:"login"`
	}
	if err := g.doJSON(req, what, &raw); err != nil {
		return Subject{}, err
	}
	// The id must be a positive JSON integer: never a string, a float, or
	// absent, and never replaced by the login.
	id, err := strconv.ParseInt(string(raw.ID), 10, 64)
	if err != nil || id <= 0 {
		return Subject{}, fmt.Errorf("%w: %s answer has no numeric id", ErrProviderRefused, what)
	}
	return Subject{ID: "github-" + strconv.FormatInt(id, 10), Login: raw.Login}, nil
}

// doJSON performs req and decodes a JSON body of at most maxBody. Anything
// else — a redirect, a non-2xx status, another content type, an oversize or
// malformed body — is ErrProviderRefused, with the provider's bytes left out.
func (g *GitHub) doJSON(req *http.Request, what string, out any) error {
	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrProviderRefused, what, redactURLError(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return fmt.Errorf("%w: %s: reading the answer: %v", ErrProviderRefused, what, err)
	}
	if resp.StatusCode == http.StatusNotFound && what == "users" {
		return ErrUnknownLogin
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%w: %s answered HTTP %d", ErrProviderRefused, what, resp.StatusCode)
	}
	if len(body) > maxBody {
		return fmt.Errorf("%w: %s answer over %d bytes", ErrProviderRefused, what, maxBody)
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "application/json" {
		return fmt.Errorf("%w: %s answered %q, not JSON", ErrProviderRefused, what, mt)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%w: %s answer is not the expected JSON", ErrProviderRefused, what)
	}
	return nil
}

// revoke deletes the token at GitHub (basic auth with the app's id and
// secret), best-effort: the identity was already proven, so a failure is a
// WARN — without the token — and nothing more.
func (g *GitHub) revoke(token string) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"access_token": token})
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, g.api+"/applications/"+url.PathEscape(g.clientID)+"/token", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.SetBasicAuth(g.clientID, g.secret.Reveal())
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := g.client.Do(req)
	if err != nil {
		log.Warn().Str("provider", "github").Str("error", redactURLError(err).Error()).Msg("oauth idp: could not revoke the provider token after the lookup")
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		log.Warn().Str("provider", "github").Int("status", resp.StatusCode).Msg("oauth idp: could not revoke the provider token after the lookup")
	}
}

// redactURLError keeps a transport error's cause and drops its URL, which
// for the token exchange is harmless today but must stay so.
func redactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}
