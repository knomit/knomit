package cmd

import (
	"context"
	"errors"
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
	"knomit/internal/platform/privdir"
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

			// The data root is private before the first thing below writes
			// into it: the crash log redirect, the crash reporter and the
			// running marker all MkdirAll 0755, and whichever ran first used
			// to decide the root's mode. Right after the logger, so what
			// Ensure reports lands in the configured log; before the first
			// log line, which could create a log file under the root.
			// app.New calls it again as the backstop for other entry points.
			if err := privdir.Ensure(cfg.Home); err != nil {
				return fmt.Errorf("data root %s: %w", cfg.Home, err)
			}
			if ignored := config.IgnoredExecutableConfig(cfg.Home); ignored != "" {
				log.Warn().Str("ignored", ignored).Str("read", filepath.Join(cfg.Home, "knomit.toml")).
					Msg("knomit.toml beside the executable is not read; move its settings into the data root's knomit.toml")
			}
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

			a, err := app.New(cmd.Context(), cfg, serveAppOptions(app.Options{LogTap: logTap}))
			if err != nil {
				return err
			}
			defer a.Close()

			router := a.Handler()

			// Every listener is BOUND before "knomit ready" and before any of
			// them serves. A taken port or a failed TLS/OAuth setup is then an
			// error RunE returns while nothing is serving yet, and the deferred
			// closes below release whatever was already bound.
			listenAddr := cfg.Host + ":" + cfg.Port
			ln, err := listenTCP(listenAddr)
			if err != nil {
				log.Error().Err(err).Msg("listen failed")
				return err
			}
			defer ln.Close()

			// serveCtx is every server's BaseContext. It is cmd.Context()
			// (cancelled by SIGINT/SIGTERM) plus a cancel of its own, which
			// serveUntil calls before Shutdown. That way SSE handlers, which
			// select on r.Context().Done(), unblock promptly on a listener
			// failure exactly as they do on a signal, and Shutdown does not
			// wait out its grace for them.
			serveCtx, cancelServe := context.WithCancel(cmd.Context())
			defer cancelServe()

			// HTTP server. The TLS and OAuth servers copy its timeouts and
			// BaseContext.
			srv := &http.Server{
				Addr:              listenAddr,
				Handler:           router,
				ReadHeaderTimeout: 10 * time.Second,
				ReadTimeout:       30 * time.Second,
				WriteTimeout:      0, // 0 = no limit for SSE long-poll
				IdleTimeout:       60 * time.Second,
				BaseContext:       func(_ net.Listener) context.Context { return serveCtx },
				// ConnContext runs once per accepted connection, which is the
				// only moment the net.Conn exists: it asks the kernel who is on
				// the other end of a unix socket and puts the answer where
				// internal/web's AuthMiddleware can read it. TCP connections are
				// left untouched.
				ConnContext: auth.ConnContext,
			}

			// Local authenticated listener: a unix socket, or a named pipe on
			// Windows. openLocalListener takes the WHOLE config rather than a
			// path and a flag, so there is no pair of arguments here to wire up
			// the wrong way round, and its policy is pinned by a test.
			ul, closeSocket, err := openLocalListener(cfg)
			if err != nil {
				return err
			}
			defer closeSocket()

			// mTLS listener for enrolled instances (F19 phase 2): its OWN
			// http.Server over the same handler, off until [tls].addr is set
			// and `knomit identity install` has placed a certificate. The
			// plaintext listener above is unchanged. serveCtx also bounds its
			// certificate re-check loop.
			tlsSrv, tl, err := app.OpenTLSServer(serveCtx, cfg.TLS, a.KeyPath(), srv)
			if err != nil {
				// Fail closed: serve does not run without a listener it was
				// configured to open. The returned error exits 1 through
				// main.go, after RunE's defers have run.
				log.Error().Err(err).Str("dir", cfg.TLS.Dir).Msg("tls listener failed")
				return err
			}
			if tl != nil {
				defer tl.Close()
			}

			// OAuth listener (F19 phase 3a): its OWN http.Server over the
			// OAuth router, off until [oauth].issuer and [oauth].addr are set.
			// The only place a bearer token is judged; a reverse proxy with
			// real TLS fronts it, never the plaintext port above.
			oauthSrv, ol, err := openOAuthServer(cfg.OAuth, a.OAuthHandler(), srv)
			if err != nil {
				// Fail closed, as for the TLS listener above.
				log.Error().Err(err).Msg("oauth listener failed")
				return err
			}
			if ol != nil {
				defer ol.Close()
			}

			// Runtime diagnostics port (#288): /runtime/* controls, pprof,
			// expvar and /metrics, off until [runtime].addr is set. Bound
			// here with the others so a taken port fails the boot, instead
			// of a warning from a goroutine after "knomit ready".
			diagSrv, dl, err := openDiagServer(cfg, srv, func() map[string]any {
				return map[string]any{
					"repos":     a.Manager().Names(),
					"read_only": cfg.ReadOnly,
					"branch":    a.AgentBranch(),
				}
			})
			if err != nil {
				log.Error().Err(err).Msg("runtime diagnostics listener failed")
				return err
			}
			if dl != nil {
				defer dl.Close()
			}

			// Startup summary: everything above is bound.
			pubKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(a.Signer().PublicKey())))
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

			// Every listener's Serve reports into errCh and serveUntil returns
			// the first failure, instead of the failure ending the process:
			// an exit skips RunE's defers, and the crash marker's release is
			// one of them, so the next boot would report a crash that never
			// happened (#261). One slot per listener below, so no send blocks.
			//
			// Each server's Close is deferred AFTER its listeners' closes, so
			// on return (LIFO) the servers stop first and no Serve sees its
			// listener, or the socket file, disappear underneath it. After
			// serveUntil's graceful Shutdown the Closes have nothing to do.
			errCh := make(chan error, 5)
			serveInto(errCh, "listen", srv, ln)
			if ul != nil {
				// `via` names the MECHANISM, so the line says which credential
				// a session over it will carry rather than assuming a socket.
				log.Info().Str("socket", cfg.Socket).Str("via", string(auth.LocalVia)).
					Msg("local authenticated listener listening")
				serveInto(errCh, "local authenticated listener", srv, ul)
			}
			defer srv.Close()
			if tlsSrv != nil {
				log.Info().Str("tls", "https://"+tl.Addr().String()).Str("dir", cfg.TLS.Dir).Msg("mTLS listener for enrolled instances")
				serveInto(errCh, "tls listener", tlsSrv, tl)
				defer tlsSrv.Close()
			}
			if oauthSrv != nil {
				log.Info().Str("addr", ol.Addr().String()).Str("issuer", cfg.OAuth.Issuer).
					Msg("OAuth listener (bearer tokens only)")
				serveInto(errCh, "oauth listener", oauthSrv, ol)
				defer oauthSrv.Close()
			}
			if diagSrv != nil {
				base := "http://" + dl.Addr().String()
				log.Info().Str("runtime", base+"/runtime/status").
					Str("pprof", base+"/debug/pprof/").
					Str("metrics", base+"/metrics").
					Bool("allow_remote", cfg.Runtime.AllowRemote).
					Msg("runtime diagnostics listener")
				serveInto(errCh, "runtime diagnostics listener", diagSrv, dl)
				defer diagSrv.Close()
			}

			// a.Close() runs via defer — shuts down repos and releases resources.
			return serveUntil(cmd.Context(), errCh, cancelServe, 5*time.Second, srv, tlsSrv, oauthSrv, diagSrv)
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

// listenTCP binds the plaintext listener. It is separate from http.Server's
// ListenAndServe so serve can bind synchronously, before it reports ready, and
// return the error.
func listenTCP(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("plaintext listener: %w", err) // err names the address
	}
	return ln, nil
}

// openDiagServer binds the runtime diagnostics listener when [runtime].addr is
// set, and returns nil, nil, nil when it is not. config.Validate has already
// refused a non-loopback addr unless [runtime].allow_remote.
//
// Its http.Server copies like's read and idle timeouts and BaseContext, so the
// one cancel in serveUntil also ends a running /debug/pprof/profile or trace.
// No ConnContext: the port has no principal, and a TCP peer address is not a
// credential. The guard (diag.Server.guard) admits the Host names the main
// listener's AuthMiddleware admits, plus the runtime addr's own name.
func openDiagServer(cfg config.Config, like *http.Server, statusExtra func() map[string]any) (*http.Server, net.Listener, error) {
	if cfg.Runtime.Addr == "" {
		return nil, nil, nil
	}
	hosts := cfg.Auth.EffectiveLoopbackHosts(cfg.Host)
	if h, _, err := net.SplitHostPort(cfg.Runtime.Addr); err == nil {
		hosts = append(hosts, config.AuthConfig{}.EffectiveLoopbackHosts(h)...)
	}
	rt := diag.NewServer(diag.Options{
		StartedAt:        time.Now(),
		HeapDumpDir:      filepath.Join(cfg.Home, "dumps"),
		StatusExtra:      statusExtra,
		LoopbackHosts:    hosts,
		AllowRemotePeers: cfg.Runtime.AllowRemote,
	})
	ln, err := net.Listen("tcp", cfg.Runtime.Addr)
	if err != nil {
		return nil, nil, fmt.Errorf("runtime diagnostics listener: %w", err) // err names the address
	}
	return &http.Server{
		Addr:              cfg.Runtime.Addr,
		Handler:           rt.Handler(),
		ReadHeaderTimeout: like.ReadHeaderTimeout,
		ReadTimeout:       like.ReadTimeout,
		IdleTimeout:       like.IdleTimeout,
		// WriteTimeout stays 0 (no limit), deliberately NOT copied from like.
		// /debug/pprof/profile and /trace would survive a limit — net/http/pprof
		// pushes the write deadline out by ?seconds= itself, as it does for
		// any delta profile — but the snapshot dumps (heap, allocs,
		// goroutine?debug=2 without ?seconds=) get no such extension, and a
		// large process's dump would be cut off mid-write.
		WriteTimeout: 0,
		BaseContext:  like.BaseContext,
	}, ln, nil
}

// serveInto runs srv.Serve(ln) in a goroutine and sends any failure to errCh,
// named by what. http.ErrServerClosed is what Serve returns once Shutdown or
// Close has been called, so it is a clean stop and is never sent.
func serveInto(errCh chan<- error, what string, srv *http.Server, ln net.Listener) {
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("%s: %w", what, err)
		}
	}()
}

// serveUntil blocks until ctx is done (SIGINT/SIGTERM) or a listener fails,
// then calls cancelServe — the cancel of every server's BaseContext, so SSE
// handlers unblock on a listener failure as they do on a signal — and shuts
// the servers down within grace. others (nil entries skipped) go first and
// their Shutdown errors are ignored; primary's is returned, joined with the
// listener failure that ended the wait, if any.
func serveUntil(ctx context.Context, errCh <-chan error, cancelServe context.CancelFunc, grace time.Duration, primary *http.Server, others ...*http.Server) error {
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errCh:
		log.Error().Err(serveErr).Msg("listener failed; shutting down")
	}
	cancelServe()
	shutCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	for _, s := range others {
		if s != nil {
			_ = s.Shutdown(shutCtx)
		}
	}
	return errors.Join(serveErr, primary.Shutdown(shutCtx))
}

// serveAppOptions is the identity in production. It exists only so a test
// can run serve's whole RunE with a deterministic embedder in place of the
// ONNX one (app.Options.Embedder, the seam internal/app's wiring tests use):
// the listener failures below are pinned against RunE's returned error,
// which is the #261 contract, not against a helper beside it.
var serveAppOptions = func(o app.Options) app.Options { return o }
