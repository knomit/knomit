package cmd

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"knomit/internal/corpus/repos"
	"knomit/internal/foundation/config"
	"knomit/internal/foundation/pki"
)

// identityCmd is `knomit identity`: what this instance is, for the fleet.
// Every fleet operation (create, enroll, join, leave, revoke, decommission,
// the root's move) is a REST route on the local listener (/api/v1/fleet/...,
// the Fleet page); `identity show` prints what a master's enroll needs (the
// agent id and the key) and what is installed.
func identityCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "identity",
		Short: "Show this instance's agent id, key, certificate and CRL state",
		// cobra answers an unknown subcommand of a group that has no Run
		// with the group's help and exit 0, before Args is consulted. A RunE
		// makes the group runnable, so `identity <typo>` is refused by
		// NoArgs (non-zero exit, with the usage hint) and bare `identity`
		// still prints the help.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	c.AddCommand(identityShowCmd())
	return c
}

func identityShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show this instance's agent id, fingerprint, certificate and CRL state",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			return showIdentity(cmd.OutOrStdout(), cfg)
		},
	}
}

func showIdentity(out io.Writer, cfg config.Config) error {
	st, err := pki.Status(cfg.TLS.Dir, config.ResolveKeyPath(cfg))
	if err != nil {
		return err
	}
	agentID, found, err := repos.StoredAgentID(cfg.Home)
	switch {
	case err != nil:
		fmt.Fprintf(out, "agent_id: unreadable: %v\n", err)
	case !found:
		fmt.Fprintln(out, "agent_id: none (assigned when the instance first starts)")
	default:
		// A stored id that is no longer valid (upper case, from before agent
		// ids were lower case) is shown as stored, with the reason boot will
		// refuse it and the remedy. show never rewrites it.
		if verr := pki.ValidAgentID(agentID); verr != nil {
			fmt.Fprintf(out, "agent_id: %s (invalid: %v; boot refuses it: %v)\n", agentID, verr, repos.ErrStoredAgentIDInvalid)
		} else {
			fmt.Fprintf(out, "agent_id: %s\n", agentID)
		}
	}
	fmt.Fprintf(out, "key: %s\nfingerprint: %s\nshort: %s\n", st.KeyPath, st.Fingerprint, pki.Short(st.Fingerprint))
	switch {
	case !st.Enrolled:
		fmt.Fprintf(out, "certificate: none (not enrolled: POST /api/v1/fleet/join installs one)\n")
	case st.IdentityErr != nil:
		fmt.Fprintf(out, "certificate: unreadable identity: %v\n", st.IdentityErr)
	case st.Cert != nil:
		id := st.Cert
		fmt.Fprintf(out, "principal: %s:%s@cert\nemail: %q\nserial: %s\nnot_after: %s\n",
			id.PrincipalKind(), id.Fingerprint, id.Email, id.Serial.Text(16), id.NotAfter.Format(time.RFC3339))
	}
	if st.Root != nil {
		fmt.Fprintf(out, "root: %s (%q)\n", st.Root.Fingerprint, st.Root.CommonName)
	}
	if st.CRL != nil {
		fmt.Fprintf(out, "crl_number: %s\ncrl_next_update: %s\n", st.CRL.Number, st.CRL.NextUpdate.Format(time.RFC3339))
	}
	if cfg.TLS.Addr == "" {
		fmt.Fprintln(out, "tls_listener: off ([tls].addr is empty)")
	} else {
		fmt.Fprintf(out, "tls_listener: %s\n", cfg.TLS.Addr)
	}
	return nil
}
