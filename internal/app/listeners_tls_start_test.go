//go:build !windows

package app

import (
	"net"
	"testing"

	"knomit/internal/pki/pkitest"
)

// StartTLSWhenInstalled: a server that booted with [tls].addr set and no
// certificate (listener down, no_certificate) starts nothing while nothing
// is installed; once a certificate is, ONE call binds the configured address
// (re-bind check), TLSListening and server.json say so with the generation
// advanced; a second call is a no-op. A server with no [tls].addr starts
// nothing even with a certificate installed.
// SABOTAGE: (a listening listener has no reason, so the reason check is what
// keeps the second call a no-op) skip the server.json rewrite -> the generation
// assert is red. (The no_certificate condition: the next test.)
func TestStartTLSWhenInstalled_StartsTheConfiguredListenerOnceACertificateIsInstalled(t *testing.T) {
	f := newFixture(t)
	f.keyPath, _ = pkitest.NewKey(t)
	f.cfg.TLS.Addr = f.tlsAddr
	r := newRig(t, f)
	if l, why := r.ls.TLSListening(); l != "" || why != ListenerReasonNoCertificate {
		t.Fatalf("fixture: TLSListening = %q, %q; want down with no_certificate", l, why)
	}
	gen := r.lockInfo(t).Generation

	if started, err := r.ls.StartTLSWhenInstalled(); started || err != nil {
		t.Fatalf("nothing installed: started=%v err=%v", started, err)
	}
	requireFree(t, f.tlsAddr)

	fleet := pkitest.New(t)
	fleet.Install(t, fleet.Enroll(t, "serve@agents.knomit.io", f.keyPath), f.cfg.TLS.Dir)
	started, err := r.ls.StartTLSWhenInstalled()
	if !started || err != nil {
		t.Fatalf("certificate installed: started=%v err=%v", started, err)
	}
	requireBound(t, f.tlsAddr)
	if l, why := r.ls.TLSListening(); l != f.tlsAddr || why != "" {
		t.Fatalf("TLSListening = %q, %q; want %s", l, why, f.tlsAddr)
	}
	info := r.lockInfo(t)
	if info.Generation != gen+1 || info.Listeners["tls"].Listening != f.tlsAddr {
		t.Fatalf("server.json generation %d (was %d), tls %+v", info.Generation, gen, info.Listeners["tls"])
	}
	if started, err := r.ls.StartTLSWhenInstalled(); started || err != nil {
		t.Fatalf("already listening: started=%v err=%v", started, err)
	}
	if r.lockInfo(t).Generation != gen+1 {
		t.Fatal("a no-op call advanced server.json's generation")
	}
	r.requireNoServeFailure(t)
}

// It is not a reload: a listener that is down for another reason than a
// missing certificate (here an address another process held at boot,
// supervised) stays down when the address frees up; the operator's reload
// starts it.
// SABOTAGE: drop the no_certificate condition -> it binds -> red.
func TestStartTLSWhenInstalled_OnlyForAMissingCertificate(t *testing.T) {
	f := newFixture(t)
	f.withCert(t)
	held, err := net.Listen("tcp", f.tlsAddr)
	if err != nil {
		t.Fatal(err)
	}
	r := newRigOpts(t, f, func(o *ListenerOptions) { o.Supervised = true })
	if l, why := r.ls.TLSListening(); l != "" || why != ListenerReasonAddrInUse {
		t.Fatalf("fixture: TLSListening = %q, %q; want down with addr_in_use", l, why)
	}
	held.Close()
	if started, err := r.ls.StartTLSWhenInstalled(); started || err != nil {
		t.Fatalf("addr_in_use at boot: started=%v err=%v", started, err)
	}
	requireFree(t, f.tlsAddr)
}

func TestStartTLSWhenInstalled_NoAddressStartsNothing(t *testing.T) {
	f := newFixture(t)
	f.withCert(t)
	f.cfg.TLS.Addr = "" // the certificate is there, the listener is off by config
	r := newRig(t, f)
	if started, err := r.ls.StartTLSWhenInstalled(); started || err != nil {
		t.Fatalf("no [tls].addr: started=%v err=%v", started, err)
	}
	requireFree(t, f.tlsAddr)
}
