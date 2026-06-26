// Command drone executes an implementation plan with Claude Code, unattended
// and sandboxed, then lets Claude open a PR against the repo.
//
// It does the deterministic, risky setup itself (clean-tree check, branch
// creation, token plumbing, sandbox policy) and delegates the open-ended work
// (implement + test + commit + push + open PR) to a single headless Claude run.
//
// Configuration is layered (lowest to highest precedence): built-in defaults,
// a TOML config file, DRONE_* environment variables, then command-line flags.
//
//	go run ./tools/drone --plan .claude/plans/my-plan.md
//	go run ./tools/drone --config drone.toml
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// config is the fully-resolved run configuration, assembled from defaults, the
// TOML file, DRONE_* env vars, and flags by loadConfig.
type config struct {
	plan      string
	repo      string
	base      string
	branch    string
	model     string
	budget    float64
	sandbox   bool
	yes       bool
	dryRun    bool
	logDir    string
	logLevel  string
	domains   []string // extra sandbox-allowed domains, appended to built-ins
	allowDirs []string // extra sandbox-writable dirs, appended to built-ins

	configFile string // path of the config file actually loaded (for reporting)
	logPath    string // derived at runtime: <logDir>/drone-<ts>.jsonl
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		// Logging may not be configured yet if we failed early; be defensive.
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "drone --plan <plan.md> [flags]",
		Short:         "Run an implementation plan with Claude Code, unattended and sandboxed, then open a PR.",
		Long: `drone executes an implementation plan with Claude Code, unattended and
sandboxed, then lets Claude open a PR.

Configuration is layered, lowest to highest precedence:
  built-in defaults  <  TOML config file  <  DRONE_* env vars  <  command-line flags

Point --config at a TOML file, or drop a drone.toml in the working directory or
~/.config/drone/. See drone.example.toml for every key.`,
		Example: `  # Preview exactly what would run — no branch, no launch, no spend:
  drone --plan .claude/plans/my-plan.md --dry-run

  # Execute a plan: branch off dev, implement, test, open a PR against dev:
  drone --plan .claude/plans/my-plan.md

  # Drive everything from a config file:
  drone --config drone.toml

  # Cap spend, target master, skip the countdown:
  drone --plan .claude/plans/my-plan.md --base master --budget 20 --yes`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig(cmd)
			if err != nil {
				return err
			}
			return run(cfg)
		},
	}

	f := cmd.Flags()
	f.String("config", "", "path to a TOML config file (else drone.toml in . or ~/.config/drone)")
	f.String("plan", "", "implementation plan markdown file (required unless set in config)")
	f.String("repo", ".", "git repository to work in")
	f.String("base", "dev", "branch the PR will target")
	f.String("branch", "", "working branch name (default: auto/<plan>-<timestamp>)")
	f.String("model", "opus", "model alias or full id passed to claude --model")
	f.Float64("budget", 0, "max USD to spend (0 = unlimited)")
	f.Bool("sandbox", true, "run inside the OS sandbox (use --sandbox=false to disable; dangerous)")
	f.Bool("yes", false, "skip the pre-flight countdown")
	f.Bool("dry-run", false, "print the plan and the claude invocation, then exit")
	f.String("log-dir", ".claude", "directory for the run's audit logs (relative paths resolve against --repo)")
	f.String("log-level", "info", "zerolog level: trace, debug, info, warn, error")
	f.StringArray("allow-domain", nil, "extra domain to allow through the sandbox (repeatable)")
	f.StringArray("allow-write", nil, "extra directory the sandbox may write to (repeatable)")
	return cmd
}

// loadConfig resolves configuration from defaults, the TOML file, DRONE_* env
// vars, and flags (in increasing precedence) via viper.
func loadConfig(cmd *cobra.Command) (*config, error) {
	v := viper.New()

	v.SetDefault("repo", ".")
	v.SetDefault("base", "dev")
	v.SetDefault("branch", "")
	v.SetDefault("model", "opus")
	v.SetDefault("budget", 0.0)
	v.SetDefault("yes", false)
	v.SetDefault("dry_run", false)
	v.SetDefault("log_dir", ".claude")
	v.SetDefault("log_level", "info")
	v.SetDefault("sandbox.enabled", true)

	// Flag name (kebab) -> viper key (snake / dotted). Bound values only win
	// when the flag was actually set, preserving the precedence order.
	binds := map[string]string{
		"plan":         "plan",
		"repo":         "repo",
		"base":         "base",
		"branch":       "branch",
		"model":        "model",
		"budget":       "budget",
		"yes":          "yes",
		"dry-run":      "dry_run",
		"log-dir":      "log_dir",
		"log-level":    "log_level",
		"sandbox":      "sandbox.enabled",
		"allow-domain": "sandbox.allow_domains",
		"allow-write":  "sandbox.allow_write",
	}
	for flagName, key := range binds {
		if f := cmd.Flags().Lookup(flagName); f != nil {
			if err := v.BindPFlag(key, f); err != nil {
				return nil, fmt.Errorf("bind flag %s: %w", flagName, err)
			}
		}
	}

	v.SetEnvPrefix("DRONE")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	v.AutomaticEnv()

	if cf, _ := cmd.Flags().GetString("config"); cf != "" {
		v.SetConfigFile(cf)
		if filepath.Ext(cf) == "" {
			v.SetConfigType("toml")
		}
	} else {
		// Match drone.toml by extension only. Deliberately NOT setting a config
		// type here: that would make viper also consider a bare file named
		// "drone" (e.g. a stray binary) a candidate and try to parse it.
		v.SetConfigName("drone")
		v.AddConfigPath(".")
		if home, err := os.UserHomeDir(); err == nil {
			v.AddConfigPath(filepath.Join(home, ".config", "drone"))
		}
	}
	if err := v.ReadInConfig(); err != nil {
		// A missing config file is only an error when one was named explicitly.
		var notFound viper.ConfigFileNotFoundError
		if !asConfigNotFound(err, &notFound) {
			return nil, fmt.Errorf("read config %q: %w", v.ConfigFileUsed(), err)
		}
	}

	return &config{
		plan:       v.GetString("plan"),
		repo:       v.GetString("repo"),
		base:       v.GetString("base"),
		branch:     v.GetString("branch"),
		model:      v.GetString("model"),
		budget:     v.GetFloat64("budget"),
		sandbox:    v.GetBool("sandbox.enabled"),
		yes:        v.GetBool("yes"),
		dryRun:     v.GetBool("dry_run"),
		logDir:     v.GetString("log_dir"),
		logLevel:   v.GetString("log_level"),
		domains:    v.GetStringSlice("sandbox.allow_domains"),
		allowDirs:  v.GetStringSlice("sandbox.allow_write"),
		configFile: v.ConfigFileUsed(),
	}, nil
}

func asConfigNotFound(err error, target *viper.ConfigFileNotFoundError) bool {
	if nf, ok := err.(viper.ConfigFileNotFoundError); ok {
		*target = nf
		return true
	}
	return false
}

func setupLogging(level string) {
	lvl, err := zerolog.ParseLevel(level)
	if err != nil {
		lvl = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(lvl)
	log.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05"}).
		With().Timestamp().Logger()
}

func run(cfg *config) error {
	setupLogging(cfg.logLevel)

	if cfg.plan == "" {
		return fmt.Errorf("no plan specified — pass --plan or set `plan` in the config file")
	}

	repo, err := filepath.Abs(cfg.repo)
	if err != nil {
		return fmt.Errorf("resolve repo path: %w", err)
	}
	cfg.repo = repo

	planBytes, err := os.ReadFile(cfg.plan)
	if err != nil {
		return fmt.Errorf("read plan: %w", err)
	}
	planText := strings.TrimSpace(string(planBytes))
	if planText == "" {
		return fmt.Errorf("plan file %s is empty", cfg.plan)
	}

	if err := preflight(cfg); err != nil {
		return err
	}

	if cfg.branch == "" {
		stamp := time.Now().Format("20060102-1504")
		base := strings.TrimSuffix(filepath.Base(cfg.plan), filepath.Ext(cfg.plan))
		cfg.branch = fmt.Sprintf("auto/%s-%s", sanitizeBranch(base), stamp)
	}
	if err := resolveLogPath(cfg); err != nil {
		return err
	}

	token, err := ghToken()
	if err != nil {
		return err
	}

	settings, err := buildSettings(cfg)
	if err != nil {
		return err
	}
	prompt := buildPrompt(cfg, planText)
	args := claudeArgs(cfg, settings)

	logBanner(cfg, planText)
	if cfg.dryRun {
		fmt.Println("\n--- claude args ---\nclaude " + strings.Join(args, " "))
		fmt.Println("\n--- settings ---\n" + settings)
		fmt.Println("\n--- prompt (truncated) ---\n" + truncate(prompt, 1200))
		return nil
	}
	if !cfg.yes {
		countdown(5)
	}

	// Deterministic, risky setup happens here — not inside the agent — so the
	// branch always exists and is clean before the unattended run begins.
	if err := prepareBranch(cfg); err != nil {
		return err
	}

	log.Info().Str("branch", cfg.branch).Msg("launching claude")
	if err := launchClaude(cfg, args, prompt, token); err != nil {
		return fmt.Errorf("claude run failed (raw log: %s): %w", cfg.logPath, err)
	}

	reportResult(cfg)
	return nil
}

// resolveLogPath turns the configured log directory into a concrete,
// timestamped, created path for this run's audit artifacts.
func resolveLogPath(cfg *config) error {
	dir := cfg.logDir
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(cfg.repo, dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create log dir %s: %w", dir, err)
	}
	cfg.logPath = filepath.Join(dir, "drone-"+time.Now().Format("20060102-150405")+".jsonl")
	return nil
}

// preflight checks tools, repo state, and base branch before touching anything.
func preflight(cfg *config) error {
	for _, tool := range []string{"claude", "gh", "git", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%q not found on PATH", tool)
		}
	}
	if out, err := git(cfg.repo, "rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(out) != "true" {
		return fmt.Errorf("%s is not a git work tree", cfg.repo)
	}
	if !cfg.dryRun {
		if out, _ := git(cfg.repo, "status", "--porcelain"); strings.TrimSpace(out) != "" {
			return fmt.Errorf("working tree is dirty; commit or stash first:\n%s", out)
		}
	}
	if _, err := git(cfg.repo, "rev-parse", "--verify", "--quiet", cfg.base); err != nil {
		if _, err := git(cfg.repo, "rev-parse", "--verify", "--quiet", "origin/"+cfg.base); err != nil {
			return fmt.Errorf("base branch %q not found locally or on origin", cfg.base)
		}
	}
	return nil
}

// prepareBranch fetches the base and creates a fresh working branch off it.
func prepareBranch(cfg *config) error {
	if _, err := git(cfg.repo, "fetch", "origin", cfg.base); err != nil {
		log.Warn().Str("base", cfg.base).Msg("git fetch failed; continuing with local base")
	}
	start := cfg.base
	if _, err := git(cfg.repo, "rev-parse", "--verify", "--quiet", "origin/"+cfg.base); err == nil {
		start = "origin/" + cfg.base
	}
	if _, err := git(cfg.repo, "checkout", "-b", cfg.branch, start); err != nil {
		return fmt.Errorf("create branch %s off %s: %w", cfg.branch, start, err)
	}
	return nil
}

// buildSettings assembles the inline JSON enabling the OS sandbox with just
// enough filesystem and network reach to implement, test, push, and open a PR.
func buildSettings(cfg *config) (string, error) {
	if !cfg.sandbox {
		return "{}", nil
	}
	home, _ := os.UserHomeDir()
	gomod := strings.TrimSpace(goEnv("GOMODCACHE"))
	gocache := strings.TrimSpace(goEnv("GOCACHE"))

	allowWrite := []string{
		cfg.repo,
		"/tmp", "/private/tmp",
		filepath.Join(home, ".claude"),
		filepath.Join(home, ".knomit"), // knomit MCP writes facts outside the repo
	}
	if gomod != "" {
		allowWrite = append(allowWrite, gomod) // go build/test writes the module cache
	}
	if gocache != "" {
		allowWrite = append(allowWrite, gocache) // go build/test writes compiled objects
	}
	allowWrite = append(allowWrite, cfg.allowDirs...)

	allowedDomains := []string{
		"github.com", "api.github.com", "codeload.github.com",
		"objects.githubusercontent.com", "*.githubusercontent.com",
		"proxy.golang.org", "sum.golang.org", "*.golang.org",
		"storage.googleapis.com",
	}
	allowedDomains = append(allowedDomains, cfg.domains...)

	settings := map[string]any{
		"sandbox": map[string]any{
			"enabled":        true,
			"filesystem":     map[string]any{"allowWrite": allowWrite},
			"allowedDomains": allowedDomains,
		},
	}
	b, err := json.Marshal(settings)
	if err != nil {
		return "", fmt.Errorf("marshal settings: %w", err)
	}
	return string(b), nil
}

func claudeArgs(cfg *config, settings string) []string {
	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "bypassPermissions",
		"--model", cfg.model,
		"--add-dir", cfg.repo,
		"--settings", settings,
	}
	if cfg.budget > 0 {
		args = append(args, "--max-budget-usd", fmt.Sprintf("%.2f", cfg.budget))
	}
	return args
}

func buildPrompt(cfg *config, planText string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `You are running UNATTENDED and SANDBOXED. No human is available to answer
questions or approve actions — make reasonable decisions and finish the job.

Repository: %s
You are already on a fresh branch: %s (created off %s).
When done, open a pull request that targets: %s

## Your task

Implement the plan below, end to end:

1. Read the plan and any files it references. Follow this repository's
   conventions (CLAUDE.md, existing patterns, tests). Use the project's
   knomit memory tools to recall relevant invariants before non-trivial edits.
2. Implement every step of the plan. Write the code properly — no shortcuts,
   no stubbing things out to "make it pass". Add or update tests as the plan
   and project conventions require.
3. Run the full build and test suite (e.g. `+"`go build ./... && go test ./...`"+`).
   Everything must pass. If a step genuinely cannot be completed, STOP and
   report why — do NOT open a PR for broken or half-finished work.
4. Commit your work in focused commits with clear messages. Do NOT add any
   "Co-Authored-By" or "Generated with" attribution trailers to commit
   messages or the PR body.
5. Push the branch to origin and open the PR with `+"`gh pr create --base %s`"+`,
   giving it a descriptive title and a body that summarizes the changes,
   notes test results, and links the plan file. Print the PR URL.

## Definition of done

- All planned changes implemented.
- `+"`go build ./... && go test ./...`"+` (or the project's equivalent) passes.
- Branch pushed, PR opened against %s, PR URL printed.

If you must abort, clearly print "RUN ABORTED:" followed by the reason, and do
not open a PR.

## The implementation plan

%s
`, cfg.repo, cfg.branch, cfg.base, cfg.base, cfg.base, cfg.base, planText)
	return b.String()
}

// launchClaude runs claude headless, feeding the prompt on stdin, streaming a
// human-readable digest to the log while teeing raw JSON to the log file.
func launchClaude(cfg *config, args []string, prompt, token string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = cfg.repo
	cmd.Stdin = strings.NewReader(prompt)
	// GH_TOKEN lets gh and git's gh credential helper work without keychain,
	// which the Seatbelt sandbox would otherwise block.
	cmd.Env = append(os.Environ(), "GH_TOKEN="+token, "GITHUB_TOKEN="+token)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	logFile, err := os.Create(cfg.logPath)
	if err != nil {
		return fmt.Errorf("create log: %w", err)
	}
	defer logFile.Close()

	// Persist the prompt and claude's stderr alongside the event log so a run is
	// fully reconstructable after the fact, even if it crashes mid-stream.
	os.WriteFile(siblingPath(cfg.logPath, ".prompt.txt"), []byte(prompt), 0o644)
	errPath := siblingPath(cfg.logPath, ".stderr.log")
	if errFile, ferr := os.Create(errPath); ferr == nil {
		defer errFile.Close()
		cmd.Stderr = io.MultiWriter(os.Stderr, errFile)
	} else {
		cmd.Stderr = os.Stderr
	}
	log.Info().Str("events", cfg.logPath).Str("stderr", errPath).Msg("audit logs")

	if err := cmd.Start(); err != nil {
		return err
	}

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024) // events can be large
	for sc.Scan() {
		line := sc.Bytes()
		logFile.Write(line)
		logFile.Write([]byte("\n"))
		printEvent(line)
	}
	if err := sc.Err(); err != nil {
		log.Warn().Err(err).Msg("stream read error")
	}
	return cmd.Wait()
}

// printEvent turns one stream-json line into a concise log message.
func printEvent(line []byte) {
	var ev struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		IsError bool   `json:"is_error"`
		Result  string `json:"result"`
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return // ignore non-JSON / partial lines
	}
	switch ev.Type {
	case "system":
		if ev.Subtype == "init" {
			log.Debug().Msg("session initialized")
		}
	case "assistant":
		for _, c := range ev.Message.Content {
			switch c.Type {
			case "text":
				if t := strings.TrimSpace(c.Text); t != "" {
					log.Info().Msg(t)
				}
			case "tool_use":
				log.Info().Str("tool", c.Name).Str("arg", summarizeInput(c.Input)).Msg("tool")
			}
		}
	case "result":
		if ev.IsError {
			log.Error().Msg("result: " + truncate(ev.Result, 2000))
		} else {
			log.Info().Msg("result: " + truncate(ev.Result, 2000))
		}
	}
}

func summarizeInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	for _, k := range []string{"command", "file_path", "path", "pattern", "description"} {
		if v, ok := m[k]; ok {
			return truncate(fmt.Sprintf("%v", v), 100)
		}
	}
	return ""
}

func reportResult(cfg *config) {
	log.Info().
		Str("events", cfg.logPath).
		Str("stderr", siblingPath(cfg.logPath, ".stderr.log")).
		Str("prompt", siblingPath(cfg.logPath, ".prompt.txt")).
		Str("branch", cfg.branch).
		Msg("claude finished — audit trail")
	if out, err := gh(cfg.repo, "pr", "view", cfg.branch, "--json", "url,title,state", "-q", `.state + "  " + .url + "  " + .title`); err == nil {
		log.Info().Str("pr", strings.TrimSpace(out)).Msg("pull request")
	} else {
		log.Warn().Str("branch", cfg.branch).Msg("no PR found — check the log; the run may have aborted")
	}
}

// --- small helpers ---

func logBanner(cfg *config, planText string) {
	sandbox := "on"
	if !cfg.sandbox {
		sandbox = "OFF — UNSANDBOXED"
	}
	budget := "unlimited"
	if cfg.budget > 0 {
		budget = fmt.Sprintf("$%.2f", cfg.budget)
	}
	e := log.Info().
		Str("repo", cfg.repo).
		Str("plan", cfg.plan).
		Int("plan_bytes", len(planText)).
		Str("branch", cfg.branch).
		Str("base", cfg.base).
		Str("model", cfg.model).
		Str("budget", budget).
		Str("sandbox", sandbox).
		Str("log_dir", cfg.logDir)
	if cfg.configFile != "" {
		e = e.Str("config", cfg.configFile)
	}
	e.Msg("drone configuration")
}

func countdown(secs int) {
	log.Info().Int("seconds", secs).Msg("starting unattended run — Ctrl-C to abort")
	time.Sleep(time.Duration(secs) * time.Second)
}

func git(repo string, args ...string) (string, error) {
	return runCmd(repo, "git", args...)
}

func gh(repo string, args ...string) (string, error) {
	return runCmd(repo, "gh", args...)
}

func goEnv(key string) string {
	out, _ := runCmd("", "go", "env", key)
	return out
}

func ghToken() (string, error) {
	out, err := runCmd("", "gh", "auth", "token")
	if err != nil {
		return "", fmt.Errorf("gh auth token failed (run `gh auth login`): %w", err)
	}
	tok := strings.TrimSpace(out)
	if tok == "" {
		return "", fmt.Errorf("gh returned an empty token; run `gh auth login`")
	}
	return tok, nil
}

func runCmd(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// siblingPath returns the log path with its extension swapped for suffix, so
// the prompt, stderr, and event log share one timestamped basename.
func siblingPath(logPath, suffix string) string {
	return strings.TrimSuffix(logPath, filepath.Ext(logPath)) + suffix
}

func sanitizeBranch(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '/':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
