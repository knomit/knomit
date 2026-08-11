package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

func newRegisterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "register",
		Short: "Register an already-built corpus with a live knomit daemon (web UI/API)",
		Long: `register makes an existing corpusgen-built .db visible in a live knomit
daemon's web UI/API without rebuilding it: copies it into the daemon's repos
directory under --name and creates the daemon's expected agent branch on the
copy (pointing at --branch, e.g. "main") — the same step "corpusgen build
--register" runs automatically at the end of a build.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runRegister,
	}
	f := cmd.Flags()
	f.String("db", "", "path to the corpus's core.db (required)")
	f.String("name", "", "repo name to register it under (required)")
	f.String("branch", "main", "branch the corpus's facts were written to")
	f.String("daemon-url", "http://127.0.0.1:19278", "daemon base URL")
	_ = cmd.MarkFlagRequired("db")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func runRegister(cmd *cobra.Command, args []string) error {
	f := cmd.Flags()
	dbPath, _ := f.GetString("db")
	name, _ := f.GetString("name")
	branch, _ := f.GetString("branch")
	daemonURL, _ := f.GetString("daemon-url")

	if err := registerWithDaemon(context.Background(), dbPath, name, branch, daemonURL); err != nil {
		return err
	}
	fmt.Printf("registered: repo %q now visible in the web UI/API\n", name)
	return nil
}
