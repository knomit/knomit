package repos

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/store"
)

// TestSwapStore_RestoresCryptForCredentialStorage regresses the bug where
// SwapStore reopened the store via store.Open but restored only the embedder —
// dropping the Crypt (and signer) that repoBuilder/configureCrypt had wired.
// A crypt-less store makes SetRemote REFUSE to persist any auth token, so the
// origin-connect "save remote config" step failed right after the disjoint
// apply swapped the clone in. After the fix, the swapped-in store can still
// store an encrypted credential.
func TestSwapStore_RestoresCryptForCredentialStorage(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_ed25519")
	writeTestKey(t, keyPath)

	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: "machine/test",
		KeyPath:     keyPath,
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	ri, err := m.Create(context.Background(), CreateSpec{
		Name: "work", Mode: "preset", OntologyPreset: "default",
	}, nil)
	require.NoError(t, err)

	// Sanity: at build time the Crypt is wired, so storing a credential works.
	storeCred := func(svc *store.Service) error {
		return svc.Remote().SetRemote(
			"origin", "https://example.test/repo.git", "main", "machine/test",
			300, 300, "token", "secret-token")
	}
	var svc *store.Service
	ri.WithRead(func(s *store.Service) { svc = s })
	require.NoError(t, storeCred(svc), "precondition: build-time store can store credentials")

	// Build a valid replacement store (mirrors the transient origin clone) and
	// swap it in, exactly as handleCommit's disjoint-apply path does.
	tempDBPath := filepath.Join(t.TempDir(), "clone.db")
	clone, err := store.Open(tempDBPath)
	require.NoError(t, err)
	require.NoError(t, clone.InitRepo(map[string]string{}, "machine/test"))
	require.NoError(t, clone.Checkpoint())
	require.NoError(t, clone.Close())

	require.NoError(t, m.SwapStore(ri, tempDBPath))

	// After the swap, the store must STILL be able to persist an encrypted
	// credential — i.e. the Crypt was re-wired on reopen.
	ri.WithRead(func(s *store.Service) { svc = s })
	require.NoError(t, storeCred(svc),
		"SwapStore must restore Crypt so the origin-connect credential save succeeds")
}
