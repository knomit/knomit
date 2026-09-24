package cmd

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
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"knomit/internal/auth"
	"knomit/internal/config"
	"knomit/internal/oauth"
)

// oauthCmd is `knomit oauth`: the operator's side of consent path 1 (F19
// phase 3a). A client that ran /oauth/authorize is parked; these commands
// list, approve and deny it. They talk to the RUNNING server over the local
// authenticated listener only — the unix socket, or the named pipe on
// Windows — because the server accepts approvals from nobody else (the
// socket is the credential), and because an approval has to wake the
// browser's long-poll inside that server. `knomit grants`, by contrast,
// opens control.db directly: it has no one to wake.
func oauthCmd() *cobra.Command {
	c := &cobra.Command{
		Use:          "oauth",
		Short:        "List, approve and deny OAuth authorization requests waiting on this instance",
		SilenceUsage: true,
	}
	pending := &cobra.Command{
		Use:   "pending",
		Short: "List the authorization requests waiting for a decision",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withLocalAPI(func(c *http.Client) error { return oauthPending(cmd.Context(), c, cmd.OutOrStdout()) })
		},
	}
	var subject string
	var scopes []string
	var approveSign, denySign signOpts
	var signed, deliverTo string
	approve := &cobra.Command{
		Use:   "approve <id> --as <subject>",
		Short: "Approve a request; the token acts as host:<subject>@token",
		Long: "Approve a waiting request. --as names the subject the token acts as (its principal is\n" +
			"host:<subject>@token). --scopes sets the token's ceiling (default: what the client asked\n" +
			"for, limited to read and write; read if it asked for neither). The subject's grants are\n" +
			"written for the ceiling; `knomit grants` can narrow them later.\n\n" +
			"On the instance this talks to the local listener. From ANOTHER machine, with the fleet\n" +
			"master key: `approve <id> --sign <issuer> --dir <master> --instance <fingerprint> --as ...`\n" +
			"prints the request for you to read, then a signed statement; `approve --signed <file|->`\n" +
			"delivers it (so does one POST of that JSON to <issuer>/oauth/approve).",
		Args: func(cmd *cobra.Command, args []string) error {
			if signed != "" {
				return cobra.NoArgs(cmd, args)
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if signed != "" {
				return oauthDeliver(cmd, signed, deliverTo)
			}
			if subject == "" {
				return errors.New("--as <subject> is required")
			}
			var sc []string
			if cmd.Flags().Changed("scopes") {
				sc = scopes
				if sc == nil {
					sc = []string{}
				}
			}
			if approveSign.issuer != "" {
				o := approveSign
				o.verb, o.subject, o.scopes, o.scopesGiven = oauth.VerbApprove, subject, sc, cmd.Flags().Changed("scopes")
				return oauthSign(cmd, args[0], o)
			}
			return withLocalAPI(func(c *http.Client) error {
				return oauthApprove(cmd.Context(), c, cmd.OutOrStdout(), args[0], subject, sc)
			})
		},
	}
	approve.Flags().StringVar(&subject, "as", "", "the subject the token acts as (required)")
	approve.Flags().StringSliceVar(&scopes, "scopes", nil, "the token's ceiling, e.g. read,write (never admin)")
	approve.Flags().StringVar(&signed, "signed", "", "deliver a signed statement (a file, or - for stdin) to its issuer")
	approve.Flags().StringVar(&deliverTo, "issuer", "", "with --signed: deliver here instead of the issuer the statement names")
	signFlags(approve, &approveSign)
	deny := &cobra.Command{
		Use:   "deny <id>",
		Short: "Deny a request; the client is told access_denied",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if denySign.issuer != "" {
				o := denySign
				o.verb = oauth.VerbDeny
				return oauthSign(cmd, args[0], o)
			}
			return withLocalAPI(func(c *http.Client) error { return oauthDeny(cmd.Context(), c, cmd.OutOrStdout(), args[0]) })
		},
	}
	signFlags(deny, &denySign)
	c.AddCommand(pending, approve, deny)
	return c
}

// signFlags adds the master-key signing flags (consent path 2) to approve
// and deny.
func signFlags(c *cobra.Command, o *signOpts) {
	c.Flags().StringVar(&o.issuer, "sign", "", "sign with the fleet master key instead, for the instance at this issuer URL (run on the master's machine)")
	c.Flags().StringVar(&o.dir, "dir", "", "with --sign: the master directory (root.key, issued.jsonl)")
	c.Flags().StringVar(&o.instance, "instance", "", "with --sign: the instance's full fingerprint; it must be issued by this master and not revoked")
	c.Flags().StringVar(&o.passFile, "passphrase-file", "", "with --sign: file holding the master passphrase (- = stdin, read AFTER the request is shown)")
	c.Flags().BoolVar(&o.yes, "yes", false, "with --sign and a passphrase FILE: sign without a confirmation")
}

// localAPIBase is the URL the local API client addresses. The host is never
// resolved: every dial goes to the local listener.
const localAPIBase = "http://knomit.local/api/v1"

// localAPIClient talks to the server on the local listener at path and
// nowhere else — no TCP fallback: over TCP the server would not know who is
// asking, and refuses these endpoints.
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
		return errors.New("no local listener is configured for this home; `knomit oauth` works only over it")
	}
	return fn(localAPIClient(cfg.Socket))
}

type pendingJSON struct {
	ID          string    `json:"id"`
	ClientID    string    `json:"client_id"`
	ClientName  string    `json:"client_name"`
	RedirectURI string    `json:"redirect_uri"`
	Scopes      []string  `json:"scopes"`
	Resource    string    `json:"resource"`
	RemoteAddr  string    `json:"remote_addr"`
	UserAgent   string    `json:"user_agent"`
	ExpiresAt   time.Time `json:"expires_at"`
	Subject     string    `json:"subject"`
	Ceiling     []string  `json:"ceiling"`
}

func oauthPending(ctx context.Context, c *http.Client, out io.Writer) error {
	var body struct {
		Pending []pendingJSON `json:"pending"`
	}
	if err := localCall(ctx, c, http.MethodGet, "/oauth/pending", nil, &body); err != nil {
		return err
	}
	if len(body.Pending) == 0 {
		fmt.Fprintln(out, "no authorization requests are waiting")
		return nil
	}
	for _, p := range body.Pending {
		name := p.ClientName
		if name == "" {
			name = p.ClientID
		}
		// Every field is printed %q-quoted: this is the consent screen, and
		// what it shows came from the requester. Ingest already refuses
		// control and format characters in a CIMD; quoting here is what keeps
		// a field added later, or a pre-registered name, from regressing.
		fmt.Fprintf(out, "%s\n", p.ID)
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "  client\t%q (%q)\n", name, p.ClientID)
		fmt.Fprintf(tw, "  from\t%q  %q\n", p.RemoteAddr, p.UserAgent)
		fmt.Fprintf(tw, "  redirect\t%q\n", p.RedirectURI)
		fmt.Fprintf(tw, "  scopes\t%q\n", strings.Join(p.Scopes, " "))
		fmt.Fprintf(tw, "  resource\t%q\n", p.Resource)
		fmt.Fprintf(tw, "  expires in\t%s\n", time.Until(p.ExpiresAt).Round(time.Second))
		_ = tw.Flush()
	}
	fmt.Fprintln(out, "\napprove with: knomit oauth approve <id> --as <name> [--scopes read,write]")
	return nil
}

func oauthApprove(ctx context.Context, c *http.Client, out io.Writer, id, subject string, scopes []string) error {
	// nil scopes encodes as null, which the server reads as "not given" and
	// applies the default ceiling to; an empty non-nil list is refused there.
	req := map[string]any{"subject": subject, "scopes": scopes}
	var p pendingJSON
	if err := localCall(ctx, c, http.MethodPost, "/oauth/pending/"+id+"/approve", req, &p); err != nil {
		return err
	}
	fmt.Fprintf(out, "approved %s: the token acts as host:%s@token with ceiling %s\n",
		id, p.Subject, strings.Join(p.Ceiling, " "))
	return nil
}

func oauthDeny(ctx context.Context, c *http.Client, out io.Writer, id string) error {
	if err := localCall(ctx, c, http.MethodPost, "/oauth/pending/"+id+"/deny", nil, nil); err != nil {
		return err
	}
	fmt.Fprintf(out, "denied %s\n", id)
	return nil
}

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
		var prob struct {
			Detail string `json:"detail"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&prob)
		if prob.Detail == "" {
			prob.Detail = resp.Status
		}
		return fmt.Errorf("%s", prob.Detail)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
