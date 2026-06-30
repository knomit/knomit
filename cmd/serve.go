package cmd

import (
	"context"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof/* on http.DefaultServeMux
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"gopkg.in/natefinch/lumberjack.v2"

	"knomit/internal/app"
	"knomit/internal/config"
	"knomit/internal/crashdump"
)

func serveCmd() *cobra.Command {
	var (
		portOverride  string
		hostOverride  string
		logFile       string
		logMaxSizeMB  int
		logMaxBackups int
		logMaxAgeDays int
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the knomit HTTP server",
		RunE: func(cmd *cobra.Command, args []string) error {
			// If a log file is requested, tee zerolog output to a rotating
			// file in addition to stderr. The console (stderr) keeps its
			// human-readable ConsoleWriter formatting; the file gets raw
			// JSON so it can be grepped/parsed.
			if logFile != "" {
				rotator := &lumberjack.Logger{
					Filename:   logFile,
					MaxSize:    logMaxSizeMB,
					MaxBackups: logMaxBackups,
					MaxAge:     logMaxAgeDays,
					Compress:   false,
				}
				multi := zerolog.MultiLevelWriter(zerolog.ConsoleWriter{Out: os.Stderr}, rotator)
				log.Logger = log.Output(multi)
				fmt.Fprintf(os.Stderr, "knomit: logging also to %s (max %dMB, %d backups, %dd retention)\n",
					logFile, logMaxSizeMB, logMaxBackups, logMaxAgeDays)
			}

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

			// Crash-report bundles + crash-loop detection (native post-mortem).
			reporter := crashdump.New(filepath.Join(cfg.Home, "crashes"), crashdump.Global)
			defer reporter.Guard("serve") // write a bundle then re-panic on a fatal serve-path panic

			marker := crashdump.NewMarker(filepath.Join(cfg.Home, "running.marker"))
			if crashed, priorStart, merr := marker.Begin(time.Now()); merr != nil {
				log.Warn().Err(merr).Msg("crash marker unavailable")
			} else if crashed {
				log.Warn().Time("prior_start", priorStart).
					Msg("previous run exited uncleanly (possible crash); see crashes/ for any bundle")
			}
			defer marker.End()

			// SIGUSR1 → dump every goroutine to a file WITHOUT exiting, so a
			// stuck-but-live server can be inspected: `kill -USR1 <pid>`.
			usr1 := make(chan os.Signal, 1)
			signal.Notify(usr1, syscall.SIGUSR1)
			defer signal.Stop(usr1)
			go func() {
				for range usr1 {
					if p, derr := crashdump.DumpGoroutines(filepath.Join(cfg.Home, "dumps")); derr != nil {
						log.Warn().Err(derr).Msg("goroutine dump failed")
					} else {
						log.Info().Str("path", p).Msg("goroutine dump written (SIGUSR1)")
					}
				}
			}()

			a, err := app.New(cmd.Context(), cfg, app.Options{})
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
				Str("api", httpAddr+"/api/v1/repos/{repo}").
				Str("mcp", httpAddr+"/api/v1/repos/{repo}/branches/{branch}/mcp")

			if cfg.Git.Serve {
				startupLog = startupLog.Str("git_remote", httpAddr+"/git")
			}

			startupLog.
				Str("public_key", pubKey).
				Str("branch", a.AgentBranch()).
				Strs("repos", a.Manager().Names()).
				Msg("knomit ready")

			// HTTP server.
			// BaseContext propagates cmd.Context() into every request context so
			// that SSE handlers (which select on r.Context().Done()) are unblocked
			// immediately when SIGTERM cancels the command context, allowing
			// Shutdown to return promptly instead of waiting for idle connections.
			srv := &http.Server{
				Addr:              listenAddr,
				Handler:           router,
				ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout:       30 * time.Second,
				WriteTimeout:      0, // 0 = no limit for SSE long-poll
				IdleTimeout:       60 * time.Second,
				BaseContext:       func(_ net.Listener) context.Context { return cmd.Context() },
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
	cmd.Flags().StringVar(&logFile, "log-file", "", "path to a file that receives JSON-structured log output (in addition to stderr)")
	cmd.Flags().IntVar(&logMaxSizeMB, "log-max-size", 10, "max log file size in MB before rotation")
	cmd.Flags().IntVar(&logMaxBackups, "log-max-backups", 3, "max number of rotated log files to keep")
	cmd.Flags().IntVar(&logMaxAgeDays, "log-max-age", 7, "max age in days to keep rotated log files")
	return cmd
}
