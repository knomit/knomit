package cmd

// The local API client: every `knomit` verb that talks to the running server
// (fleet, oauth, auth) sends ONE request over the local authenticated
// listener (the unix socket, or the named pipe on Windows) and nowhere else.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"syscall"
	"time"

	"knomit/internal/foundation/auth"
	"knomit/internal/foundation/config"
)

// localAPIBase is the URL the local API client addresses. The host is never
// resolved: every dial goes to the local listener.
const localAPIBase = "http://knomit.local/api/v1"

// localAPIClient talks to the server on the local listener at path and
// nowhere else — no TCP fallback: over TCP the server would not know who is
// asking, and refuses these endpoints. auth.DialLocal also refuses a listener
// owned by another account (auth.ErrForeignListener), so a process squatting
// the pipe name can neither show this CLI a consent screen nor take its
// approvals.
func localAPIClient(path string) *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return auth.DialLocal(ctx, path, 5*time.Second)
			},
		},
	}
}

func withLocalAPI(fn func(*http.Client) error) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.Socket == "" {
		return errors.New("no local listener is configured for this home; this command works only over it")
	}
	return fn(localAPIClient(cfg.Socket))
}

// localError is a refused local call: the problem document's detail, plus the
// code and the leave_out list a refused fleet drop carries. It formats as the
// quoted detail, as it always did.
type localError struct {
	Detail   string `json:"detail"`
	Code     string `json:"code"`
	LeaveOut []struct {
		Commit  string `json:"commit"`
		Agent   string `json:"agent"`
		Author  string `json:"author"`
		Subject string `json:"subject"`
	} `json:"leave_out"`
}

// Error quotes the detail: it can echo requester-supplied text, quoted like
// every other field this command prints.
func (e *localError) Error() string { return fmt.Sprintf("%q", e.Detail) }

// localCall does one request and turns a problem document into an error
// that carries its detail.
func localCall(ctx context.Context, c *http.Client, method, path string, in, out any) error {
	var rd io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, localAPIBase+path, rd)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		// No socket file (or pipe), or a file nobody accepts on: the same
		// advice either way.
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return errors.New("no knomit server is listening on this home's local listener; start `knomit serve`")
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var prob localError
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&prob)
		if prob.Detail == "" {
			prob.Detail = resp.Status
		}
		return &prob
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
