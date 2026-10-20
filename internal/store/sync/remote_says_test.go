package sync

import "testing"

// A push that a server REFUSES explains itself on the sideband, prefixed
// "remote: ". Dropping those lines left a reader with go-git's summary alone —
// "pre-receive hook declined" — which names the mechanism and never the
// reason: protected branch, push rule, unsigned commit, oversized file.
func TestRemoteSays(t *testing.T) {
	cases := []struct {
		name     string
		sideband string
		want     string
	}{
		{
			// The real answer from GitLab for a refused initial commit: prose
			// plus a list of remedies, wrapped. Folding it onto one line is
			// what turned guidance into a run-on.
			name:     "multi-line guidance keeps its shape",
			sideband: "remote: GitLab:\nremote: You cannot push the initial commit because the default branch is\nremote: protected and your role does not allow it.\nremote: To resolve this, either:\nremote: - Ask a Maintainer to push the initial commit.\n",
			want:     "\nGitLab:\nYou cannot push the initial commit because the default branch is\nprotected and your role does not allow it.\nTo resolve this, either:\n- Ask a Maintainer to push the initial commit.",
		},
		{
			name:     "gitlab frames its reason with banner lines",
			sideband: "remote: \nremote: GitLab: You are not allowed to push code to protected branches on this project.\nremote: \n",
			want:     "\nGitLab: You are not allowed to push code to protected branches on this project.",
		},
		{
			name:     "progress uses carriage returns, not newlines",
			sideband: "remote: Resolving deltas:   0%\rremote: Resolving deltas: 100%\rremote: GitLab: Commit message does not match\n",
			want:     "\nResolving deltas:   0%\nResolving deltas: 100%\nGitLab: Commit message does not match",
		},
		{
			name:     "a repeated line is said once",
			sideband: "remote: same\nremote: same\nremote: same\n",
			want:     "\nsame",
		},
		{
			name:     "silence adds nothing to the error",
			sideband: "",
			want:     "",
		},
		{
			name:     "blank sideband adds nothing either",
			sideband: "remote: \nremote:\n\n",
			want:     "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := remoteSays(c.sideband); got != c.want {
				t.Fatalf("remoteSays()\n got: %q\nwant: %q", got, c.want)
			}
		})
	}
}
