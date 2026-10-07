package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"knomit/tools/bridge/knomitapi"
)

// repoCreatePoll is how often `kb repo create` asks about its create job, and
// repoCreateTimeout how long it waits in all before giving up (the job itself
// keeps running on the server; the error says how to look it up).
var (
	repoCreatePoll    = 500 * time.Millisecond
	repoCreateTimeout = 10 * time.Minute
)

// isRepoCommand reports whether args is `kb repo ...`: the FIRST argument
// exactly, so the proxy mode's `-repo` flag never matches.
func isRepoCommand(args []string) bool { return len(args) >= 1 && args[0] == "repo" }

// runRepo is `kb repo <subcommand>`. Today one: `kb repo create <new>
// --template <repo>/<name> [server]` (F24), which creates a repo on the knomit
// server from a template held by one of its mounted repos — POST
// /api/v1/repos with mode "template" — and waits for the create job to end.
// Both halves of --template are required: there is no default source.
func runRepo(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "create" {
		return errors.New("usage: kb repo create <new-repo> --template <repo>/<name> [server]")
	}
	fs := flag.NewFlagSet("kb repo create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	tmpl := fs.String("template", "", "the template, as <mounted repo>/<template name>")
	// Flags may come before or after the positionals (`kb repo create x
	// --template a/b`): Go's flag package stops at the first positional, so
	// parse again after each one.
	var pos []string
	rest := args[1:]
	for {
		if err := fs.Parse(rest); err != nil {
			return fmt.Errorf("kb repo create: %w", err)
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(pos) < 1 || len(pos) > 2 {
		return errors.New("usage: kb repo create <new-repo> --template <repo>/<name> [server]")
	}
	repo, name, ok := strings.Cut(*tmpl, "/")
	if !ok || repo == "" || name == "" || strings.Contains(name, "/") {
		return fmt.Errorf("kb repo create: --template must be <repo>/<name> (both required), got %q", *tmpl)
	}
	serverArg := ""
	if len(pos) == 2 {
		serverArg = pos[1]
	}
	srv, err := knomitapi.ResolveServer(serverArg)
	if err != nil {
		return err
	}
	return createFromTemplate(context.Background(), knomitapi.NewServerClient(srv, 30*time.Second), srv.Base, pos[0], repo, name, out)
}

type createJob struct {
	CreateID string `json:"create_id"`
	State    string `json:"state"`
	Message  string `json:"message"`
	Error    string `json:"error"`
}

func createFromTemplate(ctx context.Context, c *http.Client, base, newRepo, srcRepo, tmplName string, out io.Writer) error {
	body, _ := json.Marshal(map[string]any{
		"name": newRepo, "mode": "template",
		"template": map[string]string{"repo": srcRepo, "name": tmplName},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/repos", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("kb repo create: %w", err)
	}
	job, err := readJob(resp, http.StatusAccepted)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(repoCreateTimeout)
	for job.State == "running" || job.State == "cancelling" {
		if time.Now().After(deadline) {
			return fmt.Errorf("kb repo create: still running after %s; check GET /api/v1/repo-creates/%s", repoCreateTimeout, job.CreateID)
		}
		time.Sleep(repoCreatePoll)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/repo-creates/"+url.PathEscape(job.CreateID), nil)
		if err != nil {
			return err
		}
		resp, err := c.Do(req)
		if err != nil {
			return fmt.Errorf("kb repo create: poll: %w", err)
		}
		if job, err = readJob(resp, http.StatusOK); err != nil {
			return err
		}
	}
	if job.State != "done" {
		msg := job.Error
		if msg == "" {
			msg = job.Message
		}
		return fmt.Errorf("kb repo create: %s: %s", job.State, msg)
	}
	fmt.Fprintf(out, "created repo %s from template %s/%s\n", newRepo, srcRepo, tmplName)
	return nil
}

// readJob decodes a create-job status, or turns a problem+json answer into an
// error carrying its title and detail (the named refusal).
func readJob(resp *http.Response, want int) (createJob, error) {
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != want {
		var p struct {
			Title  string `json:"title"`
			Detail string `json:"detail"`
		}
		if json.Unmarshal(raw, &p) == nil && p.Title != "" {
			return createJob{}, fmt.Errorf("kb repo create: %d %s: %s", resp.StatusCode, p.Title, p.Detail)
		}
		return createJob{}, fmt.Errorf("kb repo create: %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var j createJob
	if err := json.Unmarshal(raw, &j); err != nil {
		return createJob{}, fmt.Errorf("kb repo create: bad answer: %w", err)
	}
	return j, nil
}
