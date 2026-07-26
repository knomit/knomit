package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// redactURL must strip secrets without destroying a URL's usability. An http(s)
// token can appear as EITHER the username or the password, so the whole
// userinfo goes. An ssh username is a LOGIN NAME, not a secret — stripping it
// would publish a source URL that no longer works.
func TestRedactURL(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"https user and password", "https://me:tok@github.com/me/kb.git", "https://github.com/me/kb.git"},
		{"https token as username", "https://TOKEN@github.com/me/kb.git", "https://github.com/me/kb.git"},
		{"bitbucket token user", "https://x-token-auth:tok@bitbucket.org/me/kb.git", "https://bitbucket.org/me/kb.git"},
		{"scp-like ssh untouched", "git@github.com:me/kb.git", "git@github.com:me/kb.git"},
		{"ssh url keeps login name", "ssh://git@github.com/me/kb.git", "ssh://git@github.com/me/kb.git"},
		{"ssh password dropped", "ssh://git:secret@github.com/me/kb.git", "ssh://git@github.com/me/kb.git"},
		{"anonymous knomit untouched", "http://localhost:19278/git/knomit-kb", "http://localhost:19278/git/knomit-kb"},
		{"local path untouched", "/srv/kb-mirror.git", "/srv/kb-mirror.git"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, redactURL(tc.in))
		})
	}
}

// A credential-bearing source URL must never reach the COMMITTED config —
// .knomit-okf.yaml is pushed, so a token there is a published token.
func TestRenderFiles_PublishSourceStripsCredentials(t *testing.T) {
	kbDir, kbRepo := newKB(t)
	_ = kbDir
	head, err := kbRepo.Head()
	require.NoError(t, err)

	var out bytes.Buffer
	files, _, err := renderFiles(exportRequest{
		repo: kbRepo, dir: t.TempDir(), branch: head.Name().Short(), head: head.Hash(),
		source: "https://me:supersecret@github.com/me/kb.git", publishSource: true,
		ui: newUI(&out),
	})
	require.NoError(t, err)

	cfg := string(files[configFile])
	require.NotContains(t, cfg, "supersecret", "the committed config must not carry a token")
	require.Contains(t, cfg, "source: https://github.com/me/kb.git")
}
