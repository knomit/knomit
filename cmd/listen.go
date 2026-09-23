package cmd

import (
	"errors"
	"net"

	"github.com/rs/zerolog/log"

	"knomit/internal/auth"
	"knomit/internal/config"
)

// openLocalListener opens the local authenticated listener for cfg and decides
// whether this process may serve without one.
//
// It takes the WHOLE CONFIG rather than a path and a flag. That is the point
// of it existing: serve used to pass `cfg.Socket` and `cfg.Auth.Require` as
// two separate arguments at the call site, and nothing pinned that they were
// the right two — mutating the flag to a constant left every test green. There
// is now one argument and no pair to wire up the wrong way round, and the
// policy below is exercised directly by listen_test.go.
//
// Three outcomes, and the middle one is the subtle case:
//
//   - a listener, which the caller serves alongside its TCP one;
//   - NO listener and NO error, because another process holds the path and
//     [auth].require is off: serve TCP only, which is right, since a second
//     instance must not steal the first one's listener;
//   - an error, which fails the boot.
//
// The cleanup is always non-nil, so the caller can defer it unconditionally.
func openLocalListener(cfg config.Config) (net.Listener, func(), error) {
	ul, closeSocket, err := auth.ListenLocal(cfg.Socket)
	switch {
	case errors.Is(err, auth.ErrSocketInUse):
		// Another process holds the path — normally the desktop app, but all
		// that is KNOWN is that it is held: on Windows the pipe namespace is
		// flat and world-creatable, so the owner need not be knomit at all.
		// Do not steal it either way: bridges belong to whatever was there
		// first.
		log.Warn().Err(err).Str("socket", cfg.Socket).
			Msg("local listener path is held by another process; serving TCP only")
	case errors.Is(err, auth.ErrPathTooLong), errors.Is(err, auth.ErrUnsafeSocketDir):
		// knomit#253: the path cannot be a unix socket (the error names its
		// length and this platform's cap), or it is in the shared fallback
		// directory and that directory is not private to this user. Neither
		// is a reason to refuse the boot: serve TCP only and say why. A long
		// DATA ROOT alone never lands here (config falls back to a short
		// path); an explicit overlong [socket] does.
		log.Warn().Err(err).Str("socket", cfg.Socket).
			Msg("no local authenticated listener; serving TCP only")
	case err != nil:
		closeSocket()
		return nil, func() {}, err
	}
	// ...unless serving TCP only would mean serving NOTHING. With
	// [auth].require = true there is no anonymous path, so carrying on past
	// the benign branch above would answer every request 403 while looking
	// healthy — the silent lockout app.checkLocalListener refuses at config
	// time, arriving through the one door it cannot see.
	//
	// err here is nil, ErrSocketInUse, ErrPathTooLong or ErrUnsafeSocketDir
	// -- anything else returned above -- so passing it straight through
	// names the right cause.
	if rerr := auth.RequireLocalListener(cfg.Auth.Require, ul, cfg.Socket, err); rerr != nil {
		closeSocket()
		return nil, func() {}, rerr
	}
	return ul, closeSocket, nil
}
