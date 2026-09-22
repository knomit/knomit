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
	"knomit/internal/auth"
	"knomit/internal/config"
	"knomit/internal/platform/crashdump"
	"knomit/internal/platform/diag"
	"knomit/internal/platform/logging"
)

// logTapLines is how many recent log lines the server keeps for the Manage
// Logs tab to replay when a browser connects.
//
// 2000 is a few minutes of an ordinary server and comfortably under the
// viewer's own 5000-line scrollback cap, so a fresh tab never receives more
// backlog than it can show. It is memory the process holds for the whole run,
// which is why it is a fixed modest number rather than a config key nobody
// would tune.
const logTapLines = 2000

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

			// The Logs tab's source. Created before the logger so every line
			// this process writes from here on is in the ring a browser
			// replays on connect; bounded, so it cannot grow. Its context is
			// the command's, so subscriptions end when the server stops.
			logTap := logging.NewTap(cmd.Context(), logTapLines)

			// Reconfigure the logger from config (main set a console base);
			// keep tee'ing through the crash ring so reports retain the log tail.
			// logTap.Writer() and not logTap: a bare writer here would receive
			// zerolog's raw JSON, and the Logs viewer parses the console shape.
			lg, lvl, err := logging.Build(app.LoggingOptions(cfg.Log), os.Stderr, os.Stdout, crashdump.Global, logTap.Writer())
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

			a, err := app.New(cmd.Context(), cfg, app.Options{LogTap: logTap})
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
				// ConnContext runs once per accepted connection, which is the
				// only moment the net.Conn exists: it asks the kernel who is on
				// the other end of a unix socket and puts the answer where
				// internal/web's AuthMiddleware can read it. TCP connections are
				// left untouched.
				ConnContext: auth.ConnContext,
			}

			go func() {
				if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					log.Fatal().Err(err).Msg("listen failed")
				}
			}()

			// Runtime diagnostics port (localhost only, off unless configured):
			// /runtime/* controls + /debug/pprof + /debug/vars + /metrics.
			if cfg.Runtime.Addr != "" {
				rt := diag.NewServer(diag.Options{
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
				// 0600 on the socket, with the 0700 data root above it, IS the
				// credential: the kernel vouches for the peer uid, and the file
				// mode decides which uids can reach the socket at all. A
				// world-writable socket would let any local user be taken for
				// this one.
				if err := os.Chmod(cfg.Socket, 0o600); err != nil {
					log.Fatal().Err(err).Str("socket", cfg.Socket).Msg("chmod socket failed")
				}
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
