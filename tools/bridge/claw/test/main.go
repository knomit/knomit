//go:build clawtest

// Command clawtestenv launches a local OpenClaw Docker container wired up for
// manual integration testing of the knomit OpenClaw plugin. It:
//
//   - runs the official OpenClaw image (ghcr.io/openclaw/openclaw:latest) with
//     host.docker.internal wired so the container can reach host services, and
//   - writes an OpenClaw config that uses a local Ollama install (default
//     :11434) as the default model backend.
//
// Pointing the in-container knomit plugin at the host knomit server is left to
// the operator (see the "Still to wire up" notes printed on `up`): knomit-bridge
// resolves its server URL from a positional base-url arg, then the tray
// lockfile, then localhost:19278 — it does NOT read an env var — so the host
// URL must be passed to knomit-bridge explicitly.
//
// It is a developer harness, NOT part of the shipped knomit-bridge binary:
// it lives in its own package (never imported by ./tools/bridge) and is guarded
// by the `clawtest` build tag, so `go build ./tools/bridge`, `go build ./...`,
// and `go test ./...` all skip it. Run it explicitly with the tag:
//
//	go run -tags clawtest ./tools/bridge/claw/test up      # start (default)
//	go run -tags clawtest ./tools/bridge/claw/test logs    # follow logs
//	go run -tags clawtest ./tools/bridge/claw/test down     # stop & remove
//
// Prerequisites: Docker (e.g. OrbStack), a knomit server listening on the host
// knomit port, and Ollama listening on the host Ollama port.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// config holds the resolved launch parameters. Flags feed it; every value has a
// sensible default so the zero-flag `up` just works on a standard OrbStack +
// local-knomit + local-Ollama setup.
type config struct {
	image       string // OpenClaw image ref
	name        string // container name (idempotent: recreated on `up`)
	uiPort      int    // host port mapped to the container's 18789 control UI
	knomitPort  int    // host port where the knomit server listens
	ollamaPort  int    // host port where Ollama listens
	model       string // Ollama model tag, e.g. "llama3.2"
	hostGateway string // hostname the container uses to reach host services
	configDir   string // host dir bind-mounted to /home/node/.openclaw
	pull        bool   // docker pull before run
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	// First positional arg is the subcommand; default to "up".
	cmd := "up"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}

	home, _ := os.UserHomeDir()
	cfg := config{}
	fs := flag.NewFlagSet("clawtestenv", flag.ContinueOnError)
	fs.StringVar(&cfg.image, "image", "ghcr.io/openclaw/openclaw:latest", "OpenClaw image ref")
	fs.StringVar(&cfg.name, "name", "knomit-openclaw-test", "container name")
	fs.IntVar(&cfg.uiPort, "ui-port", 18789, "host port for the OpenClaw control UI")
	fs.IntVar(&cfg.knomitPort, "knomit-port", 19278, "host port where knomit serves")
	fs.IntVar(&cfg.ollamaPort, "ollama-port", 11434, "host port where Ollama serves")
	fs.StringVar(&cfg.model, "model", "llama3.2", "Ollama model tag to use as the default agent model")
	fs.StringVar(&cfg.hostGateway, "host-gateway", "host.docker.internal", "hostname the container uses to reach host services")
	fs.StringVar(&cfg.configDir, "config-dir", filepath.Join(home, ".openclaw-knomit-test"),
		"host dir bind-mounted to /home/node/.openclaw (kept separate from a real ~/.openclaw)")
	fs.BoolVar(&cfg.pull, "pull", false, "docker pull the image before running")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker not found on PATH: %w", err)
	}

	switch cmd {
	case "up":
		return up(cfg)
	case "down":
		return down(cfg)
	case "logs":
		return docker("logs", "-f", cfg.name)
	default:
		return fmt.Errorf("unknown subcommand %q (want: up, down, logs)", cmd)
	}
}

func up(cfg config) error {
	// Non-fatal reachability checks: the container still starts if these are
	// down, but flagging it now saves a confusing debugging session later.
	warnIfUnreachable("knomit server", cfg.knomitPort)
	warnIfUnreachable("Ollama", cfg.ollamaPort)

	if err := writeOpenClawConfig(cfg); err != nil {
		return err
	}

	if cfg.pull {
		if err := docker("pull", cfg.image); err != nil {
			return err
		}
	}

	// Idempotent: drop any prior container with the same name before recreating.
	_ = docker("rm", "-f", cfg.name)

	knomitURL := fmt.Sprintf("http://%s:%d", cfg.hostGateway, cfg.knomitPort)
	args := []string{
		"run", "-d",
		"--name", cfg.name,
		// host-gateway lets the container reach host services; OrbStack maps
		// host.docker.internal automatically, this keeps it portable to plain
		// Docker Engine on Linux too.
		"--add-host", "host.docker.internal:host-gateway",
		"-p", fmt.Sprintf("%d:18789", cfg.uiPort),
		"-v", cfg.configDir + ":/home/node/.openclaw",
		// We pre-write the model config, so skip the interactive onboarding.
		"-e", "OPENCLAW_SKIP_ONBOARDING=1",
		cfg.image,
	}
	if err := docker(args...); err != nil {
		return err
	}

	printNextSteps(cfg, knomitURL)
	return nil
}

func down(cfg config) error {
	if err := docker("rm", "-f", cfg.name); err != nil {
		return err
	}
	fmt.Printf("removed container %q (config dir %s left intact)\n", cfg.name, cfg.configDir)
	return nil
}

// writeOpenClawConfig writes ~/.openclaw/openclaw.json inside the mounted config
// dir, defining a local-Ollama custom provider and making it the default model.
// Reachable from the container as host.<hostGateway>:<ollamaPort>/v1 using the
// OpenAI-compatible ("openai-completions") adapter Ollama exposes.
func writeOpenClawConfig(cfg config) error {
	if err := os.MkdirAll(cfg.configDir, 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	ollamaURL := fmt.Sprintf("http://%s:%d/v1", cfg.hostGateway, cfg.ollamaPort)

	// JSON is valid JSON5, which is what OpenClaw parses.
	doc := map[string]any{
		"models": map[string]any{
			"mode": "merge",
			"providers": map[string]any{
				"ollama": map[string]any{
					"baseUrl": ollamaURL,
					"api":     "openai-completions",
					"models": []any{
						map[string]any{
							"id":            cfg.model,
							"name":          cfg.model,
							"reasoning":     false,
							"input":         []string{"text"},
							"contextWindow": 8192,
							"maxTokens":     4096,
						},
					},
				},
			},
		},
		"agents": map[string]any{
			"defaults": map[string]any{
				"model": map[string]any{
					"primary": "ollama/" + cfg.model,
				},
			},
		},
	}

	blob, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	path := filepath.Join(cfg.configDir, "openclaw.json")
	if err := os.WriteFile(path, append(blob, '\n'), 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	fmt.Printf("wrote OpenClaw config → %s (default model ollama/%s via %s)\n", path, cfg.model, ollamaURL)
	return nil
}

// docker runs the docker CLI with args, streaming its output to this process.
func docker(args ...string) error {
	c := exec.Command("docker", args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// warnIfUnreachable prints a warning (does not fail) if nothing is listening on
// localhost:port. A host service the container will call should be up first.
func warnIfUnreachable(what string, port int) {
	addr := net.JoinHostPort("localhost", strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		fmt.Printf("warning: %s not reachable on %s (%v) — start it before using OpenClaw\n", what, addr, err)
		return
	}
	_ = conn.Close()
}

func printNextSteps(cfg config, knomitURL string) {
	fmt.Printf(`
OpenClaw container %q is up.
  Control UI:     http://127.0.0.1:%d/   (OrbStack: http://%s.orb.local)
  Host knomit:    %s  (reach it from the container at this URL)
  Ollama model:   ollama/%s
  Logs:           go run -tags clawtest ./tools/bridge/claw/test logs
  Stop/remove:    go run -tags clawtest ./tools/bridge/claw/test down

Still to wire up the knomit plugin itself (out of scope for this launcher):
  1. Build a Linux knomit-bridge (pure Go, cross-compiles from macOS):
       GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o %s/bin/knomit-bridge ./tools/bridge
     (the mounted config dir is a handy place; ensure it lands on the container PATH)
  2. Scaffold + register the plugin in the container via: knomit-bridge claw init
  3. The plugin (register.mjs) spawns bare "knomit-bridge" in stdio-MCP mode,
     which defaults to localhost:19278 — i.e. the CONTAINER, not the host. To
     reach the host knomit above, make it spawn with the host URL as the
     positional base-url arg (e.g. adjust register.mjs's spawn args, or drop a
     wrapper "knomit-bridge" on PATH that appends "%s").
`, cfg.name, cfg.uiPort, cfg.name, knomitURL, cfg.model, cfg.configDir, knomitURL)
}
