package app

import (
	"context"
	"errors"

	"github.com/rs/zerolog/log"
)

// StartTLSWhenInstalled starts the mTLS listener that is configured but did
// not start for lack of a certificate, once one is installed: what a fleet
// create, join or host-here needs, so members can reach this instance without
// a restart or a `knomit reload`.
//
// It is NARROWER than a reload, and that is why it may be reached from the
// fleet operations (repos.Manager.SetTLSStarter) while Apply and Reload may
// not (test/archtest TestListenersApplyHasOnlyTheSignalLoopAsCaller): it
// reads no configuration, it never closes, moves or re-binds a listener, and
// it does anything only when ALL hold: the server is not closing, no mTLS
// listener is up, [tls].addr is set in the [tls] config the listeners
// reflect (the boot's, or the last reload's), and the listener is down with
// reason no_certificate. Then it opens exactly that listener, with that
// address and directory, as a reload would, and rewrites server.json with the
// generation advanced. Every other state is left alone and answers false: an
// address held by another process, or a certificate that does not load,
// still waits for the operator. Serialized with Apply.
func (l *Listeners) StartTLSWhenInstalled() (started bool, err error) {
	l.applyMu.Lock()
	defer l.applyMu.Unlock()
	l.mu.Lock()
	closed, running, state, tcfg := l.closed, l.TLS != nil, l.TLSState, l.tlsCfg
	l.mu.Unlock()
	if closed || running || tcfg.Addr == "" || state.Reason != ListenerReasonNoCertificate {
		return false, nil
	}
	if l.opts.Serve == nil {
		return false, errors.New("this Listeners cannot start a listener after boot (no Serve hook)")
	}
	tctx, cancel := context.WithCancel(l.opts.Base)
	srv, ln, err := OpenTLSServer(tctx, tcfg, l.opts.KeyPath, l.HTTP)
	if err != nil || srv == nil {
		cancel()
		reason := ListenerReasonNoCertificate
		if err != nil {
			reason = ListenerReasonTLSFailed
			if errors.Is(err, ErrTLSAddrInUse) {
				reason = ListenerReasonAddrInUse
			}
			log.Error().Err(err).Str("addr", tcfg.Addr).Msg("mTLS listener not started after the certificate was installed")
		}
		l.noteState("tls", ListenerState{Configured: tcfg.Addr, Reason: reason})
		return false, err
	}
	l.installTLS(srv, ln, cancel, tcfg)
	l.mu.Lock()
	l.generation++
	info := l.lockInfoLocked()
	wrote := l.wroteLock
	l.mu.Unlock()
	if wrote {
		if lerr := l.rewriteLock(info); lerr != nil {
			log.Error().Err(lerr).Str("path", l.lockPath).Msg("mTLS listener started; server.json not rewritten")
		}
	}
	log.Info().Str("tls", "https://"+ln.Addr().String()).Msg("mTLS listener started: the certificate it needs is installed")
	return true, nil
}
