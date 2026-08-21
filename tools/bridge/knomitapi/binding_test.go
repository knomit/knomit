package knomitapi

import "testing"

func TestIsKnomitCommand(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want bool
	}{
		{"knomit-bridge", true},
		{"/usr/local/bin/knomit-bridge", true},
		{"knomit-bridge.exe", true},
		// Note: a `C:\...\knomit-bridge.exe` style path is only split correctly
		// by filepath.Base on Windows. That is fine — the config is written and
		// read on the same machine — so there is no cross-platform row here.
		{"knomit-bridge-dev", false}, // a wrapper/dev build is NOT a command match
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

// The lensMode tri-state is the whole point of this function: a degenerate
// --lens must stay distinguishable from "no knomit target at all", or a caller
// will fall through to a repo scope and read the wrong knowledge base.
func TestClassifyArgs_LensModeIsStickyEvenWhenUnusable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantRepo string
		wantLens string
		wantMode bool
	}{
		{"repo mode", []string{"--repo", "proj"}, "proj", "", false},
		{"repo equals form", []string{"--repo=proj"}, "proj", "", false},
		{"lens mode", []string{"--lens", "eng"}, "", "eng", true},
		{"lens equals form", []string{"--lens=eng"}, "", "eng", true},
		{"lens wins after repo", []string{"--repo", "proj", "--lens", "eng"}, "", "eng", true},
		{"lens wins before repo", []string{"--lens", "eng", "--repo", "proj"}, "", "eng", true},

		// The regression this tri-state exists for: each of these is lens mode
		// with an unusable name. Returning lensMode=false here is what let a
		// sibling --repo entry win.
		{"lens token, no value", []string{"--lens"}, "", "", true},
		{"lens equals empty", []string{"--lens="}, "", "", true},
		{"lens token, no value, after repo", []string{"--repo", "proj", "--lens"}, "", "", true},

		{"no knomit flags at all", []string{"--verbose"}, "", "", false},
		{"empty args", nil, "", "", false},

		// A flag-shaped token must never be consumed as a repo NAME.
		{"repo followed by another flag", []string{"--repo", "--repo", "proj"}, "proj", "", false},
		{"repo followed by lens", []string{"--repo", "--lens", "eng"}, "", "eng", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, lens, mode := ClassifyArgs(tc.args)
			if repo != tc.wantRepo || lens != tc.wantLens || mode != tc.wantMode {
				t.Errorf("ClassifyArgs(%v) = (%q,%q,%v), want (%q,%q,%v)",
					tc.args, repo, lens, mode, tc.wantRepo, tc.wantLens, tc.wantMode)
			}
		})
	}
}
