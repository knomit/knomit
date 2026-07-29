package backup

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/backupproto"
)

// errAgentDown means the request never reached a live agent, or the agent died
// with it in flight. It is the ONLY error class call() retries: every protocol
// method is idempotent, so replaying one against the restarted agent is
// correct, whereas replaying a method that failed on its merits is not.
var errAgentDown = errors.New("backup agent is not running")

// errManagerClosed means the manager has shut down and no agent will be
// started again.
var errManagerClosed = errors.New("backup manager is closed")

const (
	// agentWaitTimeout bounds how long a call waits for a restarting agent
	// before failing. It is generous relative to a restart (spawn plus an open
	// probe) and short relative to a human noticing, so a persistently broken
	// agent surfaces as failing calls rather than a hung server.
	agentWaitTimeout = 30 * time.Second
	// callAttempts is how many times a call may be replayed across agent
	// restarts before giving up.
	callAttempts = 3
	// restartBackoffMin/Max bound the respawn delay after an unexpected exit.
	restartBackoffMin = 100 * time.Millisecond
	restartBackoffMax = 5 * time.Second
)

// shutdownGrace is how long the agent gets to exit after its stdin closes
// before it is killed. A clean exit runs a final replica sync per database with
// retry (litestream's ShutdownSyncTimeout is 30s by default), and that sync is
// the whole point of a graceful shutdown — so this is bounded well past it
// rather than tightly.
//
// It is a var, with defaultMethodBudget and methodBudget below, only so tests
// can shrink the numbers and exercise the real bounding logic in seconds
// instead of minutes. Nothing in production reassigns them.
var shutdownGrace = 45 * time.Second

// errAgentUnresponsive means the agent ACCEPTED a request and never answered
// it within the time that method is allowed.
//
// It is distinct from errAgentDown, and deliberately NOT retried: a dead agent
// is replaced and the call replayed, but a deaf one would simply consume the
// budget again, and three times the wait is not three times the chance.
var errAgentUnresponsive = errors.New("backup agent accepted a request and did not answer it")

// Every round trip is bounded. Without this, one agent that accepts a request
// and goes deaf hangs its caller forever — and because Track, Untrack and
// Pause hold opMu across the call, that one wedged request freezes every
// mutation for EVERY database, while Close (whose own request is a round trip)
// never reaches the kill that would end it. A hung shutdown then leaves an
// orphan agent replicating to the prefix the next knomit will claim.
//
// The budgets are per METHOD rather than one global number, because the honest
// answers differ by two orders of magnitude and a single value would either
// abort legitimate work or fail to bound anything. Each is derived from what
// the operation can legitimately take, with headroom:
//
//   - open: a real round-trip probe against the object store.
//   - untrack: closes the database, whose final replica sync RETRIES for up to
//     litestream's ShutdownSyncTimeout (30s by default).
//   - status: one remote LIST per tracked database, drained in full.
//   - restore: downloads and replays an entire database. This is the one that
//     can legitimately run for many minutes on a large repo over a slow link,
//     so its budget is generous — but it is still a budget, because a boot that
//     hangs forever is not better than a boot that fails.
//   - delete_replica: paginated DELETE over one archive's whole prefix.
var methodBudget = map[string]time.Duration{
	backupproto.MethodOpen:            2 * time.Minute,
	backupproto.MethodTrack:           2 * time.Minute,
	backupproto.MethodUntrack:         2 * time.Minute,
	backupproto.MethodStatus:          2 * time.Minute,
	backupproto.MethodRestore:         30 * time.Minute,
	backupproto.MethodPreflight:       5 * time.Minute,
	backupproto.MethodResetLocalState: 1 * time.Minute,
	backupproto.MethodDeleteReplica:   10 * time.Minute,
	backupproto.MethodClose:           45 * time.Second,
}

// defaultMethodBudget bounds a method the table above does not name, so a
// method added without a budget still gets one rather than none.
var defaultMethodBudget = 2 * time.Minute

// budgetFor returns the round-trip budget for a method.
func budgetFor(method string) time.Duration {
	if d, ok := methodBudget[method]; ok {
		return d
	}
	return defaultMethodBudget
}

// conn is one generation of the agent child process: its pipes, its in-flight
// requests, and its death.
//
// A generation is never reused. When the process exits, every waiter on it
// fails with errAgentDown and the supervisor starts a new one — nothing is
// carried across, because the agent holds its tracked set in memory and the
// client's own record is the only thing that survives.
type conn struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser

	// writeMu serialises whole request lines onto the pipe. Two interleaved
	// partial writes would produce a line the agent cannot parse, and a pipe
	// write is a BLOCKING call — so this is the one lock deliberately held
	// across one, and it guards nothing else.
	writeMu sync.Mutex

	// pendMu guards pending, and is held only for map access.
	pendMu  sync.Mutex
	pending map[uint64]chan *backupproto.Response

	dead     chan struct{}
	deadOnce sync.Once
	deadErr  error

	exited    chan struct{}
	closeOnce sync.Once

	// onDead lets the client stop routing new calls to a corpse the instant
	// its stream breaks, without waiting for the supervisor to notice.
	//
	// It is set BEFORE any goroutine that could read it is started, and never
	// afterwards: startConn takes it as an argument for exactly that reason.
	// Assigning it after the fact is a data race with die(), however small the
	// window. It takes the conn as an argument rather than closing over it for
	// the same reason — a closure would only move the race onto the variable.
	onDead func(*conn)
}

// startConn spawns the agent and begins reading its streams.
func startConn(bin string, extraEnv []string, onDead func(*conn)) (*conn, error) {
	cmd := exec.Command(bin)
	if len(extraEnv) > 0 {
		cmd.Env = extraEnv
	}
	// On Linux this asks the kernel to signal the child when this process dies,
	// as a second line of defence. The PRIMARY mechanism is portable and needs
	// no cooperation from the OS: knomit's death closes the write end of the
	// agent's stdin, the agent reads EOF, and it shuts down. See serveAgent.
	setParentDeathSignal(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("agent stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("agent stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("agent stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", bin, err)
	}

	c := &conn{
		cmd:     cmd,
		stdin:   stdin,
		pending: map[uint64]chan *backupproto.Response{},
		dead:    make(chan struct{}),
		exited:  make(chan struct{}),
		onDead:  onDead,
	}

	// cmd.Wait closes the pipes it created, so it must not run until both
	// readers are done — otherwise a read races a close and can lose the
	// agent's last words.
	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); c.readResponses(stdout) }()
	go func() { defer readers.Done(); forwardAgentLog(stderr) }()
	go func() {
		readers.Wait()
		err := cmd.Wait()
		c.die(fmt.Errorf("%w: agent process exited: %v", errAgentDown, err))
		close(c.exited)
	}()

	return c, nil
}

// readResponses delivers each response line to the waiter that asked for it.
func (c *conn) readResponses(r io.Reader) {
	br := bufio.NewReader(r)
	for {
		line, err := backupproto.ReadLine(br, backupproto.MaxLineBytes)
		if errors.Is(err, backupproto.ErrLineTooLong) {
			// Drained through its newline by ReadLine, so the stream is
			// resynchronised. One unreadable line must not end replication.
			log.Warn().Msg("backup agent sent an oversized response line; discarding it and continuing")
			continue
		}
		if err != nil {
			c.die(fmt.Errorf("%w: reading agent responses: %v", errAgentDown, err))
			return
		}
		var resp backupproto.Response
		if err := json.Unmarshal(line, &resp); err != nil {
			log.Warn().Err(err).Msg("backup agent sent an unparseable response line; discarding it and continuing")
			continue
		}
		if resp.ID == 0 {
			// Uncorrelatable — the agent could not recover an id from a
			// malformed request. Nobody is waiting for it, so log it rather
			// than lose it silently.
			log.Warn().Str("code", resp.Code).Str("error", resp.Error).
				Msg("backup agent rejected a request it could not correlate")
			continue
		}
		c.pendMu.Lock()
		ch, ok := c.pending[resp.ID]
		if ok {
			delete(c.pending, resp.ID)
		}
		c.pendMu.Unlock()
		if !ok {
			// A response to a call that already gave up (context cancelled, or
			// the caller timed out). Nothing to do.
			continue
		}
		ch <- &resp
	}
}

// die marks this generation dead exactly once and wakes everything waiting on
// it.
func (c *conn) die(err error) {
	c.deadOnce.Do(func() {
		c.deadErr = err
		close(c.dead)
		if c.onDead != nil {
			c.onDead(c)
		}
	})
}

// isDead reports whether this generation has already failed.
func (c *conn) isDead() bool {
	select {
	case <-c.dead:
		return true
	default:
		return false
	}
}

// deadError is the failure that ended this generation.
func (c *conn) deadError() error {
	select {
	case <-c.dead:
		if c.deadErr != nil {
			return c.deadErr
		}
	default:
	}
	return errAgentDown
}

// roundTrip writes one request and waits for its response.
//
// The manager's own locks are never held here: only writeMu, across the pipe
// write, and pendMu, across a map insert.
func (c *conn) roundTrip(ctx context.Context, req *backupproto.Request) (*backupproto.Response, error) {
	ch := make(chan *backupproto.Response, 1)

	c.pendMu.Lock()
	if c.isDead() {
		c.pendMu.Unlock()
		return nil, c.deadError()
	}
	c.pending[req.ID] = ch
	c.pendMu.Unlock()
	defer func() {
		c.pendMu.Lock()
		delete(c.pending, req.ID)
		c.pendMu.Unlock()
	}()

	c.writeMu.Lock()
	err := backupproto.WriteLine(c.stdin, req)
	c.writeMu.Unlock()
	if err != nil {
		c.die(fmt.Errorf("%w: writing to the agent: %v", errAgentDown, err))
		return nil, c.deadError()
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-c.dead:
		return nil, c.deadError()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// terminate shuts the child down and does not return until it is gone.
//
// Closing stdin is the request: the agent sees EOF, finishes in-flight work,
// closes its stores (a final replica sync per database) and exits. The kill is
// the deadline — knomit must never leave an orphan agent replicating to the
// same prefix as its successor, which is the two-writers case the whole feature
// exists to refuse.
func (c *conn) terminate(grace time.Duration) {
	c.closeOnce.Do(func() { _ = c.stdin.Close() })
	select {
	case <-c.exited:
		return
	case <-time.After(grace):
	}
	log.Warn().Dur("grace", grace).Msg("backup agent did not exit in time; killing it")
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	<-c.exited
}

// pid reports the child's process id, for tests and diagnostics.
func (c *conn) pid() int {
	if c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

// client supervises the agent process and carries the protocol.
//
// # What locking this actually needs
//
// The in-process design needed two mutexes because litestream calls ran under
// the caller's goroutine and could block for tens of seconds. Here the only
// blocking primitive the client owns is the pipe write, so the rule survives in
// a much smaller form: mu guards the current generation and the closed flag,
// and is released before every write and every wait. A call in flight therefore
// never blocks a concurrent Status, a restart, or Close.
type client struct {
	bin string
	env []string

	// establish is run against every new generation BEFORE it is published:
	// it opens the stores and re-tracks everything knomit believes is being
	// replicated. The agent holds its tracked set in memory, so without this a
	// crash would leave databases silently unreplicated — precisely the failure
	// this feature exists to prevent.
	establish func(context.Context, *conn) error

	nextID atomic.Uint64

	mu     sync.Mutex
	conn   *conn
	ready  chan struct{} // closed exactly when conn is live
	closed bool

	closing chan struct{} // closed by close(), to interrupt restart backoff
	stopped chan struct{} // closed when the supervisor has exited for good
}

// newClient builds an unstarted client. Construction is separate from start so
// the caller can install establish — which needs the client itself, to talk to
// a generation that is not published yet — before anything is spawned.
func newClient(bin string, env []string) *client {
	return &client{
		bin:     bin,
		env:     env,
		ready:   make(chan struct{}),
		closing: make(chan struct{}),
		stopped: make(chan struct{}),
	}
}

// start spawns the first generation. A failure here fails knomit's boot: there
// is no degraded "backup silently disabled" mode.
func (c *client) start(ctx context.Context) error {
	cn, err := c.spawn(ctx)
	if err != nil {
		return err
	}
	c.publish(cn)
	go c.supervise(cn)
	return nil
}

// spawn starts one generation and brings it up to a usable state.
//
// It is bounded without needing a deadline of its own: every call establish
// makes carries a per-method budget, so a child that starts and then goes deaf
// fails this rather than hanging the boot or the restart loop.
func (c *client) spawn(ctx context.Context) (*conn, error) {
	cn, err := startConn(c.bin, c.env, c.demote)
	if err != nil {
		return nil, err
	}
	if err := c.establish(ctx, cn); err != nil {
		cn.terminate(shutdownGrace)
		return nil, err
	}
	return cn, nil
}

// publish makes a generation the one new calls use.
func (c *client) publish(cn *conn) {
	c.mu.Lock()
	c.conn = cn
	ready := c.ready
	c.mu.Unlock()
	select {
	case <-ready:
	default:
		close(ready)
	}
}

// demote stops routing calls to a generation the moment its stream breaks,
// without waiting for the supervisor to finish reaping it.
func (c *client) demote(cn *conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != cn {
		return
	}
	c.conn = nil
	c.ready = make(chan struct{})
}

// supervise restarts the agent after an unexpected exit and re-establishes
// every database knomit believes is being replicated.
func (c *client) supervise(cn *conn) {
	defer close(c.stopped)
	backoff := restartBackoffMin
	for {
		<-cn.dead
		// The stream is gone; make sure the PROCESS is too before starting
		// another. Two agents replicating the same prefix is the two-writers
		// case, and it is worse than no agent at all.
		cn.terminate(shutdownGrace)
		if c.isClosed() {
			return
		}
		log.Error().Err(cn.deadError()).Msg("backup agent exited unexpectedly; restarting and re-establishing every tracked database")

		for {
			select {
			case <-c.closing:
				return
			case <-time.After(backoff):
			}
			if c.isClosed() {
				return
			}
			next, err := c.spawn(context.Background())
			if err != nil {
				log.Error().Err(err).Dur("retry_in", backoff).
					Msg("backup agent restart failed; databases are NOT being replicated until it succeeds")
				backoff = min(backoff*2, restartBackoffMax)
				continue
			}
			if c.isClosed() {
				// Close landed while we were starting. Do not leave it behind.
				next.terminate(shutdownGrace)
				return
			}
			cn = next
			break
		}
		backoff = restartBackoffMin
		c.publish(cn)
		log.Info().Int("pid", cn.pid()).Msg("backup agent restarted; tracked databases re-established")
	}
}

func (c *client) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// acquire returns the live generation, waiting a bounded time for a restart.
func (c *client) acquire(ctx context.Context) (*conn, error) {
	deadline := time.NewTimer(agentWaitTimeout)
	defer deadline.Stop()
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, errManagerClosed
		}
		cn, ready := c.conn, c.ready
		c.mu.Unlock()
		if cn != nil && !cn.isDead() {
			return cn, nil
		}
		select {
		case <-ready:
			// A new generation was published (or this one is stale); look
			// again.
			if cn != nil && cn.isDead() {
				// ready belongs to the dead generation; give the supervisor a
				// moment to swap it out.
				time.Sleep(time.Millisecond)
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("%w: no agent after %s", errAgentDown, agentWaitTimeout)
		}
	}
}

// call runs one request against the live agent, replaying it across an agent
// restart.
//
// Retrying is safe because every protocol method is idempotent, and it is
// necessary because the alternative — surfacing a crash as a failed Track — is
// how a database silently stops being replicated.
func (c *client) call(ctx context.Context, method string, params, out any) error {
	var lastErr error
	for attempt := 0; attempt < callAttempts; attempt++ {
		cn, err := c.acquire(ctx)
		if err != nil {
			return err
		}
		err = c.callOn(ctx, cn, method, params, out)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errAgentDown) && !isNotOpen(err) {
			return err
		}
		lastErr = err
		select {
		case <-cn.dead:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return lastErr
}

// callOn runs one request against a specific generation. establish uses it to
// talk to a connection that is not published yet.
//
// The budget is applied HERE rather than by callers, so no path can acquire an
// unbounded round trip by accident — including establish, which runs on the
// boot and restart paths where a hang is least visible and most damaging.
// context.WithTimeout already takes the minimum with any deadline the caller
// brought, so a caller may shorten the budget but never extend it.
func (c *client) callOn(ctx context.Context, cn *conn, method string, params, out any) error {
	req := &backupproto.Request{ID: c.nextID.Add(1), Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("encoding %s params: %w", method, err)
		}
		req.Params = raw
	}

	budget := budgetFor(method)
	callCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	resp, err := cn.roundTrip(callCtx, req)
	if err != nil {
		// Distinguish "the agent went deaf" from "my caller gave up". Only the
		// former is this agent's fault, and only the former is worth shouting
		// about: replication for that database has stopped making progress.
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			log.Error().Str("method", method).Dur("budget", budget).Int("agent_pid", cn.pid()).
				Msg("backup agent accepted a request and did not answer within its budget; " +
					"that database is not making progress")
			return fmt.Errorf("%w: %s (waited %s)", errAgentUnresponsive, method, budget)
		}
		return err
	}
	if !resp.OK {
		return protoError(method, resp)
	}
	if out != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("decoding the %s result: %w", method, err)
		}
	}
	return nil
}

// close shuts the agent down for good, in bounded time.
//
// The close REQUEST and the termination run concurrently, and that ordering is
// the whole point. Sending the request and waiting for its reply before
// closing the pipe means a deaf agent is never killed at all: the reply never
// comes, the grace-then-kill is never reached, and knomit hangs at shutdown
// leaving an orphan replicating to the prefix its successor will claim.
//
// Running them together costs nothing, because closing stdin does not
// interrupt an in-flight handler — the agent finishes it, writes the reply, and
// only then shuts down. So a responsive agent still returns its final-sync
// error, and an unresponsive one is killed on schedule.
func (c *client) close(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	cn := c.conn
	c.conn = nil
	c.mu.Unlock()
	// Wakes the supervisor out of a restart backoff, so it cannot spawn a
	// generation nobody will ever close.
	close(c.closing)

	if cn == nil {
		// No live generation: the supervisor may be mid-restart. Its spawn is
		// bounded by the per-method budgets, and it terminates anything it
		// brought up once it sees the closed flag.
		<-c.stopped
		return nil
	}

	done := make(chan error, 1)
	go func() { done <- c.callOn(ctx, cn, backupproto.MethodClose, nil, nil) }()

	cn.terminate(shutdownGrace)

	// The process is gone by now, which closes cn.dead and therefore unblocks
	// the request above however it ended — so this receive cannot hang.
	err := <-done
	if errors.Is(err, errAgentDown) || errors.Is(err, errAgentUnresponsive) || errors.Is(err, context.Canceled) {
		// The agent never answered, but it IS stopped: terminate saw to that.
		// Reporting the unanswered request as a Close failure would only
		// obscure the fact that shutdown succeeded.
		err = nil
	}
	<-c.stopped
	return err
}

// currentPID reports the live agent's process id, or 0. Tests use it to kill
// the agent; nothing in production depends on it.
func (c *client) currentPID() int {
	c.mu.Lock()
	cn := c.conn
	c.mu.Unlock()
	if cn == nil {
		return 0
	}
	return cn.pid()
}

// protoError turns a failed response into an error the caller can branch on.
// The code carries the identity an error STRING cannot survive a pipe with.
func protoError(method string, resp *backupproto.Response) error {
	msg := resp.Error
	if msg == "" {
		msg = "agent reported a failure with no message"
	}
	switch resp.Code {
	case backupproto.CodeNoSnapshot:
		return fmt.Errorf("%w: %s", errNoSnapshot, msg)
	case backupproto.CodeDiverged:
		return fmt.Errorf("%w: %s", ErrDiverged, msg)
	case backupproto.CodeTrackedElsewhere:
		return fmt.Errorf("%w (%s)", ErrTrackedElsewhere, msg)
	case backupproto.CodeNotOpen:
		return fmt.Errorf("%w: %s", errNotOpen, msg)
	}
	return fmt.Errorf("backup agent: %s: %s", method, msg)
}

// errNotOpen means the agent has not been configured yet. It is transient by
// construction — a generation is published only after open succeeds — so call
// retries it rather than surfacing it.
var errNotOpen = errors.New("backup agent has not been opened")

func isNotOpen(err error) bool { return errors.Is(err, errNotOpen) }

// forwardAgentLog copies the agent's stderr into knomit's logger, so a failing
// agent is visible in the server log rather than in a stream nobody reads.
//
// This loop MUST NOT stop before EOF, and that is a liveness requirement rather
// than a completeness one. Nothing else drains the child's stderr, so a reader
// that gave up would let the ~64 KiB pipe buffer fill and then block the agent
// on its next log write — forever, and without dying, so the supervisor would
// never restart it and every round trip against it would burn its full budget.
// A stuck logger is therefore a stopped backup.
//
// That is why this uses backupproto.ReadLine and not bufio.Scanner: Scanner
// stops permanently on a token past its buffer, which is exactly the failure
// above, triggered by one long line from a dependency we do not control.
//
// The agent logs JSON (slog's JSONHandler), so the level and message are
// preserved; anything that is not JSON — a panic trace, a runtime message, a
// stray print that landed on stderr because main redirected os.Stdout there —
// is forwarded verbatim rather than dropped.
func forwardAgentLog(r io.Reader) {
	br := bufio.NewReader(r)
	for {
		raw, err := backupproto.ReadLine(br, backupproto.MaxLineBytes)
		if errors.Is(err, backupproto.ErrLineTooLong) {
			// Drained through its newline, so the stream stays readable.
			log.Warn().Str("component", "backup-agent").
				Msg("discarded an oversized log line from the backup agent")
			continue
		}
		if err != nil {
			return
		}
		line := strings.TrimRight(string(raw), "\r\n")
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			log.Warn().Str("component", "backup-agent").Msg(line)
			continue
		}
		msg, _ := rec["msg"].(string)
		if msg == "" {
			log.Warn().Str("component", "backup-agent").Msg(line)
			continue
		}
		level, _ := rec["level"].(string)
		ev := log.WithLevel(agentLevel(level)).Str("component", "backup-agent")
		for k, v := range rec {
			switch k {
			case "msg", "level", "time":
				continue
			}
			ev = ev.Interface(k, v)
		}
		ev.Msg(msg)
	}
}

// agentLevel maps slog's level names onto zerolog's.
func agentLevel(level string) zerolog.Level {
	switch level {
	case "DEBUG":
		return zerolog.DebugLevel
	case "WARN":
		return zerolog.WarnLevel
	case "ERROR":
		return zerolog.ErrorLevel
	default:
		return zerolog.InfoLevel
	}
}
