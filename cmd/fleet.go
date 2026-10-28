package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"knomit/internal/foundation/config"
)

// fleetCmd is `knomit fleet`: a THIN client of the running server's
// /api/v1/fleet over the local listener, like `knomit oauth`. It never boots
// the app: the server owns the fleet state machine. Every verb but status is
// one request to one route, its arguments mapped onto the route's body and
// the server's answer printed as it came; refusals are the server's.
func fleetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "fleet",
		Short: "Show and change this instance's fleet (F09)",
		Long: `An instance is standalone until it creates or joins a fleet. Every fleet
operation is a REST route on the local listener (/api/v1/fleet/...), used by the
Fleet page. Each verb here sends one request to its route over that listener
and prints the server's JSON answer; status prints GET /api/v1/fleet as text.`,
		// As `knomit identity`: a RunE makes `fleet <typo>` a non-zero exit.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
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
	c.AddCommand(fleetVerbs()...)
	return c
}

// fleetVerbs is one command per mutating route of the server's RequireLocalVia
// fleet group (internal/edge/web/api/manage/mount.go). None checks, prompts
// or confirms anything: running the verb is the request.
func fleetVerbs() []*cobra.Command {
	var create struct{ name, origin, branch, authMethod, authToken string }
	createCmd := fleetVerb("create", "Found a fleet with a fresh root; this instance is its master", cobra.NoArgs,
		http.MethodPost, "/fleet/create", func(_ *cobra.Command, _ []string) (any, error) {
			body := map[string]any{}
			if create.name != "" {
				body["name"] = create.name
			}
			if create.origin != "" {
				body["origin"] = map[string]string{"url": create.origin, "branch": create.branch,
					"auth_method": create.authMethod, "auth_token": create.authToken}
			}
			return body, nil
		})
	createCmd.Flags().StringVar(&create.name, "name", "", "the fleet repository's name")
	createCmd.Flags().StringVar(&create.origin, "origin", "", "host the fleet on this forge repository URL")
	createCmd.Flags().StringVar(&create.branch, "branch", "", "with --origin: the origin branch")
	createCmd.Flags().StringVar(&create.authMethod, "auth-method", "", "with --origin: the origin's auth method")
	createCmd.Flags().StringVar(&create.authToken, "auth-token", "", "with --origin: the origin's credential")

	var address string
	enrollCmd := fleetVerb("enroll <agent-id> <public-key>", "Master: issue a member's leaf and print its bundle", cobra.ExactArgs(2),
		http.MethodPost, "/fleet/enroll", func(_ *cobra.Command, args []string) (any, error) {
			return map[string]string{"agent_id": args[0], "public_key": args[1], "address": address}, nil
		})
	enrollCmd.Flags().StringVar(&address, "address", "", "which of this instance's external_addresses the bundle's fleet URL is built from")

	join := func(use, short, path string) *cobra.Command {
		var authMethod, authToken string
		j := fleetVerb(use+" <bundle-file|->", short, cobra.ExactArgs(1),
			http.MethodPost, path, func(cmd *cobra.Command, args []string) (any, error) {
				bundle, err := readFileOrStdin(cmd, args[0])
				if err != nil {
					return nil, err
				}
				return map[string]string{"bundle": string(bundle), "auth_method": authMethod, "auth_token": authToken}, nil
			})
		j.Flags().StringVar(&authMethod, "auth-method", "", "for a forge-hosted fleet: this instance's auth method")
		j.Flags().StringVar(&authToken, "auth-token", "", "for a forge-hosted fleet: this instance's own credential")
		return j
	}
	joinCmd := join("join", "Join the fleet the bundle names (bundle from the master's enroll)", "/fleet/join")
	joinCheckCmd := join("join-check", "Check a bundle without joining: where it points and its root fingerprint", "/fleet/join:check")

	leaveCmd := fleetVerb("leave", "Leave the fleet: push the left record once, then remove the fleet here", cobra.NoArgs,
		http.MethodPost, "/fleet/leave", nil)

	var reason string
	revokeCmd := fleetVerb("revoke <agent-id>", "Master: revoke a member's leaf", cobra.ExactArgs(1),
		http.MethodPost, "/fleet/revoke", func(_ *cobra.Command, args []string) (any, error) {
			return map[string]string{"agent_id": args[0], "reason": reason}, nil
		})
	revokeCmd.Flags().StringVar(&reason, "reason", "", "why, recorded with the revocation")

	decommissionCmd := fleetVerb("decommission", "Master: end the fleet here and delete its root", cobra.NoArgs,
		http.MethodPost, "/fleet/decommission", nil)

	rootCmd := &cobra.Command{
		Use:   "root",
		Short: "Get, put or delete this instance's fleet root files",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	rootCmd.AddCommand(
		fleetVerb("get", "Print the fleet root files (the root key among them) as JSON", cobra.NoArgs,
			http.MethodGet, "/fleet/root", nil),
		fleetVerb("put <json-file|->", `Install another master's root files: the body {"files": {...}} as given`, cobra.ExactArgs(1),
			http.MethodPut, "/fleet/root", func(cmd *cobra.Command, args []string) (any, error) {
				raw, err := readFileOrStdin(cmd, args[0])
				if err != nil {
					return nil, err
				}
				if !json.Valid(raw) {
					return nil, errors.New("the root files are not JSON")
				}
				return json.RawMessage(raw), nil
			}),
		fleetVerb("delete", "Stand down as master: the root files go, the membership stays", cobra.NoArgs,
			http.MethodDelete, "/fleet/root", nil),
	)

	var confirmLoss bool
	hostCmd := fleetVerb("host", "Host the fleet here: this copy drops its origin and becomes the hub", cobra.NoArgs,
		http.MethodPost, "/fleet/host", func(_ *cobra.Command, _ []string) (any, error) {
			return map[string]bool{"confirm_loss": confirmLoss}, nil
		})
	hostCmd.Flags().BoolVar(&confirmLoss, "confirm-loss", false, "host even though the old master has not stood down")

	var follow struct{ authMethod, authToken, user, password string }
	followCmd := fleetVerb("follow <url>", "Point at the new host: follow the fleet from url", cobra.ExactArgs(1),
		http.MethodPost, "/fleet/follow", func(_ *cobra.Command, args []string) (any, error) {
			return map[string]string{"url": args[0], "auth_method": follow.authMethod, "auth_token": follow.authToken,
				"user": follow.user, "password": follow.password}, nil
		})
	followCmd.Flags().StringVar(&follow.authMethod, "auth-method", "", "the new origin's auth method")
	followCmd.Flags().StringVar(&follow.authToken, "auth-token", "", "the new origin's credential")
	followCmd.Flags().StringVar(&follow.user, "user", "", "with --auth-method basic: the user")
	followCmd.Flags().StringVar(&follow.password, "password", "", "with --auth-method basic: the password")

	indexRebuildCmd := fleetVerb("index-rebuild", "Rebuild the fleet repository's index", cobra.NoArgs,
		http.MethodPost, "/fleet/index-rebuild", nil)

	var drop struct {
		leaveOutMerge bool
		leftOut       []string
	}
	dropCmd := fleetVerb("drop-commit <commit>", "Rebuild the fleet's agent branch on main without a commit that holds main back", cobra.ExactArgs(1),
		http.MethodPost, "/fleet/drop-commit", func(_ *cobra.Command, args []string) (any, error) {
			return map[string]any{"commit": args[0], "leave_out_merge": drop.leaveOutMerge, "left_out": drop.leftOut}, nil
		})
	dropCmd.Flags().BoolVar(&drop.leaveOutMerge, "leave-out-merge", false, "leave the whole merge out instead (needs --left-out)")
	dropCmd.Flags().StringSliceVar(&drop.leftOut, "left-out", nil, "with --leave-out-merge: the commit hashes a refusal's leave_out listed")

	return []*cobra.Command{createCmd, enrollCmd, joinCmd, joinCheckCmd, leaveCmd, revokeCmd, decommissionCmd,
		rootCmd, hostCmd, followCmd, indexRebuildCmd, dropCmd}
}

// fleetVerb is one route as a command: body (nil for a route without one)
// builds the request from the arguments, and the answer is printed by
// fleetAPI.
func fleetVerb(use, short string, args cobra.PositionalArgs, method, path string,
	body func(*cobra.Command, []string) (any, error)) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  args,
		RunE: func(cmd *cobra.Command, a []string) error {
			var in any
			if body != nil {
				var err error
				if in, err = body(cmd, a); err != nil {
					return err
				}
			}
			return withFleetAPI(func(hc *http.Client) error {
				return fleetAPI(cmd.Context(), hc, cmd.OutOrStdout(), method, path, in)
			})
		},
	}
}

// fleetAPI sends one request over the local listener and prints the answer
// indented; an answer with no body (204) prints nothing. A refusal comes back
// as localCall's error, the server's detail.
func fleetAPI(ctx context.Context, hc *http.Client, out io.Writer, method, path string, in any) error {
	var raw json.RawMessage
	if err := localCall(ctx, hc, method, path, in, &raw); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return err
	}
	buf.WriteByte('\n')
	_, err := out.Write(buf.Bytes())
	return err
}

// readFileOrStdin reads a file, or stdin for "-".
func readFileOrStdin(cmd *cobra.Command, name string) ([]byte, error) {
	if name == "-" {
		return io.ReadAll(cmd.InOrStdin())
	}
	return os.ReadFile(name)
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
	AgentID     string `json:"agent_id"`
	RecordState string `json:"record_state"`
	Since       string `json:"since"`
	LastAttempt string `json:"last_attempt"`
	LastError   string `json:"last_error"`
	Master      bool   `json:"master"`
	Hosting     string `json:"hosting"`
	RootDir     string `json:"root_dir"`
	// F10
	Host struct {
		ExternalAddresses []string `json:"external_addresses"`
	} `json:"host"`
	Record *struct {
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
		switch st.RecordState {
		case "":
			fmt.Fprintln(out, "record: none")
		case "pending":
			fmt.Fprintln(out, "record: pending (on this instance's branch only; accepted once the fleet's master merges it into main)")
		default:
			fmt.Fprintf(out, "record: %s (on the fleet's main)\n", st.RecordState)
		}
		fmt.Fprintf(out, "fleet master: %t (fleet root directory: %s)\n", st.Master, st.RootDir)
	}
	if len(st.Host.ExternalAddresses) > 0 {
		fmt.Fprintf(out, "addresses (knomit.toml): %s\n", strings.Join(st.Host.ExternalAddresses, " "))
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
