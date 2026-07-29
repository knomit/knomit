package cmd

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"knomit/internal/app"
	"knomit/internal/config"
	"knomit/internal/crashdump"
	"knomit/internal/runtimeobs"
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

			// Logging flags override config/env (flags win). The rotating-file
			// flags only override when explicitly set, so they don't clobber a
			// configured value with their own defaults.
			if logFile != "" {
				cfg.Log.File = logFile
			}
			if cmd.Flags().Changed("log-max-size") {
				cfg.Log.MaxSizeMB = logMaxSizeMB
			}
			if cmd.Flags().Changed("log-max-backups") {
				cfg.Log.MaxBackups = logMaxBackups
			}
			if cmd.Flags().Changed("log-max-age") {
				cfg.Log.MaxAgeDays = logMaxAgeDays
			}

			// Reconfigure the logger from config (main set a console base);
			// keep tee'ing through the crash ring so reports retain the log tail.
			lg, lvl, err := buildLogger(cfg.Log, os.Stderr, os.Stdout, crashdump.Global)
			if err != nil {
				return fmt.Errorf("configure logging: %w", err)
			}
			zerolog.SetGlobalLevel(lvl)
			log.Logger = lg
			if cfg.Log.File != "" {
				log.Info().Str("file", cfg.Log.File).Int("max_mb", cfg.Log.MaxSizeMB).
					Int("backups", cfg.Log.MaxBackups).Int("max_age_days", cfg.Log.MaxAgeDays).
					Msg("logging to rotating file")
			}

			// Optionally persist fd 2 so runtime/CGO (ONNX) fatal tracebacks —
			// which bypass the logger and write straight to stderr — survive on
			// disk. Daemon-only; containers leave KNOMIT_CRASH_LOG unset.
			if cfg.Log.CrashFile != "" {
				if cf, cerr := crashdump.RedirectStderr(cfg.Log.CrashFile); cerr != nil {
					log.Warn().Err(cerr).Str("file", cfg.Log.CrashFile).Msg("could not redirect stderr to crash log")
				} else {
					defer cf.Close()
					log.Info().Str("file", cfg.Log.CrashFile).Msg("fatal tracebacks (incl. CGO) persisted to crash log")
				}
			}

			// Crash-report bundles + crash-loop detection (native post-mortem).
			reporter := crashdump.New(filepath.Join(cfg.Home, "crashes"), crashdump.Global)
			crashdump.SetGlobalReporter(reporter) // recovered HTTP/task panics report here too
			defer reporter.Guard("serve")         // write a bundle then re-panic on a fatal serve-path panic

			marker := crashdump.NewMarker(filepath.Join(cfg.Home, "running.marker"))
			if crashed, priorStart, merr := marker.Begin(time.Now()); merr != nil {
				log.Warn().Err(merr).Msg("crash marker unavailable")
			} else if crashed {
				log.Warn().Time("prior_start", priorStart).
					Msg("previous run exited uncleanly (possible crash); see crashes/ for any bundle")
			}
			// EndUnlessPanicking (not End): on a panic unwind the marker is left
			// in place so the next boot detects the unclean exit — an
			// unconditional End() would run before reporter.Guard re-panics and
			// erase the crash-loop signal.
			defer marker.EndUnlessPanicking()

			// On unix, SIGUSR1 dumps every goroutine to a file WITHOUT exiting,
			// so a stuck-but-live server can be inspected: `kill -USR1 <pid>`.
			stopDumps := installGoroutineDumpSignal(filepath.Join(cfg.Home, "dumps"))
			defer stopDumps()

			// Bootstrap BEFORE app.New, always. It restores control.db and every
			// registered repo database from the replica, and restore refuses to
			// overwrite a file that exists — so any database app.New opens
			// first is a restore that silently does nothing, leaving empty
			// state that replication then writes over the good backup.
			boot, err := app.Bootstrap(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			if boot.Backup != nil {
				// context.Background(), not cmd.Context(): shutdown runs after
				// SIGTERM has already cancelled the command context, and each
				// tracked database performs a FINAL replica sync on close. A
				// cancelled context would abort exactly the sync that makes the
				// backup current as of shutdown.
				defer boot.Backup.Close(context.Background())
			}

			a, err := app.New(cmd.Context(), cfg, boot, app.Options{})
			if err != nil {
				return err
			}
			defer a.Close()

			// Replicate what actually opened. This has to follow Manager.Start
			// (inside app.New): Start is what reconciles the registry against
			// the disk, so before it runs there is no truthful answer to "which
			// databases are live".
			if boot.Backup != nil {
				controlPath := filepath.Join(cfg.Home, "control.db")
				if err := boot.Backup.Track("control", controlPath); err != nil {
					return fmt.Errorf("track control.db: %w", err)
				}
				for name, dbPath := range a.Manager().OpenDBPaths() {
					if err := boot.Backup.Track(name, dbPath); err != nil {
						return fmt.Errorf("track %s: %w", name, err)
					}
				}
				// Archived databases too, under the retention-disabled archive
				// namespace. Without this an archive is replicated only for the
				// lifetime of the process that created it: after a restart
				// nothing tracks it, so Purge's untrack is a permanent no-op.
				// Archives whose database is NOT on this volume are skipped here
				// and fetched from the replica on unarchive instead.
				archived, aerr := a.Manager().ArchivedDBPaths()
				if aerr != nil {
					return fmt.Errorf("list archived repos for replication: %w", aerr)
				}
				for id, dbPath := range archived {
					if err := boot.Backup.TrackArchived(id, dbPath); err != nil {
						return fmt.Errorf("track archived %s: %w", id, err)
					}
				}
			}

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

			names := a.Manager().Names()
			startupLog.
				Str("public_key", pubKey).
				Str("branch", a.AgentBranch()).
				Strs("repos", names).
				Msg("knomit ready")
			if len(names) == 0 {
				// Zero repos is valid — knomit creates none on its own — but an
				// empty server looks broken unless we say what to do about it.
				log.Info().Str("web", httpAddr).
					Msg("no repositories yet; create one in the web UI or with POST /api/v1/repos")
			}

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

			// Runtime diagnostics port (localhost only, off unless configured):
			// /runtime/* controls + /debug/pprof + /debug/vars + /metrics.
			if cfg.Runtime.Addr != "" {
				rt := runtimeobs.NewServer(runtimeobs.Options{
					StartedAt:   time.Now(),
					HeapDumpDir: filepath.Join(cfg.Home, "dumps"),
					StatusExtra: func() map[string]any {
						return map[string]any{
							"repos":     a.Manager().Names(),
							"read_only": cfg.ReadOnly,
							"branch":    a.AgentBranch(),
						}
					},
				})
				rtSrv := &http.Server{
					Addr:              cfg.Runtime.Addr,
					Handler:           rt.Handler(),
					ReadHeaderTimeout: 10 * time.Second,
				}
				go func() {
					log.Info().Str("runtime", "http://"+cfg.Runtime.Addr+"/runtime/status").
						Str("pprof", "http://"+cfg.Runtime.Addr+"/debug/pprof/").
						Str("metrics", "http://"+cfg.Runtime.Addr+"/metrics").
						Msg("runtime diagnostics port listening")
					if err := rtSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
						log.Warn().Err(err).Msg("runtime diagnostics server failed")
					}
				}()
				defer func() {
					shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					_ = rtSrv.Shutdown(shutCtx)
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
