//go:build windows

package auth

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// LocalVia names the mechanism the local authenticated listener vouches with
// on this platform. On Windows that is a named pipe, whose ACL is the gate
// and whose ImpersonateNamedPipeClient answer is the identity.
//
// It is ViaPipe and not ViaSocket deliberately (knomit#245): the grants key
// is "<kind>:<id>@<via>", and a grants row or a log line has to be able to
// tell a SID read off a pipe from a uid read off a socket. Windows 10 1803+
// does accept net.Listen("unix", …), but AF_UNIX there has no SO_PEERCRED
// equivalent, so it could carry no verified identity at all — which is why
// the pipe, and why the two mechanisms are not spelled the same.
//
// See local_unix.go for why this is a per-platform constant rather than a
// parameter.
const LocalVia = ViaPipe

// localID formats a kernel-reported local identity as a Principal ID. There
// is exactly one spelling: LocalPrincipal feeds it this process's own SID and
// PeerCred feeds it the pipe client's, so the two cannot drift.
func localID(sid string) string { return "sid:" + sid }

// LocalPrincipal is the principal this process presents over the local
// listener. See the unix half for the invariant this exists to keep; the
// difference here is that the answer has to be ASKED of the OS, so this one
// can fail.
//
// The SID is the token USER's, which is stable across elevation: an elevated
// and a non-elevated shell for the same account produce the same SID (they
// differ in integrity level and group membership, not in who they are). That
// is what lets an elevated server seed a grant a non-elevated bridge can use.
func LocalPrincipal() (Principal, error) {
	sid, err := ownSID()
	if err != nil {
		return Principal{}, err
	}
	return Principal{Kind: KindBridge, ID: localID(sid), Via: LocalVia}, nil
}

// ownSID is this process's token user SID in string (S-1-5-21-…) form.
//
// GetCurrentProcessToken returns a PSEUDO-handle, which must not be closed —
// there is deliberately no Close here.
func ownSID() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("GetTokenUser for the current process: %w", err)
	}
	return u.User.Sid.String(), nil
}

// PipePrefix is the Windows named-pipe namespace prefix. It is an OS fact
// rather than a knomit one, and it appears twice on purpose: internal/config
// BUILDS the local listener path with it, and this package RECOGNISES the
// result. TestLocalListenerPath_IsAPipeConfigAgrees pins the two together, so
// a drift would fail a test rather than silently listen on a file.
const PipePrefix = `\\.\pipe\`

// ListenLocal opens the local authenticated listener at path. One function
// per platform, called by cmd/serve (and callable from the desktop boot), so
// that "what a local listener IS" is answered in one place rather than inline
// at each listen site.
//
// The PIPE'S ACL IS THE CREDENTIAL here, as the 0700 data root and the 0600
// socket are on unix: only this user and SYSTEM can open it at all. PeerCred
// is what then turns "you got through" into a named principal, so that the
// identity is asked of the OS rather than assumed from the ACL.
//
// A path that is not in the pipe namespace is REFUSED rather than passed
// through. CreateFile would happily open a regular file at an ordinary path,
// and a server listening on a file would accept nothing while looking like it
// had come up.
func ListenLocal(path string) (net.Listener, error) {
	if !strings.HasPrefix(path, PipePrefix) {
		return nil, fmt.Errorf("local listener path %q is not a named pipe (it must start with %s): "+
			"on Windows the local authenticated listener is a pipe, and anything else would open a FILE "+
			"that no client could be authenticated over", path, PipePrefix)
	}
	sddl, err := ownerOnlySDDL()
	if err != nil {
		return nil, err
	}
	l, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: sddl})
	if err != nil {
		return nil, fmt.Errorf("listen on named pipe %s: %w", path, err)
	}
	return l, nil
}

// ownerOnlySDDL grants generic-all to this process's user and to SYSTEM, and
// to nobody else.
//
// P (protected) matters: without it an inherited ACE could widen the pipe
// silently. There is deliberately no explicit DENY ace — a DACL with no
// matching ACE already denies, whereas an explicit deny for "everyone else"
// is evaluated first and would also lock US out if our own SID ever turned up
// inside the denied group.
//
// SYSTEM is included so that a service or an administrator's tooling can
// still reach the pipe; every other account, including another interactive
// user on the same machine, is refused by the OS before any knomit code runs.
func ownerOnlySDDL() (string, error) {
	sid, err := ownSID()
	if err != nil {
		return "", err
	}
	return "D:P(A;;GA;;;" + sid + ")(A;;GA;;;SY)", nil
}

// DialLocal dials the local authenticated listener at path. It is the other
// half of ListenLocal and the bridge's only door to it: keeping both here
// means the client and the server cannot come to disagree about what the
// transport is, the way they once disagreed about where the data root was.
//
// THE IMPERSONATION LEVEL IS THE WHOLE POINT OF NOT USING
// winio.DialPipeContext. That helper dials at PipeImpLevelAnonymous, and at
// SECURITY_ANONYMOUS the server's OpenThreadToken fails with
// ERROR_CANT_OPEN_ANONYMOUS: there is no identity to read, so PeerCred
// returns no peer and the caller silently becomes anonymous. Identification
// is the least level that lets the server learn WHO is calling while still
// refusing it the right to ACT as the caller.
func DialLocal(ctx context.Context, path string, timeout time.Duration) (net.Conn, error) {
	if !strings.HasPrefix(path, PipePrefix) {
		return nil, fmt.Errorf("local listener path %q is not a named pipe (it must start with %s): "+
			"dialling it as a file would open whatever happens to be there", path, PipePrefix)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return winio.DialPipeAccessImpLevel(ctx, path,
		uint32(windows.GENERIC_READ|windows.GENERIC_WRITE),
		winio.PipeImpLevelIdentification)
}
