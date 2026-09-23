package knomitapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"knomit/internal/config"
)

// LoginOptions tunes `kb login`. The zero value prints the authorization URL
// and refuses anything that would need a question answered.
type LoginOptions struct {
	// Scopes requested; nil means "read write". The operator's approval sets
	// the real ceiling.
	Scopes []string
	// OpenBrowser opens the authorization URL; nil only prints it.
	OpenBrowser func(url string) error
	// Out receives the instructions (the URL, what to run on the instance).
	Out io.Writer
	// Confirm asks the user a yes/no question; nil answers no.
	Confirm func(question string) bool
	// Timeout bounds the wait for approval; zero means ten minutes, the
	// lifetime of the request on the instance.
	Timeout time.Duration
}

// Login runs the authorization-code flow with PKCE against the knomit
// instance at base as the pre-registered client "kb", and saves the pair
// (CredentialsPath). Discovery follows RFC 9728 then RFC 8414 with the path
// insertion both use, and refuses before opening anything unless the
// instance advertises S256 and the RFC 9207 iss parameter, its metadata
// names itself, and its issuer is at base's origin (or the user agrees).
// The callback must carry the state kb sent and the iss it discovered.
func Login(ctx context.Context, base string, o LoginOptions) (*Credentials, error) {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Timeout == 0 {
		o.Timeout = 10 * time.Minute
	}
	scopes := o.Scopes
	if scopes == nil {
		scopes = []string{"read", "write"}
	}
	bu, err := url.Parse(strings.TrimSuffix(base, "/"))
	if err != nil || (bu.Scheme != "https" && bu.Scheme != "http") || bu.Host == "" {
		return nil, fmt.Errorf("kb login: %q is not an http(s) URL", base)
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	md, err := discover(ctx, hc, bu, o.Confirm)
	if err != nil {
		return nil, err
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("kb login: loopback listener: %w", err)
	}
	redirect := "http://" + ln.Addr().String() + "/callback"
	verifier, challenge, state := secret(), "", secret()
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])

	results := make(chan callbackResult, 1)
	cbSrv := &http.Server{Handler: callbackHandler(state, md.Issuer, results), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = cbSrv.Serve(ln) }()
	defer cbSrv.Close()

	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {"kb"},
		"redirect_uri":          {redirect},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
		"scope":                 {strings.Join(scopes, " ")},
		"resource":              {md.Resource},
	}
	authURL := md.AuthorizationEndpoint + "?" + q.Encode()
	fmt.Fprintf(o.Out, "Authorize kb at:\n\n  %s\n\nThe request then waits for approval ON THE INSTANCE:\n  knomit oauth pending\n  knomit oauth approve <id> --as <name>\n\n", authURL)
	if o.OpenBrowser != nil {
		if err := o.OpenBrowser(authURL); err != nil {
			fmt.Fprintf(o.Out, "(could not open a browser: %v; open the URL yourself)\n", err)
		}
	}

	wait, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	var res callbackResult
	select {
	case res = <-results:
	case <-wait.Done():
		return nil, fmt.Errorf("kb login: no answer from the browser: %w", wait.Err())
	}
	if res.err != nil {
		return nil, res.err
	}

	tr, err := postToken(ctx, hc, md.TokenEndpoint, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {res.code},
		"redirect_uri":  {redirect},
		"client_id":     {"kb"},
		"code_verifier": {verifier},
		"resource":      {md.Resource},
	})
	if err != nil {
		return nil, fmt.Errorf("kb login: %w", err)
	}
	creds := &Credentials{
		Issuer: md.Issuer, TokenEndpoint: md.TokenEndpoint, RevocationEndpoint: md.RevocationEndpoint,
		ClientID: "kb", Resource: md.Resource, Scope: tr.Scope,
		AccessToken: tr.AccessToken, RefreshToken: tr.RefreshToken,
		Expiry: time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second),
	}
	if err := SaveCredentials(bu, creds); err != nil {
		return nil, err
	}
	return creds, nil
}

// Logout revokes the saved pair at the instance (best effort) and deletes it.
func Logout(ctx context.Context, base string) error {
	bu, err := url.Parse(strings.TrimSuffix(base, "/"))
	if err != nil {
		return err
	}
	creds, err := LoadCredentials(bu)
	if err != nil {
		return DeleteCredentials(bu) // nothing (readable) to revoke
	}
	if creds.RevocationEndpoint != "" {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, creds.RevocationEndpoint,
			strings.NewReader(url.Values{"token": {creds.RefreshToken}, "client_id": {creds.ClientID}}.Encode()))
		if rerr == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if resp, derr := (&http.Client{Timeout: 10 * time.Second}).Do(req); derr == nil {
				resp.Body.Close()
			}
		}
	}
	return DeleteCredentials(bu)
}

type metadata struct {
	Resource              string
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RevocationEndpoint    string   `json:"revocation_endpoint"`
	Methods               []string `json:"code_challenge_methods_supported"`
	IssParam              bool     `json:"authorization_response_iss_parameter_supported"`
}

func discover(ctx context.Context, hc *http.Client, bu *url.URL, confirm func(string) bool) (*metadata, error) {
	origin := bu.Scheme + "://" + bu.Host
	var prm struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	if err := getJSON(ctx, hc, origin+"/.well-known/oauth-protected-resource"+bu.EscapedPath(), &prm); err != nil {
		return nil, fmt.Errorf("kb login: protected-resource metadata: %w", err)
	}
	// RFC 9728 §3.3: the document must be about the URL we asked about.
	if prm.Resource != bu.String() {
		return nil, fmt.Errorf("kb login: the instance describes resource %q, not %q", prm.Resource, bu.String())
	}
	if len(prm.AuthorizationServers) == 0 {
		return nil, errors.New("kb login: the instance names no authorization server")
	}
	issuer := prm.AuthorizationServers[0]
	norm, err := config.NormalizeIssuer(issuer)
	if err != nil || norm != issuer {
		return nil, fmt.Errorf("kb login: authorization server %q is not an acceptable issuer", issuer)
	}
	iu, _ := url.Parse(issuer)
	if iu.Scheme+"://"+iu.Host != origin {
		q := fmt.Sprintf("%s says its tokens come from %s. Trust that issuer?", origin, issuer)
		if confirm == nil || !confirm(q) {
			return nil, fmt.Errorf("kb login: issuer %s is not at %s; refusing", issuer, origin)
		}
	}
	var md metadata
	if err := getJSON(ctx, hc, iu.Scheme+"://"+iu.Host+"/.well-known/oauth-authorization-server"+iu.EscapedPath(), &md); err != nil {
		return nil, fmt.Errorf("kb login: authorization-server metadata: %w", err)
	}
	switch {
	case md.Issuer != issuer:
		return nil, fmt.Errorf("kb login: metadata names issuer %q, fetched for %q", md.Issuer, issuer)
	case !slices.Contains(md.Methods, "S256"):
		return nil, errors.New("kb login: the instance does not advertise PKCE S256; refusing")
	case !md.IssParam:
		return nil, errors.New("kb login: the instance does not advertise the iss response parameter (RFC 9207); refusing")
	case md.AuthorizationEndpoint == "" || md.TokenEndpoint == "":
		return nil, errors.New("kb login: metadata lacks an endpoint")
	}
	md.Resource = prm.Resource
	return &md, nil
}

func getJSON(ctx context.Context, hc *http.Client, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(v)
}

type callbackResult struct {
	code string
	err  error
}

// callbackHandler receives the one redirect. It answers the browser either
// way and reports the first result; state and iss must match exactly.
func callbackHandler(state, issuer string, out chan<- callbackResult) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		var res callbackResult
		switch {
		case q.Get("state") != state:
			res.err = errors.New("kb login: the callback's state does not match; refusing")
		case q.Get("iss") != issuer:
			res.err = fmt.Errorf("kb login: the callback's iss %q is not %q (possible mix-up); refusing", q.Get("iss"), issuer)
		case q.Get("error") != "":
			res.err = fmt.Errorf("kb login: %s: %s", q.Get("error"), q.Get("error_description"))
		case q.Get("code") == "":
			res.err = errors.New("kb login: the callback carries no code")
		default:
			res.code = q.Get("code")
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if res.err != nil {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintln(w, res.err)
		} else {
			fmt.Fprintln(w, "kb is authorized. You can close this window.")
		}
		select {
		case out <- res:
		default:
		}
	})
}

func secret() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
