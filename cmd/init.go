package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/identity"
	"knomit/internal/store"
)

func initCmd() *cobra.Command {
	var ontologyPath string
	var repoName string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialise a new knomit repo",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			reposDir := filepath.Join(cfg.Home, "repos")
			if err := os.MkdirAll(reposDir, 0o755); err != nil {
				return err
			}

			// Ensure SSH keypair exists.
			keyPath := cfg.Remote.SSHKey
			if keyPath == "" {
				keyPath = filepath.Join(cfg.Home, "id_ed25519")
			}
			_, keyFingerprint, err := identity.EnsureKeyPair(keyPath)
			if err != nil {
				return fmt.Errorf("ensure keypair: %w", err)
			}
			agentBranch := identity.AgentBranch(keyFingerprint)

			// Load ontology: custom file or embedded default.
			ontology := fact.DefaultOntology()
			if ontologyPath != "" {
				data, err := os.ReadFile(ontologyPath)
				if err != nil {
					return fmt.Errorf("read ontology file: %w", err)
				}
				ontology, err = fact.ParseOntology(data)
				if err != nil {
					return fmt.Errorf("parse ontology: %w", err)
				}
			}
			ontologyYAML, err := ontology.Serialize()
			if err != nil {
				return fmt.Errorf("serialize ontology: %w", err)
			}

			dbPath := filepath.Join(reposDir, repoName+".db")
			svc, err := store.Open(dbPath)
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			defer svc.Close()

			initFiles := map[string]string{
				"domains/ontology.yaml": string(ontologyYAML),
			}
			if err := svc.InitRepo(initFiles, agentBranch); err != nil {
				return fmt.Errorf("init git: %w", err)
			}
			fmt.Printf("Initialized knomit repo %q at %s\n", repoName, dbPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&ontologyPath, "ontology", "", "path to custom ontology YAML file")
	cmd.Flags().StringVar(&repoName, "name", "knomit", "repo name")
	return cmd
}
