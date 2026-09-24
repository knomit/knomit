package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"knomit/internal/config"
	"knomit/internal/oauth/idp"
)

// githubAPIBase, when set (tests only), points the resolve helper at a
// fake GitHub instead of api.github.com.
var githubAPIBase string

// oauthIDPCmd is `knomit oauth idp`: operator helpers for consent path 3
// (F19 phase 3c). The allow list in [oauth.idp] takes stable numeric ids
// only, because a GitHub login can be renamed and then claimed by someone
// else; `resolve` is how an operator turns a login into the id to paste,
// once, deliberately. It needs no server, no secret and no token: GitHub's
// public users endpoint answers it.
func oauthIDPCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "idp",
		Short: "Helpers for approving OAuth requests through an external identity provider",
	}
	c.AddCommand(&cobra.Command{
		Use:          "resolve github <login>",
		SilenceUsage: true,
		Short:        "Print the allow-list entry (github-<numeric id>) for a GitHub login",
		Args:         cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "github" {
				return fmt.Errorf("unknown provider %q: the only provider is github", args[0])
			}
			g := idp.NewGitHub("", config.IDPSecret{})
			if githubAPIBase != "" {
				g.SetEndpoints(githubAPIBase, githubAPIBase)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			sub, err := g.LookupLogin(ctx, args[1])
			if errors.Is(err, idp.ErrUnknownLogin) {
				return fmt.Errorf("github has no such account: %q", args[1])
			}
			if err != nil {
				return err
			}
			// The login is GitHub's answer, printed quoted; the id is ours.
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s is GitHub account %q today. Allow it with:\n\n", sub.ID, sub.Login)
			fmt.Fprintf(out, "  [oauth.idp]\n  allowed_subjects = [%q]\n\n", sub.ID)
			fmt.Fprintln(out, "The id stays with the account if it is renamed; the login does not.")
			return nil
		},
	})
	return c
}
