package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{Use: "knomit", Short: "Git-backed knowledge base"}
	root.AddCommand(serveCmd())
	root.AddCommand(initCmd())
	root.AddCommand(rebuildCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Start the knomit HTTP server",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("serve: not yet implemented")
			return nil
		},
	}
}

func initCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Initialise a new knomit repo",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("init: not yet implemented")
			return nil
		},
	}
}

func rebuildCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rebuild",
		Short: "Rebuild the search index",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("rebuild: not yet implemented")
			return nil
		},
	}
}
