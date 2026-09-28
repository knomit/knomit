package knomitapi

import "testing"

func TestIsKnomitCommand(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want bool
	}{
		{"kb", true},
		{"/usr/local/bin/kb", true},
		{"kb.exe", true},
		// Note: a `C:\...\kb.exe` style path is only split correctly
		// by filepath.Base on Windows. That is fine — the config is written and
		// read on the same machine — so there is no cross-platform row here.
		{"kb-dev", false}, // a wrapper/dev build is NOT a command match
		// The pre-rename name. The rename shipped no compatibility alias, so
		// an un-migrated config is not recognised and init re-scaffolds beside it.
		{"knomit-bridge", false},
		{"/usr/local/bin/knomit-bridge", false},
		{"something-else", false},
		{"", false},
	} {
		if got := IsKnomitCommand(tc.cmd); got != tc.want {
			t.Errorf("IsKnomitCommand(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
}

func TestIsKnomitKey(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want bool
	}{
		{"knomit", true},
		{"knomit-repo-alpha", true},
		{"knomit-lens-eng", true},
		{"knomitten", false}, // no hyphen: not our namespace
		{"other", false},
	} {
		if got := IsKnomitKey(tc.key); got != tc.want {
			t.Errorf("IsKnomitKey(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
}

// ClassifyArgs is the ONE parser of a knomit entry's args, shared by both
// hosts' hooks and both inits. It reads the args the way the bridge will run
// them: --log is peeled, and parsing stops at the first positional or "--",
// exactly where the bridge's flag.Parse stops. The flag fields record that a
// flag APPEARED, value or not: a degenerate --lens must stay distinguishable
// from "no knomit flag at all", or a caller will fall through to a repo scope
// and read the wrong knowledge base.
func TestClassifyArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want Scope
	}{
		{"repo mode", []string{"--repo", "proj"}, Scope{Repo: "proj", RepoFlag: true}},
		{"repo equals form", []string{"--repo=proj"}, Scope{Repo: "proj", RepoFlag: true}},
		{"single dash", []string{"-repo", "proj"}, Scope{Repo: "proj", RepoFlag: true}},
		{"lens mode", []string{"--lens", "eng"}, Scope{Lens: "eng", LensFlag: true}},
		{"lens equals form", []string{"--lens=eng"}, Scope{Lens: "eng", LensFlag: true}},
		{"lens wins after repo", []string{"--repo", "proj", "--lens", "eng"}, Scope{Lens: "eng", LensFlag: true}},
		{"lens wins before repo", []string{"--lens", "eng", "--repo", "proj"}, Scope{Lens: "eng", LensFlag: true}},

		// Each of these is lens mode with an unusable name. Reporting no lens
		// flag here is what let a sibling --repo entry win.
		{"lens token, no value", []string{"--lens"}, Scope{LensFlag: true}},
		{"lens equals empty", []string{"--lens="}, Scope{LensFlag: true}},
		{"lens token, no value, after repo", []string{"--repo", "proj", "--lens"}, Scope{LensFlag: true}},

		// A repo flag with no value is broken, not unbound.
		{"repo token, no value", []string{"--repo"}, Scope{RepoFlag: true}},
		{"repo equals empty", []string{"--repo="}, Scope{RepoFlag: true}},

		{"no knomit flags at all", []string{"--verbose"}, Scope{}},
		{"empty args", nil, Scope{}},

		// A flag-shaped token must never be consumed as a repo NAME.
		{"repo followed by another flag", []string{"--repo", "--repo", "proj"}, Scope{Repo: "proj", RepoFlag: true}},
		{"repo followed by lens", []string{"--repo", "--lens", "eng"}, Scope{Lens: "eng", LensFlag: true}},

		// Parsing stops where the bridge's flag.Parse stops.
		{"positional first", []string{"http://host:8080", "--repo", "x"}, Scope{}},
		{"repo then positional then lens", []string{"--repo", "x", "http://h", "--lens", "e"}, Scope{Repo: "x", RepoFlag: true}},
		{"double dash", []string{"--", "--repo", "x"}, Scope{}},
		{"lone dash is positional", []string{"-", "--repo", "x"}, Scope{}},
		// --log is peeled by the bridge wherever it appears, value included.
		{"log value is not positional", []string{"--log", "/tmp/kb.log", "--repo", "x"}, Scope{Repo: "x", RepoFlag: true}},
		{"log equals", []string{"-log=/tmp/kb.log", "--lens", "e"}, Scope{Lens: "e", LensFlag: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyArgs(tc.args); got != tc.want {
				t.Errorf("ClassifyArgs(%q) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

func TestScope_Unbound(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{nil, true},
		{[]string{"http://host:8080"}, true},
		{[]string{"http://host:8080", "--repo", "x"}, true},
		{[]string{"--repo", "x"}, false},
		{[]string{"--repo"}, false},
		{[]string{"-lens"}, false},
	} {
		if got := ClassifyArgs(tc.args).Unbound(); got != tc.want {
			t.Errorf("ClassifyArgs(%q).Unbound() = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// SingleScope is the one rule both hosts use to reduce a config's knomit
// entries to a scope: for the hooks' binding, and for whether a flagless init
// keeps what is there. Only one usable scope is kept.
func TestSingleScope(t *testing.T) {
	valid := func(s string) bool { return s != "bad" }
	for _, tc := range []struct {
		name      string
		entries   [][]string
		wantScope Scope
		wantSkip  string
	}{
		{"none", nil, Scope{}, SkipNoBinding},
		{"repo", [][]string{{"--repo", "a"}}, Scope{Repo: "a", RepoFlag: true}, ""},
		{"lens", [][]string{{"--lens", "e"}}, Scope{Lens: "e", LensFlag: true}, ""},
		{"same scope twice", [][]string{{"--repo", "a"}, {"--repo=a"}}, Scope{Repo: "a", RepoFlag: true}, ""},
		{"unbound", [][]string{{}}, Scope{}, SkipUnbound},
		{"two scopes", [][]string{{"--repo", "a"}, {"--repo", "b"}}, Scope{}, SkipAmbiguous},
		{"unbound beside repo", [][]string{{}, {"--repo", "a"}}, Scope{}, SkipAmbiguous},
		{"degenerate lens beside unbound", [][]string{{}, {"--lens"}}, Scope{}, SkipAmbiguous},
		{"degenerate lens", [][]string{{"--lens"}}, Scope{}, SkipLensUnusable},
		{"degenerate repo", [][]string{{"--repo"}}, Scope{}, SkipNoBinding},
		{"invalid repo", [][]string{{"--repo", "bad"}}, Scope{}, SkipInvalidScope},
		{"invalid lens", [][]string{{"--lens", "bad"}}, Scope{}, SkipInvalidScope},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, skip := SingleScope(tc.entries, valid)
			if s != tc.wantScope || skip != tc.wantSkip {
				t.Errorf("SingleScope(%q) = (%+v, %q), want (%+v, %q)", tc.entries, s, skip, tc.wantScope, tc.wantSkip)
			}
		})
	}
}

func TestScope_String(t *testing.T) {
	for _, tc := range []struct {
		s    Scope
		want string
	}{
		{Scope{Repo: "a", RepoFlag: true}, "repo a"},
		{Scope{Lens: "e", LensFlag: true}, "lens e"},
		{Scope{}, "unbound"},
	} {
		if got := tc.s.String(); got != tc.want {
			t.Errorf("%+v.String() = %q, want %q", tc.s, got, tc.want)
		}
	}
}
