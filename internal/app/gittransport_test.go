package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	transportclient "github.com/go-git/go-git/v5/plumbing/transport/client"

	"knomit/internal/pki"
)

// installGitTransport is the boot hook app.New calls before repos.New and
// Manager.Start. It is tested in isolation because a test through app.New
// needs the embedding model, and internal/app runs in no CI job (#254).
//
// An unenrolled instance still registers the scheme, and a fetch fails with
// ErrNotEnrolled rather than go-git's "unsupported scheme" — the named failure
// is the whole point. SABOTAGE: removing the pki.InstallGitTransport call
// turns this red with "unsupported scheme".
func TestInstallGitTransport_RegistersTheSchemeEvenUnenrolled(t *testing.T) {
	home := t.TempDir()
	installGitTransport(filepath.Join(home, "pki"), filepath.Join(home, "id_ed25519"))
	if _, ok := transportclient.Protocols[pki.GitScheme]; !ok {
		t.Fatalf("%s is not registered after boot", pki.GitScheme)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := gogit.PlainCloneContext(ctx, t.TempDir(), false, &gogit.CloneOptions{URL: pki.GitScheme + "://127.0.0.1:1/git/kb"})
	if !errors.Is(err, pki.ErrNotEnrolled) {
		t.Fatalf("err=%v, want ErrNotEnrolled", err)
	}
}
