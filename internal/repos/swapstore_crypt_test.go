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
// dropping the Crypt (and signer) that rewireStore/configureCrypt should have
// wired. A crypt-less store makes SetRemote REFUSE to persist any auth token,
// so the origin-connect "save remote config" step failed right after the
// disjoint apply swapped the clone in.
//
// A build-time preset repo (no control.db origin) no longer gets a Crypt from
// openStore at all — since Task 4, credential decryption for an injected
// origin happens once in control.db, and the store needs no Crypt of its own
// for that path; only initClone and SwapStore's rewireStore still wire one, for
// the legacy per-repo `remotes` write path SetRemote uses. So the precondition
// here is that storing a credential FAILS before the swap; after the fix, the
// swapped-in store can still store an encrypted credential because rewireStore
// wires a fresh Crypt on every reopen.
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

	storeCred := func(svc *store.Service) error {
		return svc.Remote().SetRemote(
			"origin", "https://example.test/repo.git", "main", "machine/test",
			300, 300, "token", "secret-token")
	}
	var svc *store.Service
	ri.WithRead(func(s *store.Service) { svc = s })
	require.Error(t, storeCred(svc),
		"precondition: a build-time preset repo has no Crypt, so storing a credential must refuse rather than write plaintext")

	// Build a valid replacement store (mirrors the transient origin clone) and
	// swap it in, exactly as handleCommit's disjoint-apply path does.
	tempDBPath := filepath.Join(t.TempDir(), "clone.db")
	clone, err := store.Open(tempDBPath)
	require.NoError(t, err)
	require.NoError(t, clone.InitRepo(map[string]string{}, "machine/test"))
	require.NoError(t, clone.Checkpoint())
	require.NoError(t, clone.Close())

	require.NoError(t, m.SwapStore(ri, tempDBPath))

	// After the swap, the store must be able to persist an encrypted
	// credential — i.e. rewireStore wired a Crypt on reopen.
	ri.WithRead(func(s *store.Service) { svc = s })
	require.NoError(t, storeCred(svc),
		"SwapStore must wire Crypt so the origin-connect credential save succeeds")
}
