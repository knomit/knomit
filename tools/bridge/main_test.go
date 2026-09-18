package main

import (
	"strings"
	"testing"
)

// TestLensConflict covers the mutual-exclusion guard: --lens rejects an
// explicitly-set --repo. Since --repo carries a non-empty default, the caller
// passes an explicit-set bool (from flag.Visit), never a value comparison.
func TestLensConflict(t *testing.T) {
	cases := []struct {
		name    string
		lens    string
		repoSet bool
		wantSub string // "" means no conflict
	}{
		{"no lens, repo set", "", true, ""},
		{"lens alone", "eng", false, ""},
		{"lens + repo", "eng", true, "mutually exclusive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lensConflict(tc.lens, tc.repoSet)
			if tc.wantSub == "" {
				if got != "" {
					t.Errorf("lensConflict = %q, want no conflict", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantSub) {
				t.Errorf("lensConflict = %q, want substring %q", got, tc.wantSub)
			}
		})
	}
}

func TestMcpURL_RepoMode(t *testing.T) {
	got := mcpURL("http://localhost:19278", "core", "", "agent:host")
	want := "http://localhost:19278/api/v1/repos/core/branches/agent:host/mcp"
	if got != want {
		t.Errorf("mcpURL repo mode = %q, want %q", got, want)
	}
}

func TestMcpURL_LensMode(t *testing.T) {
	// A lens has no branch segment (LensMiddleware resolves each mount's
	// branch server-side).
	got := mcpURL("http://localhost:19278", "", "eng", "")
	want := "http://localhost:19278/api/v1/lenses/eng/mcp"
	if got != want {
		t.Errorf("mcpURL lens mode = %q, want %q", got, want)
	}
}

// mcpURL must ignore repo/branch when a lens is set — a lens URL never carries
// a branch even if that arg is non-empty.
func TestMcpURL_LensTakesPrecedence(t *testing.T) {
	got := mcpURL("http://localhost:19278", "core", "eng", "agent:host")
	want := "http://localhost:19278/api/v1/lenses/eng/mcp"
	if got != want {
		t.Errorf("mcpURL lens precedence = %q, want %q", got, want)
	}
}

// With neither flag the bridge connects to the unscoped mount: the agent
// picks its repo or lens later with knomit_bind.
func TestMcpURL_UnscopedMode(t *testing.T) {
	got := mcpURL("http://h:1", "", "", "")
	if want := "http://h:1/api/v1/mcp"; got != want {
		t.Errorf("mcpURL unscoped mode = %q, want %q", got, want)
	}
}

// selectMode distinguishes "flag not given" from "flag given empty". Only the
// first selects the session-bound mount; the second is a misconfigured wrapper
// (an unset variable) and must exit rather than silently proxy.
func TestSelectMode(t *testing.T) {
	cases := []struct {
		name             string
		repo, lens       string
		repoSet, lensSet bool
		want             bridgeMode
	}{
		{"neither flag given", "", "", false, false, modeSessionBound},
		{"repo given", "work", "", true, false, modeRepo},
		{"lens given", "", "eng", false, true, modeLens},
		{"repo given empty", "", "", true, false, modeInvalid},
		{"lens given empty", "", "", false, true, modeInvalid},
		{"both given empty", "", "", true, true, modeInvalid},
		// An unset shell variable is the real-world shape of this mistake.
		{"repo from unset var", "", "", true, false, modeInvalid},
	}
	for _, c := range cases {
		if got := selectMode(c.repo, c.lens, c.repoSet, c.lensSet); got != c.want {
			t.Errorf("%s: selectMode(%q,%q,%v,%v)=%v want %v",
				c.name, c.repo, c.lens, c.repoSet, c.lensSet, got, c.want)
		}
	}
}

// A mistyped subcommand must not become a base URL. Session-bound mode made
// no-flags legal, so this is the only thing standing between `kb
// clade init` and a proxy that dials http://clade forever.
func TestBaseURLArg(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
		bad  bool
	}{
		{name: "no argument", args: nil, want: ""},
		{name: "http", args: []string{"http://localhost:19278"}, want: "http://localhost:19278"},
		{name: "https", args: []string{"https://kb.example.com"}, want: "https://kb.example.com"},
		{name: "trailing slash trimmed", args: []string{"http://h:1/"}, want: "http://h:1"},
		// The typo this guard exists for, and the tail flag.Parse never reached.
		{name: "mistyped subcommand", args: []string{"clade", "init", "-repo", "x"}, bad: true},
		{name: "bare subcommand", args: []string{"init"}, bad: true},
		{name: "host without scheme", args: []string{"localhost:19278"}, bad: true},
		{name: "wrong scheme", args: []string{"ftp://h/x"}, bad: true},
		{name: "scheme without host", args: []string{"http://"}, bad: true},
	}
	for _, c := range cases {
		got, err := baseURLArg(c.args)
		if c.bad {
			if err == nil {
				t.Errorf("%s: baseURLArg(%q) = %q, want an error", c.name, c.args, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: baseURLArg(%q) errored: %v", c.name, c.args, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: baseURLArg(%q) = %q, want %q", c.name, c.args, got, c.want)
		}
	}
}
