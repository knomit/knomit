package repos

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// countStarts installs a TLS starter on m that counts its calls.
func countStarts(m *Manager) *atomic.Int32 {
	var n atomic.Int32
	m.SetTLSStarter(func() (bool, error) { n.Add(1); return true, nil })
	return &n
}

// The listener auto-start (PR8 item 3): a fleet create, a join and a
// host-here that SUCCEED each ask the TLS starter once (cmd/serve.go hands it
// app.Listeners.StartTLSWhenInstalled); a refused one asks nothing.
// SABOTAGE: drop startTLSAfterInstall from CreateFleet / JoinFleet /
// HostFleetHere -> that row's count is 0 -> red; call it before the refusals
// return -> the refused rows count 1 -> red.
func TestFleetListenerStart_AfterASuccessfulCreateJoinOrHost(t *testing.T) {
	ctx := context.Background()

	host := newFleetHost(t, t.TempDir(), nil)
	n := countStarts(host)
	_, err := host.CreateFleet(ctx, FleetCreateSpec{})
	require.NoError(t, err)
	require.Equal(t, int32(1), n.Load(), "a create asks once")

	f := newForgeFleet(t, nil)
	alpha := f.namedMember(t, "alpha")
	na := countStarts(alpha)
	enr, err := f.master.EnrollFleetMember(ctx, "bravo", keyLine("bravo"), "")
	require.NoError(t, err)
	_, err = alpha.JoinFleet(ctx, FleetJoinSpec{Bundle: []byte(enr.Bundle)})
	require.Error(t, err, "fixture: bravo's bundle is not alpha's")
	require.Equal(t, int32(0), na.Load(), "a refused join asks nothing")
	f.enrollAndJoin(t, alpha, "alpha")
	require.Equal(t, int32(1), na.Load(), "a join asks once")

	nm := countStarts(f.master)
	_, code := fleetCode(t, hostHere(ctx, f.master))
	require.Equal(t, "listener_required", code)
	require.Equal(t, int32(0), nm.Load(), "a refused host asks nothing")
	f.master.deps.Cfg.TLS.Addr = "127.0.0.1:0"
	f.master.deps.Cfg.ExternalAddresses = []string{"https://master.example"}
	require.NoError(t, hostHere(ctx, f.master))
	require.Equal(t, int32(1), nm.Load(), "a host-here asks once")
}
