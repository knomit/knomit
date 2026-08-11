package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"knomit/internal/store"
)

// checkpointDB flushes dbPath's WAL into the main file so a subsequent
// file-level copy (copyFile) sees a complete, self-consistent database
// rather than one missing whatever transactions are still only in the WAL.
func checkpointDB(dbPath string) error {
	svc, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer svc.Close()
	return svc.Checkpoint()
}

// reposListView / repoInfoView mirror the shape of GET /api/v1/repos and
// GET /api/v1/repos/{name} closely enough to read the two fields this needs
// (see internal/web/handlers_repos_hal.go) — not a full HAL client, just
// enough to answer "what agent branch does this daemon expect its repos to
// have."
type reposListView struct {
	Embedded struct {
		Repos []struct {
			Name string `json:"name"`
		} `json:"repos"`
	} `json:"_embedded"`
}

type repoInfoView struct {
	AgentBranch string `json:"agent_branch"`
}

// registerWithDaemon makes a generated corpus visible in the live knomit
// daemon's web UI/API: copies dbPath into the daemon's repos directory,
// creates the daemon's own agent branch on the copy (pointing at the tip of
// writeBranch — see the doc comment on why this is needed), and triggers a
// rescan. Failure here is treated as non-fatal by the caller (build.go) —
// the corpus itself is complete and valid via calibrate bridges regardless
// of whether this succeeds.
//
// Why the branch step is needed: a knomit repo's stable "id" (and several
// web-UI views) resolve via RootCommit on the repo's *agent branch*
// (internal/repos/instance.go's ID()), which for any repo the running
// daemon didn't create itself defaults to that daemon's own per-machine
// convention (e.g. "agent/<hostname>-<suffix>") regardless of what branches
// actually exist in the file. corpusgen only ever writes to "main", so
// without this step a freshly-registered repo's id resolves to "" and at
// least one web UI view 404s trying to load it (found and fixed manually
// earlier this session before being generalized into this function).
func registerWithDaemon(ctx context.Context, dbPath, repoName, writeBranch, daemonURL string) error {
	agentBranch, err := discoverAgentBranch(ctx, daemonURL)
	if err != nil {
		return fmt.Errorf("discover daemon's agent branch (is the daemon running at %s?): %w", daemonURL, err)
	}

	// Checkpoint dbPath before the file-level copy below, unconditionally —
	// not just when the caller happens to have already flushed the WAL.
	// copyFile only copies the main .db file, not any -wal/-shm sidecars, so
	// skipping this produces a copy silently missing whatever transactions
	// are still sitting in the WAL: found the hard way when a write against
	// dbPath that left the WAL un-checkpointed (Close() doesn't do it
	// implicitly — see store.Service.Checkpoint's doc comment) was followed
	// by a standalone "corpusgen register", producing a copy that failed
	// "PRAGMA integrity_check" outright (invalid page numbers, duplicate
	// page references) rather than just looking stale.
	if err := checkpointDB(dbPath); err != nil {
		return fmt.Errorf("checkpoint %s before copy: %w", dbPath, err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}
	destPath := filepath.Join(home, ".knomit", "repos", repoName+".db")
	if err := copyFile(dbPath, destPath); err != nil {
		return fmt.Errorf("copy %s -> %s: %w", dbPath, destPath, err)
	}

	svc, err := store.Open(destPath)
	if err != nil {
		return fmt.Errorf("open registered copy: %w", err)
	}
	defer svc.Close()
	if err := svc.OpenRepo(); err != nil {
		return fmt.Errorf("open registered copy's git store: %w", err)
	}
	if err := svc.Branches().CreateBranch(ctx, agentBranch, writeBranch); err != nil {
		return fmt.Errorf("create agent branch %q from %q: %w", agentBranch, writeBranch, err)
	}
	svc.Close()

	if err := triggerRescan(ctx, daemonURL); err != nil {
		return fmt.Errorf("trigger rescan: %w", err)
	}
	return nil
}

// discoverAgentBranch asks the daemon for its repo list, then reads the
// agent_branch field off the first one — every repo a given daemon manages
// shares the same per-machine agent branch convention, so any existing repo
// tells us what a newly-registered one needs too.
func discoverAgentBranch(ctx context.Context, daemonURL string) (string, error) {
	var list reposListView
	if err := getJSON(ctx, daemonURL+"/api/v1/repos", &list); err != nil {
		return "", err
	}
	if len(list.Embedded.Repos) == 0 {
		return "", fmt.Errorf("daemon has no existing repos to learn its agent-branch convention from")
	}
	var info repoInfoView
	if err := getJSON(ctx, daemonURL+"/api/v1/repos/"+list.Embedded.Repos[0].Name, &info); err != nil {
		return "", err
	}
	if info.AgentBranch == "" {
		return "", fmt.Errorf("daemon repo %q has no agent_branch", list.Embedded.Repos[0].Name)
	}
	return info.AgentBranch, nil
}

func triggerRescan(ctx context.Context, daemonURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, daemonURL+"/api/v1/repos:rescan", nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("rescan returned %s: %s", resp.Status, body)
	}
	return nil
}

func getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s returned %s: %s", url, resp.Status, body)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
