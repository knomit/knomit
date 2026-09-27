package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"

	"knomit/internal/config"
)

// fleetCmd is `knomit fleet`: a THIN client of the running server's
// /api/v1/fleet over the local listener, like `knomit oauth`. It never boots
// the app: the server owns the fleet state machine.
func fleetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "fleet",
		Short: "Show, join or leave this instance's fleet (F09)",
		Long: `An instance is standalone until it registers with a fleet: a knomit
repository whose ontology is the fleet preset. Registering mounts it, writes this
instance's member record on its agent branch and pushes it; a human accepts by
merging that branch into the fleet's main. Unregistering pushes a "left" record
first, and only then unmounts the fleet repository (retrying until the push
succeeds).`,
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Show the fleet state, this agent's record state and the last error",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withFleetAPI(func(hc *http.Client) error {
				return fleetCall(cmd.Context(), hc, cmd.OutOrStdout(), http.MethodGet, nil)
			})
		},
	}
	var token, method string
	register := &cobra.Command{
		Use:   "register <fleet-repo-url>",
		Short: "Join a fleet (or refresh this instance's record in it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]string{"url": args[0], "auth_method": method, "auth_token": token}
			return withFleetAPI(func(hc *http.Client) error {
				return fleetCall(cmd.Context(), hc, cmd.OutOrStdout(), http.MethodPut, body)
			})
		},
	}
	register.Flags().StringVar(&token, "token", "", "access token for the fleet repository's origin")
	register.Flags().StringVar(&method, "auth-method", "", "auth method for the origin (default: inferred)")
	unregister := &cobra.Command{
		Use:   "unregister",
		Short: "Leave the fleet: push a left record, then unmount the fleet repository",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withFleetAPI(func(hc *http.Client) error {
				return fleetCall(cmd.Context(), hc, cmd.OutOrStdout(), http.MethodDelete, nil)
			})
		},
	}
	c.AddCommand(status, register, unregister)
	return c
}

func withFleetAPI(fn func(*http.Client) error) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.Socket == "" {
		return errors.New("no local listener is configured for this home; `knomit fleet` works only over it")
	}
	return fn(localAPIClient(cfg.Socket))
}

type fleetStatusJSON struct {
	State       string `json:"state"`
	FleetRepo   string `json:"fleet_repo"`
	FleetURL    string `json:"fleet_url"`
	AgentID     string `json:"agent_id"`
	RecordState string `json:"record_state"`
	Since       string `json:"since"`
	LastAttempt string `json:"last_attempt"`
	LastError   string `json:"last_error"`
}

func fleetCall(ctx context.Context, hc *http.Client, out io.Writer, method string, body any) error {
	var st fleetStatusJSON
	if err := localCall(ctx, hc, method, "/fleet", body, &st); err != nil {
		return err
	}
	printFleetStatus(out, st)
	return nil
}

func printFleetStatus(out io.Writer, st fleetStatusJSON) {
	fmt.Fprintf(out, "state:  %s (since %s)\n", st.State, st.Since)
	fmt.Fprintf(out, "agent:  %s\n", st.AgentID)
	if st.FleetRepo != "" {
		fmt.Fprintf(out, "fleet:  %s (%s)\n", st.FleetRepo, st.FleetURL)
		fmt.Fprintf(out, "record: %s\n", st.RecordState)
	}
	if st.LastError != "" {
		fmt.Fprintf(out, "last error (%s): %s\n", st.LastAttempt, st.LastError)
	}
}
