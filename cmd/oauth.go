package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"knomit/internal/edge/oauth"
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
		Use:   "oauth",
		Short: "List, approve and deny OAuth authorization requests waiting on this instance",
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
			"fleet root key: `approve <id> --sign <issuer> --dir <fleet-root-dir> --instance <fingerprint> --as ...`\n" +
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

// signFlags adds the fleet-root-key signing flags (consent path 2) to approve
// and deny.
func signFlags(c *cobra.Command, o *signOpts) {
	c.Flags().StringVar(&o.issuer, "sign", "", "sign with the fleet root key instead, for the instance at this issuer URL (run on the fleet root's machine)")
	c.Flags().StringVar(&o.dir, "dir", "", "with --sign: the fleet root directory (root.crt, issued.jsonl; default <home>/fleet)")
	c.Flags().StringVar(&o.instance, "instance", "", "with --sign: the instance's full fingerprint; it must be issued by this fleet root and not revoked")
	c.Flags().BoolVar(&o.yes, "yes", false, "with --sign: sign without asking \"sign this statement?\" after the request is shown")
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

	GrantsUnchanged bool `json:"grants_unchanged"`
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
		fmt.Fprintf(out, "%q\n", p.ID)
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
	principal := "host:" + p.Subject + "@token"
	fmt.Fprintf(out, "approved %s: the token acts as %q with ceiling %q\n",
		id, principal, strings.Join(p.Ceiling, " "))
	if p.GrantsUnchanged {
		// The subject was granted before: its grants stand as they are and
		// cap this token below the ceiling if they were narrowed.
		fmt.Fprintf(out, "grants unchanged; widen with `knomit grants add %q <perm>`\n", principal)
	}
	return nil
}

func oauthDeny(ctx context.Context, c *http.Client, out io.Writer, id string) error {
	if err := localCall(ctx, c, http.MethodPost, "/oauth/pending/"+id+"/deny", nil, nil); err != nil {
		return err
	}
	fmt.Fprintf(out, "denied %s\n", id)
	return nil
}
