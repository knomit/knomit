package cmd

import (
	"context"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof/* on http.DefaultServeMux
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"knomit/internal/app"
	"knomit/internal/config"
)

func serveCmd() *cobra.Command {
	var (
		portOverride string
		hostOverride string
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the knomit HTTP server",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			if portOverride != "" {
				cfg.Port = portOverride
			}
			if hostOverride != "" {
				cfg.Host = hostOverride
			}

			a, err := app.New(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			defer a.Close()

			router := a.Handler()

			// Startup summary.
			pubKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(a.Signer().PublicKey())))
			listenAddr := cfg.Host + ":" + cfg.Port
			httpAddr := "http://" + listenAddr

			startupLog := log.Info().
				Str("http", httpAddr).
				Str("api", httpAddr+"/api/v1/{repo}").
				Str("mcp", httpAddr+"/api/v1/{repo}/mcp")

			if cfg.Git.Serve {
				startupLog = startupLog.Str("git_remote", httpAddr+"/git")
			}

			startupLog.
				Str("public_key", pubKey).
				Str("branch", a.AgentBranch()).
				Strs("repos", a.Manager().Names()).
				Msg("knomit ready")

			// HTTP server.
			srv := &http.Server{
				Addr:              listenAddr,
				Handler:           router,
				ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout:       30 * time.Second,
				WriteTimeout:      0, // 0 = no limit for SSE long-poll
				IdleTimeout:       60 * time.Second,
			}

			go func() {
				if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.Fatal().Err(err).Msg("listen failed")
				}
			}()

			// pprof debug server (localhost only).
			if debugAddr := os.Getenv("KNOMIT_PPROF_ADDR"); debugAddr != "" {
				go func() {
					log.Info().Str("pprof", "http://"+debugAddr+"/debug/pprof/").Msg("pprof listening")
					if err := http.ListenAndServe(debugAddr, nil); err != nil {
						log.Warn().Err(err).Msg("pprof server failed")
					}
				}()
			}

			// Optional Unix socket listener.
			if cfg.Socket != "" {
				_ = os.Remove(cfg.Socket) // clean up stale socket
				ul, err := net.Listen("unix", cfg.Socket)
				if err != nil {
					log.Fatal().Err(err).Str("socket", cfg.Socket).Msg("unix socket listen failed")
				}
				defer ul.Close()
				defer os.Remove(cfg.Socket)
				log.Info().Str("socket", cfg.Socket).Msg("unix socket listening")
				go func() {
					if err := srv.Serve(ul); err != nil && err != http.ErrServerClosed {
						log.Fatal().Err(err).Msg("unix socket serve failed")
					}
				}()
			}

			<-cmd.Context().Done()
			// a.Close() runs via defer — shuts down repos and releases resources.
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return srv.Shutdown(shutCtx)
		},
	}
	cmd.Flags().StringVar(&portOverride, "port", "", "override the listen port (default: from config)")
	cmd.Flags().StringVar(&hostOverride, "host", "", "override the listen host (default: from config)")
	return cmd
}
