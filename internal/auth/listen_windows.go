//go:build windows

package auth

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// listenLocal opens the Windows half of ListenLocal: a named pipe whose SDDL
// is the credential. See that doc for why there is no lock file here.
func listenLocal(path string) (net.Listener, func(), error) {
	noop := func() {}
	if !strings.HasPrefix(path, PipePrefix) {
		// An ERROR and not a silent nil. CreateFile would open an ordinary
		// path as a FILE, so a server handed one would report a listener and
		// accept nothing — the shape of failure this whole ticket exists to
		// remove.
		return nil, noop, fmt.Errorf("local listener path %q is not a named pipe (it must start with %s): "+
			"on Windows the local authenticated listener is a pipe, and anything else would open a FILE "+
			"that no client could be authenticated over", path, PipePrefix)
	}
	sddl, err := ownerOnlySDDL()
	if err != nil {
		return nil, noop, err
	}
	ln, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: sddl})
	if err != nil {
		if isPipeNameTaken(err) {
			return nil, noop, fmt.Errorf("%w: %s", ErrSocketInUse, path)
		}
		return nil, noop, fmt.Errorf("listen on named pipe %s: %w", path, err)
	}
	var once sync.Once
	cleanup := func() {
		once.Do(func() { ln.Close() })
	}
	return ln, cleanup, nil
}

// isPipeNameTaken reports whether err is "another process already owns this
// pipe name", which is the Windows spelling of ErrSocketInUse.
//
// winio.ListenPipe creates with FILE_FLAG_FIRST_PIPE_INSTANCE, so the OS
// refuses a second listener on a live name, and the refusal is
// ERROR_ACCESS_DENIED — measured on this hardware, not inferred; see
// TestListenLocal_LivePipeIsNotStolen, which records the errno it actually
// got so a future Windows build changing it fails loudly.
//
// ERROR_PIPE_BUSY is included because the name is equally taken when every
// instance is momentarily in use; it is not a case knomit's single-instance
// listener produces, but treating it as "free" would be wrong in exactly the
// dangerous direction.
//
// This is a CLASS of failure, not a synonym for "denied". A pipe we are not
// allowed to create for some other reason lands in the generic branch above
// and is fatal, which is right: the difference that matters to the caller is
// "somebody else is serving here" versus "this machine cannot serve here".
func isPipeNameTaken(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_PIPE_BUSY)
}
