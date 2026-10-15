package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"knomit/internal/config"
)

// fleetCmd is `knomit fleet`: a THIN client of the running server's
// /api/v1/fleet over the local listener, like `knomit oauth`. It never boots
// the app: the server owns the fleet state machine.
func fleetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "fleet",
		Short: "Show this instance's fleet (F09)",
		Long: `An instance is standalone until it creates or joins a fleet. Every fleet
operation is a REST route on the local listener (/api/v1/fleet/create, enroll,
join, leave, revoke, decommission, root), used by the Fleet page; this command
only shows the state (GET /api/v1/fleet).`,
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Show the fleet state, this agent's record state and the last error",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withFleetAPI(func(hc *http.Client) error {
				return fleetCall(cmd.Context(), hc, cmd.OutOrStdout())
			})
		},
	}
	c.AddCommand(status)
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
	Master      bool   `json:"master"`
	Hosting     string `json:"hosting"`
	RootDir     string `json:"root_dir"`
	// F10
	ExternalAddresses []string `json:"external_addresses"`
	Record            *struct {
		Addresses    []string          `json:"addresses"`
		Capabilities map[string]string `json:"capabilities"`
	} `json:"record"`
	RecordCurrent       *bool  `json:"record_current"`
	RecordPendingUpdate bool   `json:"record_pending_update"`
	Notice              string `json:"notice"`
}

func fleetCall(ctx context.Context, hc *http.Client, out io.Writer) error {
	var st fleetStatusJSON
	if err := localCall(ctx, hc, http.MethodGet, "/fleet", nil, &st); err != nil {
		return err
	}
	printFleetStatus(out, st)
	return nil
}

func printFleetStatus(out io.Writer, st fleetStatusJSON) {
	fmt.Fprintf(out, "state:  %s (since %s)\n", st.State, st.Since)
	fmt.Fprintf(out, "agent:  %s\n", st.AgentID)
	if st.FleetRepo != "" {
		fmt.Fprintf(out, "fleet:  %s (hosting: %s)\n", st.FleetRepo, st.Hosting)
		fmt.Fprintf(out, "record: %s\n", st.RecordState)
		role := "member"
		if st.Master {
			role = "master"
		}
		fmt.Fprintf(out, "role:   %s (fleet root directory: %s)\n", role, st.RootDir)
	}
	if len(st.ExternalAddresses) > 0 {
		fmt.Fprintf(out, "addresses (knomit.toml): %s\n", strings.Join(st.ExternalAddresses, " "))
	}
	if st.Record != nil {
		fmt.Fprintf(out, "addresses (record):      %s\n", strings.Join(st.Record.Addresses, " "))
		caps := make([]string, 0, len(st.Record.Capabilities))
		for k, v := range st.Record.Capabilities {
			caps = append(caps, k+"="+v)
		}
		sort.Strings(caps)
		fmt.Fprintf(out, "capabilities: %s\n", strings.Join(caps, " "))
	}
	switch {
	case st.RecordCurrent != nil && !*st.RecordCurrent:
		fmt.Fprintln(out, "record differs from knomit.toml / this binary: restart to update it")
	case st.RecordPendingUpdate:
		fmt.Fprintln(out, "record: pending update (waiting for the fleet's master to merge it into main)")
	}
	if st.Notice != "" {
		fmt.Fprintf(out, "notice: %s\n", st.Notice)
	}
	if st.LastError != "" {
		fmt.Fprintf(out, "last error (%s): %s\n", st.LastAttempt, st.LastError)
	}
}
