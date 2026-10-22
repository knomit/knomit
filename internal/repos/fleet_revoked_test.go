package repos

import (
	"context"
	"crypto/ed25519"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/domain/fact"
	"knomit/internal/pki"
	"knomit/internal/pki/pkitest"
)

// pushToFleetRemote commits files (path -> content, on top of main) to the
// bare fleet remote's main.
func pushToFleetRemote(t *testing.T, bare string, files map[string][]byte) {
	t.Helper()
	work := t.TempDir()
	runGit(t, "", "clone", "-q", bare, work)
	for p, body := range files {
		require.NoError(t, os.MkdirAll(filepath.Join(work, filepath.Dir(p)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(work, p), body, 0o644))
	}
	runGit(t, work, "add", "-A")
	runGit(t, work, "-c", "user.email=h@h", "-c", "user.name=h", "commit", "-q", "-m", "fleet")
	runGit(t, work, "push", "-q", "origin", "main")
}

// activeRecordFile is an active member record for id holding pub, as the
// fleet code writes it.
func activeRecordFile(t *testing.T, id string, pub ed25519.PublicKey) []byte {
	t.Helper()
	k, err := ssh.NewPublicKey(pub)
	require.NoError(t, err)
	return []byte("---\nkind: pragmatic\ntype: policy\n---\n# member " + id + "\n\n" +
		fact.RenderMember(fact.Member{Agent: id, State: fact.MemberActive, Key: k, Host: "h"}))
}

// M6: "revoked" is DERIVED, never written. A fleet whose main holds two
// active records and both leaves; the master revokes this instance's leaf
// (a new crl.pem on main). Before the CRL lists it, FleetMembers and
// FleetStatus say active; after, at the revision whose CRL lists the serial,
// they say revoked for this instance only, while the record on main still
// says active (nothing wrote the word).
//
// SABOTAGE: drop the Revoked derivation in fleetMembersAtMain → both reads
// stay active → red; make FleetPKI.Revoked ignore the CRL (return true for any
// chaining leaf) → "other" reads revoked → red.
func TestFleet_RevokedIsDerivedFromTheCRLAtMain(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	bare := filepath.Join(dir, "fleet.git")
	url := seedFleetRemote(t, bare)
	root := pkitest.New(t)
	self := root.Enroll(t, "test-fleet@agents.knomit.io")
	other := root.Enroll(t, "other@agents.knomit.io")
	read := func(name string) []byte {
		raw, err := os.ReadFile(filepath.Join(root.Dir, name))
		require.NoError(t, err)
		return raw
	}
	selfRec := memberRecordPath("kb", "test-fleet")
	pushToFleetRemote(t, bare, map[string][]byte{
		"artifacts/fleet/" + pki.RootCertFile:  read(pki.RootCertFile),
		"artifacts/fleet/" + pki.CRLFile:       read(pki.CRLFile),
		"artifacts/fleet/certs/test-fleet.pem": self.CertPEM,
		"artifacts/fleet/certs/other.pem":      other.CertPEM,
		selfRec:                                activeRecordFile(t, "test-fleet", self.Pub),
		memberRecordPath("kb", "other"):        activeRecordFile(t, "other", other.Pub),
	})
	m := newFleetManager(t, dir)
	mountFleetByDoor(t, m, "fleet", url)

	states := func() map[string]string {
		t.Helper()
		ms, err := m.FleetMembers(ctx)
		require.NoError(t, err)
		out := map[string]string{}
		for _, mem := range ms {
			out[mem.Agent] = mem.State
		}
		return out
	}
	recordState := func() string {
		t.Helper()
		st, err := m.FleetStatus(ctx)
		require.NoError(t, err)
		return st.RecordState
	}
	require.Equal(t, map[string]string{"test-fleet": fact.MemberActive, "other": fact.MemberActive}, states())
	require.Equal(t, fact.MemberActive, recordState())

	root.Revoke(t, self)
	pushToFleetRemote(t, bare, map[string][]byte{"artifacts/fleet/" + pki.CRLFile: read(pki.CRLFile)})
	syncFleet(t, m)

	require.Equal(t, map[string]string{"test-fleet": fact.MemberRevoked, "other": fact.MemberActive}, states())
	require.Equal(t, fact.MemberRevoked, recordState())
	onMain, err := exec.Command("git", "-C", bare, "show", "main:"+selfRec).Output()
	require.NoError(t, err)
	require.Contains(t, string(onMain), "state: active", "the record as written is unchanged: revoked is derived")
}
