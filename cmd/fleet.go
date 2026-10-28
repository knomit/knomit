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

	"knomit/internal/corpus/repos"
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
and prints the server's JSON answer. Verbs are grouped by subject: members
(list, enroll, revoke), master (export, import, resign), hosting (here,
follow), sync (is it blocked; drop the commit that holds it) and index
(rebuild). status and sync print GET /api/v1/fleet as text; a bare members
lists the members.`,
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

// fleetVerbs is the verb tree, grouped by subject. Every leaf is one request
// to one existing route (the mutating ones are the server's RequireLocalVia
// fleet group, internal/edge/web/api/manage/mount.go). None checks, prompts or
// confirms anything: running the verb is the request.
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

	// join and join --check differ only in the route: the check is the same
	// body to /fleet/join:check.
	var join struct {
		check                 bool
		authMethod, authToken string
	}
	joinCmd := &cobra.Command{
		Use:   "join <bundle-file|->",
		Short: "Join the fleet the bundle names (bundle from the master's members enroll); --check only looks",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			bundle, err := readFileOrStdin(cmd, args[0])
			if err != nil {
				return err
			}
			path := "/fleet/join"
			if join.check {
				path = "/fleet/join:check"
			}
			in := map[string]string{"bundle": string(bundle), "auth_method": join.authMethod, "auth_token": join.authToken}
			return withFleetAPI(func(hc *http.Client) error {
				return fleetAPI(cmd.Context(), hc, cmd.OutOrStdout(), http.MethodPost, path, in)
			})
		},
	}
	joinCmd.Flags().BoolVar(&join.check, "check", false, "do not join: show where the bundle points and its root fingerprint")
	joinCmd.Flags().StringVar(&join.authMethod, "auth-method", "", "for a forge-hosted fleet: this instance's auth method")
	joinCmd.Flags().StringVar(&join.authToken, "auth-token", "", "for a forge-hosted fleet: this instance's own credential")

	leaveCmd := fleetVerb("leave", "Leave the fleet: push the left record once, then remove the fleet here", cobra.NoArgs,
		http.MethodPost, "/fleet/leave", nil)
	decommissionCmd := fleetVerb("decommission", "Master: end the fleet here and delete its root", cobra.NoArgs,
		http.MethodPost, "/fleet/decommission", nil)

	// members: bare is the read; enroll and revoke change it.
	membersCmd := fleetVerb("members", "List the fleet's members (enroll and revoke below)", cobra.NoArgs,
		http.MethodGet, "/fleet/members", nil)
	var address, reason string
	enrollCmd := fleetVerb("enroll <agent-id> <public-key>", "Master: issue a member's leaf and print its bundle", cobra.ExactArgs(2),
		http.MethodPost, "/fleet/enroll", func(_ *cobra.Command, args []string) (any, error) {
			return map[string]string{"agent_id": args[0], "public_key": args[1], "address": address}, nil
		})
	enrollCmd.Flags().StringVar(&address, "address", "", "which of this instance's external_addresses the bundle's fleet URL is built from")
	revokeCmd := fleetVerb("revoke <agent-id>", "Master: revoke a member's leaf", cobra.ExactArgs(1),
		http.MethodPost, "/fleet/revoke", func(_ *cobra.Command, args []string) (any, error) {
			return map[string]string{"agent_id": args[0], "reason": reason}, nil
		})
	revokeCmd.Flags().StringVar(&reason, "reason", "", "why, recorded with the revocation")
	membersCmd.AddCommand(enrollCmd, revokeCmd)

	masterCmd := fleetGroup(repos.HostingMaster, "Export, import or resign this instance's fleet root files")
	masterCmd.AddCommand(
		fleetVerb("export", "Print the fleet root files (the root key among them) as JSON", cobra.NoArgs,
			http.MethodGet, "/fleet/root", nil),
		fleetVerb("import <json-file|->", `Install another master's root files: the body {"files": {...}} as given`, cobra.ExactArgs(1),
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
		fleetVerb("resign", "Stand down as master: the root files go, the membership stays", cobra.NoArgs,
			http.MethodDelete, "/fleet/root", nil),
	)

	hostingCmd := fleetGroup("hosting", "Where the fleet repository is hosted")
	var confirmLoss bool
	hereCmd := fleetVerb("here", "Host the fleet here: this copy drops its origin and becomes the hub", cobra.NoArgs,
		http.MethodPost, "/fleet/host", func(_ *cobra.Command, _ []string) (any, error) {
			return map[string]bool{"confirm_loss": confirmLoss}, nil
		})
	hereCmd.Flags().BoolVar(&confirmLoss, "confirm-loss", false, "host even though the old master has not stood down")
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
	hostingCmd.AddCommand(hereCmd, followCmd)

	indexCmd := fleetGroup("index", "The fleet repository's index")
	indexCmd.AddCommand(fleetVerb("rebuild", "Rebuild the fleet repository's index", cobra.NoArgs,
		http.MethodPost, "/fleet/index-rebuild", nil))

	return []*cobra.Command{createCmd, joinCmd, leaveCmd, decommissionCmd, membersCmd, masterCmd, hostingCmd, fleetSyncCmd(), indexCmd}
}

// fleetGroup is a command that only groups verbs: as `knomit identity`, a
// RunE makes an unknown subcommand a non-zero exit.
func fleetGroup(use, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
}

// fleetSyncCmd is `sync` (reads GET /fleet and says whether the fleet's rules
// hold this instance's branch back) and `sync drop`.
func fleetSyncCmd() *cobra.Command {
	syncCmd := &cobra.Command{
		Use:   "sync",
		Short: "Show whether this instance's sync is blocked, and the last error (drop below)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withFleetAPI(func(hc *http.Client) error {
				var st fleetStatusJSON
				if err := localCall(cmd.Context(), hc, http.MethodGet, "/fleet", nil, &st); err != nil {
					return err
				}
				printFleetSync(cmd.OutOrStdout(), st)
				return nil
			})
		},
	}
	var leaveOut []string
	dropCmd := &cobra.Command{
		Use:   "drop [<commit>]",
		Short: "Drop the commit the fleet's rules refuse so this instance can sync again (default: the commit `sync` reports)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withFleetAPI(func(hc *http.Client) error {
				ctx := cmd.Context()
				var commit string
				if len(args) == 1 {
					commit = args[0]
				} else {
					var st fleetStatusJSON
					if err := localCall(ctx, hc, http.MethodGet, "/fleet", nil, &st); err != nil {
						return err
					}
					if !st.LastErrorMainHeld || st.LastErrorMainHeldCommit == "" {
						return errors.New("sync is not blocked: there is no commit to drop")
					}
					commit = st.LastErrorMainHeldCommit
				}
				in := map[string]any{"commit": commit}
				if len(leaveOut) > 0 {
					in["leave_out_merge"] = true
					in["left_out"] = leaveOut
				}
				err := fleetAPI(ctx, hc, cmd.OutOrStdout(), http.MethodPost, "/fleet/drop-commit", in)
				return withLeaveOutPlan(err, commit)
			})
		},
	}
	dropCmd.Flags().StringSliceVar(&leaveOut, "leave-out", nil, "also drop these commits, exactly as a refused drop listed them (confirms the loss)")
	syncCmd.AddCommand(dropCmd)
	return syncCmd
}

// withLeaveOutPlan turns a refused drop that carries a leave_out list into an
// error that shows the list and the exact command that confirms it; any other
// error passes through.
func withLeaveOutPlan(err error, commit string) error {
	var le *localError
	if !errors.As(err, &le) || len(le.LeaveOut) == 0 {
		return err
	}
	var b strings.Builder
	b.WriteString(plainLine(le.Detail))
	hashes := make([]string, 0, len(le.LeaveOut))
	for _, c := range le.LeaveOut {
		who := c.Agent
		if who == "" {
			who = c.Author
		}
		fmt.Fprintf(&b, "\n  %s %s: %s", short8(c.Commit), plainLine(who), plainLine(c.Subject))
		hashes = append(hashes, c.Commit)
	}
	fmt.Fprintf(&b, "\nconfirm with: knomit fleet sync drop %s --leave-out %s", commit, strings.Join(hashes, ","))
	return errors.New(b.String())
}

// plainLine keeps requester-supplied text from steering the terminal: C0, DEL,
// C1 and the Unicode bidi controls become a space.
func plainLine(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f,
			r == 0x200e, r == 0x200f, r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return ' '
		}
		return r
	}, s)
}

// printFleetSync is the `sync` read: blocked when the fleet's rules hold this
// instance's branch back (last_error_main_held), else the last error if any.
func printFleetSync(out io.Writer, st fleetStatusJSON) {
	if st.LastErrorMainHeld && st.LastErrorMainHeldCommit != "" {
		fmt.Fprintf(out, "sync blocked: the fleet's rules refuse commit %s on this instance's branch; nothing from this instance reaches the fleet until it is dropped. Retrying does not help.\n", short8(st.LastErrorMainHeldCommit))
		fmt.Fprintf(out, "  refused: %s\n", st.LastError)
		fmt.Fprintln(out, "  drop it: knomit fleet sync drop")
		return
	}
	fmt.Fprintln(out, "sync: not blocked")
	if st.LastError != "" {
		fmt.Fprintf(out, "last error (%s): %s\n", st.LastAttempt, st.LastError)
	}
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
	if err := localCall(ctx, hc, method, path, in, &raw); err != nil && err != io.EOF { // identity: only the decoder's bare EOF on an empty 2xx body; a *url.Error wrapping EOF is a dropped connection
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
	LastErrorMainHeld       bool   `json:"last_error_main_held"`
	LastErrorMainHeldCommit string `json:"last_error_main_held_commit"`
	RecordCurrent           *bool  `json:"record_current"`
	RecordPendingUpdate     bool   `json:"record_pending_update"`
	Notice                  string `json:"notice"`
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
			fmt.Fprintln(out, "record: pending (not yet accepted by the fleet (the master has not taken it))")
		default:
			fmt.Fprintf(out, "record: %s (accepted by the fleet)\n", st.RecordState)
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
		fmt.Fprintln(out, "record: pending update (waiting for the fleet's master to accept it)")
	}
	if st.Notice != "" {
		fmt.Fprintf(out, "notice: %s\n", st.Notice)
	}
	if st.LastError != "" {
		fmt.Fprintf(out, "last error (%s): %s\n", st.LastAttempt, st.LastError)
	}
}
