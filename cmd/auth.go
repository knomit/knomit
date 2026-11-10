package cmd

// `knomit auth` (unified auth plan P3, Task 3.5): the instance's credentials —
// one-time browser login links, personal access tokens, browser sessions and
// role bindings. Every verb is ONE request to one /api/v1/auth route over the
// local listener (thin verbs, like `knomit fleet`).
//
// HIDDEN until the flip (P4): until then a session or a token is accepted
// nowhere on the main port, so these verbs mint credentials that do nothing
// yet. P4 unhides the group and adds pending/approve/deny.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"knomit/internal/edge/web/httpx"
)

func authCmd() *cobra.Command {
	c := &cobra.Command{
		Use:    "auth",
		Short:  "Browser login links, personal access tokens, sessions and role bindings",
		Hidden: true,
		Long: `Manage this instance's credentials over the local listener (the unix socket,
or the named pipe on Windows). Each verb sends one request to /api/v1/auth.`,
		// A RunE makes `auth <typo>` a non-zero exit (cobra otherwise prints
		// help and exits 0 for an unknown subcommand of a group).
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	c.AddCommand(authLinkCmd(), authTokensCmd(), authSessionsCmd(), authRolesCmd())
	return c
}

// group is a parent command that only holds verbs: a typo exits non-zero.
func group(use, short string, verbs ...*cobra.Command) *cobra.Command {
	g := &cobra.Command{Use: use, Short: short, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	g.AddCommand(verbs...)
	return g
}

func local(cmd *cobra.Command, fn func(ctx context.Context, c *http.Client, out io.Writer) error) error {
	return withLocalAPI(func(c *http.Client) error { return fn(cmd.Context(), c, cmd.OutOrStdout()) })
}

func printJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// --- link -------------------------------------------------------------------

func authLinkCmd() *cobra.Command {
	var asJSON bool
	var base string
	c := &cobra.Command{
		Use:   "link",
		Short: "Print a one-time link that signs a browser in as you (valid 2 minutes, once)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return local(cmd, func(ctx context.Context, c *http.Client, out io.Writer) error {
				return authLink(ctx, c, out, asJSON, base)
			})
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print {url, expires_at} as JSON")
	c.Flags().StringVar(&base, "url", "", "base the link on this external address (default: the first one)")
	return c
}

func authLink(ctx context.Context, c *http.Client, out io.Writer, asJSON bool, base string) error {
	path := "/auth/links"
	if base != "" {
		path += "?url=" + url.QueryEscape(base)
	}
	var res struct {
		URL       string    `json:"url"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := localCall(ctx, c, http.MethodPost, path, nil, &res); err != nil {
		return err
	}
	if asJSON {
		return printJSON(out, res)
	}
	_, err := fmt.Fprintln(out, res.URL)
	return err
}

// --- tokens -----------------------------------------------------------------

type tokenJSON struct {
	ID        string     `json:"id"`
	Label     string     `json:"label"`
	Role      string     `json:"role"`
	CreatedBy string     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	LastUsed  *time.Time `json:"last_used_at,omitempty"`
	Secret    string     `json:"secret,omitempty"`
}

func authTokensCmd() *cobra.Command {
	var role, label, expires string
	var createJSON, listJSON bool
	create := &cobra.Command{
		Use:   "create --role <reader|contributor|operator> --label <text> [--expires 90d]",
		Short: "Create a personal access token (its secret is printed once)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return local(cmd, func(ctx context.Context, c *http.Client, out io.Writer) error {
				return authTokensCreate(ctx, c, out, role, label, expires, createJSON)
			})
		},
	}
	create.Flags().StringVar(&role, "role", "", "reader, contributor or operator")
	create.Flags().StringVar(&label, "label", "", "what the token is for")
	create.Flags().StringVar(&expires, "expires", "", "lifetime from now, e.g. 90d, 12h (default [auth].pat_default_ttl; at most [auth].pat_max_ttl)")
	create.Flags().BoolVar(&createJSON, "json", false, "print the token, secret included, as JSON")
	_ = create.MarkFlagRequired("role")
	_ = create.MarkFlagRequired("label")
	list := &cobra.Command{
		Use:   "list",
		Short: "List the personal access tokens that can still be used",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return local(cmd, func(ctx context.Context, c *http.Client, out io.Writer) error {
				return authTokensList(ctx, c, out, listJSON)
			})
		},
	}
	list.Flags().BoolVar(&listJSON, "json", false, "print as JSON")
	revoke := &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke a personal access token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return local(cmd, func(ctx context.Context, c *http.Client, out io.Writer) error {
				return authTokensRevoke(ctx, c, out, args[0])
			})
		},
	}
	return group("tokens", "Personal access tokens", create, list, revoke)
}

func authTokensCreate(ctx context.Context, c *http.Client, out io.Writer, role, label, expires string, asJSON bool) error {
	var res tokenJSON
	body := httpx.AuthTokenCreateBody{Role: role, Label: label, ExpiresIn: expires}
	if err := localCall(ctx, c, http.MethodPost, "/auth/tokens", body, &res); err != nil {
		return err
	}
	if asJSON {
		return printJSON(out, res)
	}
	fmt.Fprintf(out, "token %s (%s, %q) expires %s\n", res.ID, res.Role, res.Label, res.ExpiresAt.Local().Format(time.RFC3339))
	fmt.Fprintf(out, "secret (shown once; store it now): %s\n", res.Secret)
	return nil
}

func authTokensList(ctx context.Context, c *http.Client, out io.Writer, asJSON bool) error {
	var res struct {
		Tokens []tokenJSON `json:"tokens"`
	}
	if err := localCall(ctx, c, http.MethodGet, "/auth/tokens", nil, &res); err != nil {
		return err
	}
	if asJSON {
		return printJSON(out, res.Tokens)
	}
	if len(res.Tokens) == 0 {
		_, err := fmt.Fprintln(out, "no personal access tokens")
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tROLE\tLABEL\tEXPIRES\tLAST USED")
	for _, t := range res.Tokens {
		used := "never"
		if t.LastUsed != nil {
			used = t.LastUsed.Local().Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%q\t%s\t%s\n", t.ID, t.Role, t.Label, t.ExpiresAt.Local().Format(time.RFC3339), used)
	}
	return tw.Flush()
}

func authTokensRevoke(ctx context.Context, c *http.Client, out io.Writer, id string) error {
	if err := localCall(ctx, c, http.MethodDelete, "/auth/tokens/"+url.PathEscape(id), nil, nil); err != nil {
		return err
	}
	_, err := fmt.Fprintf(out, "revoked token %s\n", id)
	return err
}

// --- sessions ---------------------------------------------------------------

func authSessionsCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "sessions",
		Short: "List browser sessions (sessions revoke <id> ends one)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return local(cmd, func(ctx context.Context, c *http.Client, out io.Writer) error {
				return authSessions(ctx, c, out, asJSON)
			})
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	c.AddCommand(&cobra.Command{
		Use:   "revoke <id>",
		Short: "End a browser session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return local(cmd, func(ctx context.Context, c *http.Client, out io.Writer) error {
				return authSessionsRevoke(ctx, c, out, args[0])
			})
		},
	})
	return c
}

func authSessions(ctx context.Context, c *http.Client, out io.Writer, asJSON bool) error {
	var res struct {
		Sessions []struct {
			ID         string    `json:"id"`
			Principal  string    `json:"principal"`
			UserAgent  string    `json:"user_agent"`
			CreatedAt  time.Time `json:"created_at"`
			LastSeenAt time.Time `json:"last_seen_at"`
			ExpiresAt  time.Time `json:"expires_at"`
		} `json:"sessions"`
	}
	if err := localCall(ctx, c, http.MethodGet, "/auth/sessions", nil, &res); err != nil {
		return err
	}
	if asJSON {
		return printJSON(out, res.Sessions)
	}
	if len(res.Sessions) == 0 {
		_, err := fmt.Fprintln(out, "no browser sessions")
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tPRINCIPAL\tLAST SEEN\tBROWSER")
	for _, s := range res.Sessions {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%q\n", s.ID, s.Principal, s.LastSeenAt.Local().Format(time.RFC3339), s.UserAgent)
	}
	return tw.Flush()
}

func authSessionsRevoke(ctx context.Context, c *http.Client, out io.Writer, id string) error {
	if err := localCall(ctx, c, http.MethodDelete, "/auth/sessions/"+url.PathEscape(id), nil, nil); err != nil {
		return err
	}
	_, err := fmt.Fprintf(out, "ended session %s\n", id)
	return err
}

// --- roles ------------------------------------------------------------------

func authRolesCmd() *cobra.Command {
	var asJSON bool
	var label string
	list := &cobra.Command{
		Use:   "list",
		Short: "List the stored role bindings (the owner is implicit and not listed)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return local(cmd, func(ctx context.Context, c *http.Client, out io.Writer) error {
				return authRolesList(ctx, c, out, asJSON)
			})
		},
	}
	list.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	bind := &cobra.Command{
		Use:   "bind <principal> <role>",
		Short: "Bind a principal (os:uid:<n>, os:sid:<SID>, anonymous) to a role",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return local(cmd, func(ctx context.Context, c *http.Client, out io.Writer) error {
				return authRolesBind(ctx, c, out, args[0], args[1], label)
			})
		},
	}
	bind.Flags().StringVar(&label, "label", "", "a display name for the principal")
	unbind := &cobra.Command{
		Use:   "unbind <principal> <role>",
		Short: "Remove a role binding",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return local(cmd, func(ctx context.Context, c *http.Client, out io.Writer) error {
				return authRolesUnbind(ctx, c, out, args[0], args[1])
			})
		},
	}
	return group("roles", "Role bindings", list, bind, unbind)
}

func authRolesList(ctx context.Context, c *http.Client, out io.Writer, asJSON bool) error {
	var res struct {
		Bindings []struct {
			Principal string    `json:"principal"`
			Role      string    `json:"role"`
			Label     string    `json:"label"`
			By        string    `json:"by"`
			At        time.Time `json:"at"`
		} `json:"bindings"`
	}
	if err := localCall(ctx, c, http.MethodGet, "/auth/bindings", nil, &res); err != nil {
		return err
	}
	if asJSON {
		return printJSON(out, res.Bindings)
	}
	if len(res.Bindings) == 0 {
		_, err := fmt.Fprintln(out, "no role bindings (the owner holds everything implicitly)")
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PRINCIPAL\tROLE\tLABEL\tBY")
	for _, b := range res.Bindings {
		fmt.Fprintf(tw, "%s\t%s\t%q\t%s\n", b.Principal, b.Role, b.Label, b.By)
	}
	return tw.Flush()
}

func authRolesBind(ctx context.Context, c *http.Client, out io.Writer, principal, role, label string) error {
	body := httpx.AuthBindingBody{Principal: principal, Role: role, Label: label}
	if err := localCall(ctx, c, http.MethodPost, "/auth/bindings", body, nil); err != nil {
		return err
	}
	_, err := fmt.Fprintf(out, "bound %s to %s\n", principal, role)
	return err
}

func authRolesUnbind(ctx context.Context, c *http.Client, out io.Writer, principal, role string) error {
	path := "/auth/bindings/" + url.PathEscape(principal) + "/" + url.PathEscape(role)
	if err := localCall(ctx, c, http.MethodDelete, path, nil, nil); err != nil {
		return err
	}
	_, err := fmt.Fprintf(out, "unbound %s from %s\n", principal, role)
	return err
}
