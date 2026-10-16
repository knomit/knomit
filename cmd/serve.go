package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	"knomit/internal/platform/version"
)

// The exit codes `knomit serve --supervised` documents to its supervisor (the
// desktop). Unsupervised serve exits 0 or 1 only.
//
//	0  stopped (signal or stdin closed)
//	2  configuration or trust failure: restarting cannot help
//	3  the local socket/pipe is held by another server: attach to it instead
//	   anything else: a crash; the supervisor may restart
const (
	ExitSupervisedConfig = 2
	ExitSupervisedHeld   = 3
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
		supervised    bool
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the knomit HTTP server",
		RunE: func(cmd *cobra.Command, args []string) error {
			// ctx is the command's context plus a cancel of our own, so the
			// supervisor's stdin closing stops the server exactly as SIGTERM
			// does: serveUntil returns, the defers run, the exit is 0.
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			ctx, stop := context.WithCancel(ctx)
			defer stop()
			// Started before anything slow (app.New may download models for
			// minutes): a parent that dies during that wait must still end us.
			var stdinClosed atomic.Bool
			if supervised {
				// The supervisor holds our stderr pipe too; its death must
				// not kill us before the stdin-EOF shutdown below runs.
				survivePipedStderrLoss()
				watchStdin(cmd.InOrStdin(), func() {
					if ctx.Err() != nil {
						return // already stopping (or stopped): nothing to report
					}
					stdinClosed.Store(true)
					log.Info().Msg("supervisor closed stdin; shutting down")
					stop()
				})
			}
			// coded gives a supervised failure its documented exit code.
			coded := func(err error) error { return supervisedExit(supervised, err) }

			// loadCfg is the ONE way this process reads its configuration:
			// the loader (defaults, knomit.toml, env) and then the command-
			// line flags, which win. A reload runs it again, so a reload and
			// a boot cannot disagree about what the configuration is.
			loadCfg := func() (config.Config, error) {
				cfg, err := config.Load()
				if err != nil {
					return config.Config{}, err
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
				return cfg, nil
			}
			cfg, err := loadCfg()
			if err != nil {
				return coded(&app.ListenerConfigError{Err: fmt.Errorf("load config: %w", err)})
			}

			// The Logs tab's source. Created before the logger so every line
			// this process writes from here on is in the ring a browser
			// replays on connect; bounded, so it cannot grow. Its context is
			// the command's, so subscriptions end when the server stops.
			logTap := logging.NewTap(ctx, logTapLines)

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

			a, err := app.New(ctx, cfg, serveAppOptions(app.Options{LogTap: logTap}))
			if err != nil {
				if stdinClosed.Load() {
					return nil // asked to stop while still booting
				}
				return err
			}
			defer a.Close()

			router := a.Handler()

			// serveCtx is every server's BaseContext. It is ctx (cancelled by
			// SIGINT/SIGTERM or the supervisor's stdin) plus a cancel of its
			// own, which serveUntil calls before Shutdown. That way SSE
			// handlers, which select on r.Context().Done(), unblock promptly
			// on a listener failure exactly as they do on a signal, and
			// Shutdown does not wait out its grace for them.
			serveCtx, cancelServe := context.WithCancel(ctx)
			defer cancelServe()

			// Every listener is BOUND before "knomit ready" and before any of
			// them serves, in internal/app's listener-set order (TCP, local socket/pipe,
			// require check, mTLS), each step undoing all before it. A taken
			// port or a failed TLS/OAuth setup is then an error RunE returns
			// while nothing is serving yet, and the deferred closes release
			// whatever was already bound. Open takes the WHOLE config:
			// listenerFrom-style pairs of arguments are how knomit#245's
			// mis-wiring went unnoticed.
			//
			// errCh gets one slot per listener Serve below plus room for the
			// listeners a reload binds later; a reload's send never blocks.
			errCh := make(chan error, 16)
			var ls *app.Listeners // set below; the hooks run only after that
			ls, err = app.OpenListeners(serveCtx, app.ListenerOptions{
				Config:     cfg,
				Handler:    router,
				Base:       serveCtx,
				KeyPath:    a.KeyPath(),
				Version:    version.String(),
				Supervised: supervised,
				// A reload binds listeners after boot; they are served here,
				// like the boot ones.
				Serve: func(what string, srv *http.Server, ln net.Listener) {
					serveRetirable(errCh, func(l net.Listener) bool { return ls.Retired(l) }, what, srv, ln)
				},
				OnAddress: a.Manager().SetServerAddress,
			})
			if err != nil {
				return coded(err)
			}
			defer ls.Close()
			srv, tlsSrv, tl := ls.HTTP, ls.TLSServer, ls.TLS
			// The address this server's own children (recipe `exec`) get as
			// KNOMIT_SERVER: the local listener only if it bound.
			a.Manager().SetServerAddress(ls.Address)
			// GET /fleet's host.listening: whether the mTLS listener is UP,
			// not only configured (a reload can start it later).
			a.Manager().SetTLSListener(ls.TLSListening)
			// The one line to copy into KNOMIT_SERVER to reach THIS instance.
			log.Info().Str("knomit_server", ls.Address).Msg("this server's address (KNOMIT_SERVER for kb)")

			// OAuth listener (F19 phase 3a): its OWN http.Server over the
			// OAuth router, off until [oauth].issuer and [oauth].addr are set.
			// The only place a bearer token is judged; a reverse proxy with
			// real TLS fronts it, never the plaintext port above.
			oauthSrv, ol, err := openOAuthServer(cfg.OAuth, a.OAuthHandler(), srv)
			if err != nil {
				if availabilityOnly(supervised, err) {
					// A held address is an availability problem, like TCP and
					// TLS: the desktop must not lose its UI over it.
					log.Warn().Err(err).Msg("oauth listener address is held by another process; serving without it")
					oauthSrv, ol = nil, nil
				} else {
					// Fail closed, as for the TLS listener above.
					log.Error().Err(err).Msg("oauth listener failed")
					return coded(err)
				}
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
				if availabilityOnly(supervised, err) {
					log.Warn().Err(err).Msg("runtime diagnostics address is held by another process; serving without it")
					diagSrv, dl = nil, nil
				} else {
					log.Error().Err(err).Msg("runtime diagnostics listener failed")
					return coded(err)
				}
			}
			if dl != nil {
				defer dl.Close()
			}

			// The reload trigger (SIGHUP; on Windows a named event). It is
			// installed before server.json is written so the file can say
			// "reload": true, but a reload that arrives before the listeners
			// are serving waits for `ready` (it must not rebind listeners this
			// function has yet to serve) and gives up if the server stops
			// first. The deferred stop runs AFTER abort is closed (LIFO), so
			// it never waits on a reload parked at the gate.
			ready := make(chan struct{})
			abort := make(chan struct{})
			stopReload, rerr := installReloadSignal(cfg.Home, func() {
				select {
				case <-ready:
				case <-abort:
					return
				}
				reloadListeners(ls, loadCfg)
			})
			if rerr != nil {
				log.Warn().Err(rerr).Msg("reload trigger not installed; configuration changes need a restart")
			} else {
				ls.SetReloadable(true)
			}
			defer stopReload()
			defer close(abort)

			// server.json, LAST: every listener that can fail is bound, so the
			// file never names a port nothing serves.
			//
			// Failing to write it does not stop a plain `knomit serve`: before
			// the file existed nothing depended on it, and kb falls back to
			// the socket. Supervised, the desktop needs it, and no retry fixes
			// an unwritable state directory: exit 2.
			if err := ls.Advertise(); err != nil {
				if !supervised {
					log.Warn().Err(err).Str("path", ls.LockPath()).
						Msg("server.json not written; kb and the desktop will not discover this server by file")
				} else {
					return coded(&app.ListenerConfigError{Err: err})
				}
			}

			// Startup summary: everything above is bound.
			pubKey := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(a.Signer().PublicKey())))
			startupLog := log.Info()
			if ls.TCP != nil {
				httpAddr := "http://" + ls.TCP.Addr().String()
				startupLog = startupLog.
					Str("http", httpAddr).
					Str("api", httpAddr+"/api/v1/repos/{repo}").
					Str("mcp", httpAddr+"/api/v1/repos/{repo}/branches/{branch}/mcp")
				if cfg.Git.Serve {
					startupLog = startupLog.Str("git_remote", httpAddr+"/git")
				}
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
			// happened (#261). errCh is made before the listeners open, with a
			// slot for each, so no boot send blocks.
			//
			// Each server's Close is deferred AFTER its listeners' closes, so
			// on return (LIFO) the servers stop first and no Serve sees its
			// listener, or the socket file, disappear underneath it. After
			// serveUntil's graceful Shutdown the Closes have nothing to do.
			if ls.TCP != nil {
				serveRetirable(errCh, ls.Retired, "listen", srv, ls.TCP)
			}
			if ls.Local != nil {
				// `via` names the MECHANISM, so the line says which credential
				// a session over it will carry rather than assuming a socket.
				log.Info().Str("socket", cfg.Socket).Str("via", string(auth.LocalVia)).
					Msg("local authenticated listener listening")
				serveInto(errCh, "local authenticated listener", srv, ls.Local)
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

			close(ready) // reloads may run now
			// a.Close() runs via defer — shuts down repos and releases resources.
			return serveUntil(ctx, errCh, cancelServe, 5*time.Second, srv, tlsSrv, oauthSrv, diagSrv)
		},
	}
	cmd.Flags().StringVar(&portOverride, "port", "", "override the listen port (default: from config)")
	cmd.Flags().StringVar(&hostOverride, "host", "", "override the listen host (default: from config)")
	cmd.Flags().StringVar(&logFile, "log-file", "", "path to a file that receives JSON-structured log output (in addition to stderr)")
	cmd.Flags().IntVar(&logMaxSizeMB, "log-max-size", 10, "max log file size in MB before rotation")
	cmd.Flags().IntVar(&logMaxBackups, "log-max-backups", 3, "max number of rotated log files to keep")
	cmd.Flags().IntVar(&logMaxAgeDays, "log-max-age", 7, "max age in days to keep rotated log files")
	cmd.Flags().BoolVar(&supervised, "supervised", false,
		"run as a supervised child (the desktop): stop cleanly when stdin closes; the local socket/pipe is mandatory "+
			"(held by another server: exit 3); a held TCP or TLS address is a warning, not a failure; "+
			"configuration or trust failure exits 2")
	return cmd
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

// serveRetirable is serveInto for a plaintext/TLS listener that a reload may
// close on purpose: retired(ln) reports that, and the resulting Serve error
// ("use of closed network connection") is then a clean stop, not a failure.
// The send never blocks: a reload may start more listeners than errCh had
// slots for at boot.
func serveRetirable(errCh chan<- error, retired func(net.Listener) bool, what string, srv *http.Server, ln net.Listener) {
	go func() {
		err := srv.Serve(ln)
		if err == nil || errors.Is(err, http.ErrServerClosed) || retired(ln) {
			return
		}
		select {
		case errCh <- fmt.Errorf("%s: %w", what, err):
		default:
			log.Error().Err(err).Str("listener", what).Msg("listener failed and the error channel is full")
		}
	}()
}

// reloadListeners is what a reload trigger does: re-read the configuration
// with the boot loader and apply it. It is the ONLY production caller of
// Listeners.Reload/Apply (test/archtest pins that): a reload must never be
// reachable from an HTTP route.
func reloadListeners(ls *app.Listeners, load func() (config.Config, error)) {
	log.Info().Msg("reload requested")
	res, err := ls.Reload(load)
	if err != nil {
		return // logged by Reload; generation unchanged
	}
	for item, why := range res.Failed {
		log.Error().Str("item", item).Str("error", why).Msg("reload: left as it was")
	}
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

// watchStdin reads in until it ends, then calls onEOF once. It is the
// supervised server's lifeline: the desktop holds the write end of the pipe
// that is our stdin, so the pipe closing means the desktop is gone, on every
// OS and however it died. The goroutine is never joined; at process exit it
// dies with the process.
func watchStdin(in io.Reader, onEOF func()) {
	go func() {
		_, _ = io.Copy(io.Discard, in)
		onEOF()
	}()
}

// supervisedExit gives a failure of a supervised serve its documented exit
// code (see ExitSupervisedConfig). Unsupervised, and for any other error, err
// is returned as is and exits 1.
func supervisedExit(supervised bool, err error) error {
	if !supervised || err == nil {
		return err
	}
	var ce *app.ListenerConfigError
	switch {
	case errors.Is(err, auth.ErrSocketInUse):
		return &ExitCodeError{Code: ExitSupervisedHeld, Err: err}
	case errors.As(err, &ce):
		return &ExitCodeError{Code: ExitSupervisedConfig, Err: err}
	}
	return err
}

// availabilityOnly reports whether err is an "address already in use" that a
// supervised serve survives (WARN, serve without that listener). Unsupervised
// serve keeps its refusal.
func availabilityOnly(supervised bool, err error) bool {
	return supervised && app.IsAddrInUse(err)
}
