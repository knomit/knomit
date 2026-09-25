package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/spf13/cobra"

	"knomit/internal/auth"
	"knomit/internal/config"
)

// grantsCmd is `knomit grants`: the operator's view of the control.db
// grants table — what a principal may do on THIS instance beyond what it
// holds implicitly (anonymous loopback from [auth].loopback_default, a
// chained instance's read).
func grantsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "grants",
		Short: "Add, revoke and list per-principal permissions on this instance",
	}
	var by string
	add := &cobra.Command{
		Use:   "add <principal> <permission>",
		Short: "Grant a permission, e.g. `knomit grants add instance:<64-hex>@cert write`",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withGrants(func(g *auth.SQLGrants) error {
				return grantsAdd(cmd.Context(), cmd.OutOrStdout(), g, args[0], args[1], by)
			})
		},
	}
	add.Flags().StringVar(&by, "by", "", "who granted it, recorded in the row (default: $USER)")
	revoke := &cobra.Command{
		Use:   "revoke <principal> <permission>",
		Short: "Revoke a permission (the row stays as history)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withGrants(func(g *auth.SQLGrants) error {
				return grantsRevoke(cmd.Context(), cmd.OutOrStdout(), g, args[0], args[1])
			})
		},
	}
	list := &cobra.Command{
		Use:   "list [<principal>]",
		Short: "List grants rows, live and revoked",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := ""
			if len(args) == 1 {
				p = args[0]
			}
			return withGrants(func(g *auth.SQLGrants) error {
				return grantsList(cmd.Context(), cmd.OutOrStdout(), g, p)
			})
		},
	}
	c.AddCommand(add, revoke, list)
	return c
}

// withGrants opens THIS home's control.db and hands its grants table to fn.
// It never migrates: a home whose control.db has no grants table has not been
// served by a phase-1 build yet, and creating schema from a CLI is how a home
// gets half-converted (see repos.controlUp). The operator starts
// `knomit serve` once instead. SQLite WAL plus a busy timeout lets this run
// beside a live server.
func withGrants(fn func(*auth.SQLGrants) error) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	return withGrantsAt(cfg, fn)
}

func withGrantsAt(cfg config.Config, fn func(*auth.SQLGrants) error) error {
	path := filepath.Join(cfg.Home, "control.db")
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%s: %w; start `knomit serve` once on this home first", path, err)
	}
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return err
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'grants'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%s has no grants table; start `knomit serve` once with this build to migrate it", path)
	}
	return fn(auth.NewSQLGrants(db))
}

func parseGrantArgs(principal, permission string) (auth.Principal, auth.Permission, error) {
	p, err := auth.ParsePrincipal(principal)
	if err != nil {
		return auth.Principal{}, "", err
	}
	set, err := auth.ParseSet([]string{permission})
	if err != nil {
		return auth.Principal{}, "", err
	}
	var perm auth.Permission
	for k := range set {
		perm = k
	}
	// A certificate principal's id is the FULL 64-hex fingerprint. The 8-hex
	// short form (what the branch name carries) names nobody: a row for it
	// could never match a request, and `grants add` would report success.
	if p.Via == auth.ViaCert && !isFingerprint(p.ID) {
		return auth.Principal{}, "", fmt.Errorf("%s: a certificate principal's id is the full 64-hex fingerprint (`knomit identity show` prints it as \"fingerprint:\"), not %q", principal, p.ID)
	}
	if p.Kind == auth.KindAnonymous {
		// loopbackGrants answers anonymous from config and never reads the
		// store, so a row here would silently do nothing.
		return auth.Principal{}, "", errors.New("the anonymous principal's permissions are [auth].loopback_default in knomit.toml, not grants rows")
	}
	return p, perm, nil
}

func grantsAdd(ctx context.Context, out io.Writer, g *auth.SQLGrants, principal, permission, by string) error {
	p, perm, err := parseGrantArgs(principal, permission)
	if err != nil {
		return err
	}
	if by == "" {
		by = os.Getenv("USER")
	}
	if err := g.Grant(ctx, p, perm, by); err != nil {
		return err
	}
	fmt.Fprintf(out, "granted %s to %s\n", perm, p)
	return nil
}

func grantsRevoke(ctx context.Context, out io.Writer, g *auth.SQLGrants, principal, permission string) error {
	p, perm, err := parseGrantArgs(principal, permission)
	if err != nil {
		return err
	}
	// Parsed, not globbed: the refusal keys on the Kind and Via the string
	// PARSES to, which is exactly what auth.CertGrants keys on.
	if perm == auth.Read && p.Kind == auth.KindInstance && p.Via == auth.ViaCert {
		return errors.New("read is implicit for enrolled instances (the certificate chain is admission); to remove an instance, revoke its certificate with `knomit identity revoke`")
	}
	if err := g.Revoke(ctx, p, perm); err != nil {
		return err
	}
	fmt.Fprintf(out, "revoked %s from %s\n", perm, p)
	return nil
}

func grantsList(ctx context.Context, out io.Writer, g *auth.SQLGrants, principal string) error {
	if principal != "" {
		if _, err := auth.ParsePrincipal(principal); err != nil {
			return err
		}
	}
	rows, err := g.List(ctx, principal)
	if err != nil {
		return err
	}
	for _, r := range rows {
		state := "live"
		if r.RevokedAt != nil {
			state = "revoked " + r.RevokedAt.Format(time.RFC3339)
		}
		fmt.Fprintf(out, "%s\t%s\t%s\tby=%s\tat=%s\n", r.Principal, r.Permission, state, r.GrantedBy, r.GrantedAt.Format(time.RFC3339))
	}
	return nil
}

// isFingerprint reports whether s has pki.Fingerprint's shape: 64 lowercase hex.
func isFingerprint(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
