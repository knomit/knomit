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
// rather than a knomit one. internal/config BUILDS the local listener path
// with it and refuses an operator-named socket without it; this package
// RECOGNISES the result. TestSocketPath_IsOpenableAndDialableByAuth
// (internal/config) round-trips a real listener through both.
const PipePrefix = `\\.\pipe\`

// ownerOnlySDDL names this process's user as the pipe's OWNER, and grants
// generic-all to that user and to SYSTEM, and to nobody else.
//
// WHAT THIS IS AND IS NOT. The DACL controls who may OPEN knomit's pipe,
// which is the unix parallel to 0600 under a 0700 data root. It does NOT
// control who may CREATE a pipe by that name: \\.\pipe\ is world-creatable,
// so another local user can take the name before knomit boots. ListenLocal
// then fails to create the name and the server says so; the CLIENT is
// protected by DialLocal, which verifies the owner below before it writes a
// byte (knomit#265).
//
// THE O: PART IS WHAT MAKES THAT CHECK WORK. Without it the owner is the
// creating token's TokenOwner, which for a UAC-elevated server — the desktop,
// typically — is BUILTIN\Administrators, not the user (measured; the same
// finding as knomit#301's data directory). A token may always assign its own
// user SID as owner, so naming it here is accepted elevated or not, and the
// client can then expect its own SID.
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
	return "O:" + sid + "D:P(A;;GA;;;" + sid + ")(A;;GA;;;SY)", nil
}

// DialLocal dials the local authenticated listener at path. It is the other
// half of ListenLocal (listen.go, listen_windows.go) and the bridge's only
// door to it: keeping the pair in one package means the client and the server
// cannot come to disagree about what the transport is, the way they once
// disagreed about where the data root was.
//
// THE IMPERSONATION LEVEL IS THE WHOLE POINT OF NOT USING
// winio.DialPipeContext. That helper dials at PipeImpLevelAnonymous, and at
// SECURITY_ANONYMOUS the server's OpenThreadToken fails with
// ERROR_CANT_OPEN_ANONYMOUS: there is no identity to read, so PeerCred
// returns no peer and the caller silently becomes anonymous. Identification
// is the least level that lets the server learn WHO is calling while still
// refusing it the right to ACT as the caller.
//
// A connection is returned only once its OWNER is verified (verifyPipeOwner):
// anything else is closed before a byte is written, and a foreign owner comes
// back as ErrForeignListener.
func DialLocal(ctx context.Context, path string, timeout time.Duration) (net.Conn, error) {
	if !strings.HasPrefix(path, PipePrefix) {
		return nil, fmt.Errorf("local listener path %q is not a named pipe (it must start with %s): "+
			"dialling it as a file would open whatever happens to be there", path, PipePrefix)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := winio.DialPipeAccessImpLevel(ctx, path,
		uint32(windows.GENERIC_READ|windows.GENERIC_WRITE),
		winio.PipeImpLevelIdentification)
	if err != nil {
		return nil, err
	}
	if err := verifyPipeOwner(conn, path); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// acceptedPipeOwner reports whether a pipe owned by owner may be trusted by a
// client whose token user is self: this user, SYSTEM or
// BUILTIN\Administrators.
//
// BUILTIN\Administrators is what a UAC-elevated server that predates
// knomit#265 owns its pipe as, because its SDDL had no O: part. SYSTEM is
// NOT accepted so that a knomit run as a service is reachable — its pipe is
// DACL'd to its own SID and SYSTEM, so a user's client is refused with
// access denied before any owner check runs. It is accepted because it costs
// nothing: SYSTEM is admin-equivalent, only admin-level code can produce a
// SYSTEM-owned pipe, and an administrator is out of scope — it can already
// read this user's data root.
//
// It is a variable so a test can NARROW it: a pipe owned by another account
// cannot be created without privileges the suite does not have.
var acceptedPipeOwner = func(owner *windows.SID, self string) bool {
	return owner.String() == self ||
		owner.IsWellKnown(windows.WinLocalSystemSid) ||
		owner.IsWellKnown(windows.WinBuiltinAdministratorsSid)
}

// RefuseEveryPipeOwnerForTest makes DialLocal treat EVERY listener as
// foreign until restore is called, so a test in another package can stand a
// real listener in for a squatter's (tools/bridge/knomitapi). It can only
// NARROW the accepted set, never widen it: misused, it makes dials fail,
// and it cannot make one succeed. Not safe for parallel tests.
func RefuseEveryPipeOwnerForTest() (restore func()) {
	orig := acceptedPipeOwner
	acceptedPipeOwner = func(*windows.SID, string) bool { return false }
	return func() { acceptedPipeOwner = orig }
}

// verifyPipeOwner reads the OWNER of the pipe conn is connected to and
// refuses it unless acceptedPipeOwner does. Every failure to read it is an
// error too: a connection is never used unverified.
//
// WHY THE OWNER and not the server process's token user. The owner is read
// from the handle already held, in one call, and names whoever CREATED the
// pipe name — the squatter, if there is one. The token route
// (GetNamedPipeServerProcessId, then OpenProcess and OpenProcessToken) needs
// two more handles and goes through a PID, which can be reused between the
// lookup and the open. Both were measured to work from an unelevated client
// against an elevated server (knomit#265).
//
// SE_KERNEL_OBJECT and not SE_FILE_OBJECT: the file form fails with
// ERROR_INVALID_PARAMETER on a pipe whose DACL is not protected, which is
// exactly what a squatter is free to create (measured). READ_CONTROL, which
// the query needs, is part of the GENERIC_READ DialLocal asks for, so a pipe
// that withholds it cannot be dialled at all.
//
// THE LIMIT: a squatter running as THIS user owns its pipe as this user and
// passes. That is the unix same-uid boundary, and no owner check can move it.
func verifyPipeOwner(conn net.Conn, path string) error {
	fder, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return fmt.Errorf("local listener %s: the connection (%T) exposes no handle, so its owner cannot be verified", path, conn)
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(fder.Fd()), windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("local listener %s: reading its owner: %w", path, err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("local listener %s: reading its owner: %w", path, err)
	}
	if owner == nil {
		return fmt.Errorf("local listener %s: it has no owner, so it cannot be verified", path)
	}
	self, err := ownSID()
	if err != nil {
		return err
	}
	if !acceptedPipeOwner(owner, self) {
		// No escape-hatch advice here: what skips the pipe differs by
		// caller (kb's server argument or KNOMIT_SERVER for the bridge and hooks,
		// nothing at all for `knomit oauth`), so each caller adds its own.
		return fmt.Errorf("%w: %s is owned by %s, not %s; another process holds the local listener's name",
			ErrForeignListener, path, owner.String(), self)
	}
	return nil
}
