package repos

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// The chicken-and-egg of a fresh instance (2026-10-17): it booted with no
// [tls].addr, Settings → Network set the Fleet port and reloaded, so the
// listeners now have an address and wait for the certificate a create
// installs (no_certificate). The Manager judged the hub listener by the BOOT
// config, so GET /fleet said listener_on false and the create was refused
// listener_required: the create that installs the certificate the listener
// needs could not run until the listener ran. The live listener state is
// what decides (SetTLSListener reports the configured address too).
// SABOTAGE: tlsAddr() returns m.deps.Cfg.TLS.Addr -> listener_on false and
// the create is refused listener_required -> red.
func TestFleetHub_ListenerConfiguredByAReloadCountsBeforeTheCertificate(t *testing.T) {
	ctx := context.Background()
	m := newFleetHost(t, t.TempDir(), func(d *Deps) { d.Cfg.TLS.Addr = "" })
	m.SetTLSListener(func() (string, string, string) { return "0.0.0.0:19279", "", "no_certificate" })

	st, err := m.FleetStatus(ctx)
	require.NoError(t, err)
	require.True(t, st.Host.ListenerOn, "a reload configured the listener")
	require.Equal(t, "0.0.0.0:19279", st.Host.Listener)
	require.False(t, st.Host.Listening)
	require.Equal(t, "no_certificate", st.Host.ListenerReason)
	require.NotEmpty(t, st.Host.WouldServe, "the standalone offer names where members would join")

	_, err = m.CreateFleet(ctx, FleetCreateSpec{})
	require.NoError(t, err, "the create installs the certificate the listener waits for; it must not wait for the listener")
}

// The reverse: a reload that switched the listener OFF ([tls].addr removed)
// after a boot that had one. The boot config still names an address; the
// running process has none, and a fleet hosted here would be unreachable.
// SABOTAGE: tlsAddr() returns m.deps.Cfg.TLS.Addr -> listener_on true and
// the create succeeds -> red.
func TestFleetHub_ListenerSwitchedOffByAReloadIsOff(t *testing.T) {
	ctx := context.Background()
	m := newFleetHost(t, t.TempDir(), nil)
	require.NotEmpty(t, m.deps.Cfg.TLS.Addr, "fixture: the boot config has [tls].addr")
	m.SetTLSListener(func() (string, string, string) { return "", "", "" })

	st, err := m.FleetStatus(ctx)
	require.NoError(t, err)
	require.False(t, st.Host.ListenerOn)
	require.Empty(t, st.Host.Listener)

	_, err = m.CreateFleet(ctx, FleetCreateSpec{})
	_, code := fleetCode(t, err)
	require.Equal(t, "listener_required", code)
}
