package repos

import (
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"knomit/internal/store/verify"

	"github.com/stretchr/testify/require"

	"knomit/internal/foundation/pki"
)

// The classes of a refused fleet sync, from the recorded error's text and
// this instance's own standing. The texts are the ones the transport really
// records (pinned in internal/foundation/pki's TestGitTransport_RevokedClient... and
// ...OtherRootServerIsRefused).
// SABOTAGE: classify every ErrRefusedByPeer as revoked (drop the standing
// check) -> the expired row is red; drop the bad_certificate condition -> the
// handshake-failure row is red; drop the judge exclusion -> the commit row is
// red.
func TestClassifyFleetRefusal(t *testing.T) {
	refusedBad := "push agent branch: " + pki.ErrRefusedByPeer.Error() + ": remote error: tls: bad certificate"
	refusedOther := "push agent branch: " + pki.ErrRefusedByPeer.Error() + ": remote error: tls: handshake failure"
	untrusted := "sync: " + pki.ErrUntrustedRoot.Error() + ": x509: certificate signed by unknown authority"
	for _, tc := range []struct {
		name, err string
		own       pki.Standing
		want      string
	}{
		{"bad_certificate, valid here: revoked", refusedBad, pki.StandingValid, FleetRefusalRevoked},
		{"bad_certificate, the installed CRL lists it", refusedBad, pki.StandingRevoked, FleetRefusalRevoked},
		{"bad_certificate, expired here", refusedBad, pki.StandingExpired, FleetRefusalOther},
		{"bad_certificate, not of the installed root", refusedBad, pki.StandingUntrusted, FleetRefusalOther},
		{"another alert (a host with nothing installed)", refusedOther, pki.StandingValid, FleetRefusalOther},
		{"this instance refused the host's certificate", untrusted, pki.StandingValid, FleetRefusalMoved},
		{"a commit the host's judge refused", verify.CommitRefusedByJudge + ": rule B: " + pki.ErrUntrustedRoot.Error(), pki.StandingValid, ""},
		{"no error", "", pki.StandingValid, ""},
		{"an unreachable host", "push: dial tcp 127.0.0.1:1: connect: connection refused", pki.StandingValid, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, classifyFleetRefusal(tc.err, tc.own))
		})
	}
}

// countAttempts counts the fetch and push attempts of each repo's origin
// rounds.
func countAttempts(t *testing.T) func(repo string) int {
	var mu sync.Mutex
	n := map[string]int{}
	setSyncHooks(t, syncHooks{attempt: func(repo, step string) {
		mu.Lock()
		n[repo]++
		mu.Unlock()
	}})
	return func(repo string) int {
		mu.Lock()
		defer mu.Unlock()
		return n[repo]
	}
}

// A forge member that is revoked learns it from main (its round installs the
// CRL that lists its own certificate), says so (GET /fleet revoked,
// sync_stopped) and stops its rounds: the next one fetches and pushes
// nothing, while another member's still does. Its leave and rejoin (a new
// certificate) run them again.
// SABOTAGE: drop noteFleetRevoked from the tick -> bravo's next round
// attempts -> red; drop the serial comparison in fleetSyncHalt -> the
// rejoined bravo never syncs -> red; Revoked from record_state only -> still
// green here (main's CRL says it too), see the hub test in cmd for the case
// only the refusal can show.
func TestFleetRefusal_ARevokedForgeMemberStopsItsRounds(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	installMasterLeaf(t, f)
	alpha := f.namedMember(t, "alpha")
	bravo := f.namedMember(t, "bravo")
	f.enrollAndJoin(t, alpha, "alpha")
	f.enrollAndJoin(t, bravo, "bravo")
	f.masterRounds(t)
	attempts := countAttempts(t)

	_, err := f.master.RevokeFleetMember(ctx, "bravo", "test")
	require.NoError(t, err)
	st, err := bravo.FleetStatus(ctx)
	require.NoError(t, err)
	require.False(t, st.Revoked, "fixture: bravo has not fetched the revocation yet")
	require.False(t, st.SyncStopped)

	bri := bravo.fleetRepo()
	roundOnPush(t, bri, bravo.fleetPushed)
	require.Greater(t, attempts(bri.Name()), 0, "fixture: the round that learns it ran")
	st, err = bravo.FleetStatus(ctx)
	require.NoError(t, err)
	require.True(t, st.Revoked, "the installed CRL lists bravo's certificate")
	require.True(t, st.SyncStopped, "and its rounds stop")
	require.Equal(t, "revoked", st.RecordState, "main's CRL says so too")

	before := attempts(bri.Name())
	roundOnPush(t, bri, bravo.fleetPushed)
	require.Equal(t, before, attempts(bri.Name()), "a stopped round fetches and pushes nothing")
	ari := alpha.fleetRepo()
	aBefore := attempts(ari.Name())
	roundOnPush(t, ari, alpha.fleetPushed)
	require.Greater(t, attempts(ari.Name()), aBefore, "positive control: alpha's round still runs")
	ast, err := alpha.FleetStatus(ctx)
	require.NoError(t, err)
	require.False(t, ast.Revoked)
	require.False(t, ast.SyncStopped)

	// Leave (offline is fine), then rejoin with a new certificate: the
	// rounds run again.
	_, err = bravo.LeaveFleet(ctx)
	require.NoError(t, err)
	f.enrollAndJoin(t, bravo, "bravo")
	bri = bravo.fleetRepo()
	before = attempts(bri.Name())
	roundOnPush(t, bri, bravo.fleetPushed)
	require.Greater(t, attempts(bri.Name()), before, "a new certificate syncs again")
	st, err = bravo.FleetStatus(ctx)
	require.NoError(t, err)
	require.False(t, st.Revoked)
	require.False(t, st.SyncStopped)
}

// The master is never stopped, and never reads as revoked, whatever its
// recorded error says: it holds the fleet's root.
// SABOTAGE: drop the root-holder check from fleetRevokedHere -> red.
func TestFleetRefusal_TheRootHolderIsNeverHalted(t *testing.T) {
	f := newForgeFleet(t, nil)
	installMasterLeaf(t, f)
	refused := "push: " + pki.ErrRefusedByPeer.Error() + ": remote error: tls: bad certificate"
	revoked, _ := f.master.fleetRevokedHere(refused)
	require.False(t, revoked)
	f.master.noteFleetRevoked(refused)
	require.Empty(t, f.mri.fleetSyncHalt())
	// Positive control: the same text on a member with a valid certificate.
	alpha := f.namedMember(t, "alpha")
	f.enrollAndJoin(t, alpha, "alpha")
	revoked, _ = alpha.fleetRevokedHere(refused)
	require.True(t, revoked)
}

// GET /fleet certificate_expired (#536 review SF1, the user's copy split):
// false while the installed certificate is valid here, true once it is past
// its NotAfter by this clock.
// SABOTAGE: CertificateExpired = false always -> red.
func TestFleetStatus_CertificateExpiredIsThisClocksVerdict(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	alpha := f.namedMember(t, "alpha")
	f.enrollAndJoin(t, alpha, "alpha")
	st, err := alpha.FleetStatus(ctx)
	require.NoError(t, err)
	require.False(t, st.CertificateExpired, "fixture: a fresh certificate")

	cur, err := pki.LoadInstanceCert(alpha.deps.Cfg.TLS.Dir)
	require.NoError(t, err)
	old, err := pki.IssueLeaf(f.root.Root, cur.PublicKey.(ed25519.PublicKey), "alpha@"+pki.AgentEmailDomain, time.Minute, time.Now().Add(-2*time.Hour))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(alpha.deps.Cfg.TLS.Dir, pki.InstanceCertFile), old.CertPEM, 0o600))
	st, err = alpha.FleetStatus(ctx)
	require.NoError(t, err)
	require.True(t, st.CertificateExpired, "an installed certificate past its NotAfter")
}
