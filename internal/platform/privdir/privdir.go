// Package privdir makes a directory private to the current user, on every
// OS, and is how a data root gets that property at boot.
//
// "Private" means what 0700 means on unix: this user may do anything inside,
// and no other ordinary account may read, list or write it. Everything the
// application writes below such a directory inherits the property, which is
// the point: the tree writes its secrets (a private key, OAuth tokens, a
// config file that can hold an API key) through temp-file-then-rename, and a
// per-file permission does not survive that on Windows (the rename carries
// the TEMP file's DACL onto the target). A private directory covers every
// write site at once, including ones not yet written.
//
// Go's mode bits do not express this on Windows. There, os.Mkdir ignores the
// mode, and OpenFile and Chmod map it onto the read-only attribute alone, so a
// "0600" file simply inherits its parent's DACL. Under the user's profile that
// is owner-only by default; anywhere else (a data root under C:\, say) it can
// grant BUILTIN\Users read and Authenticated Users modify. Hence one file per
// OS: mode bits on unix, a protected DACL on Windows.
//
// Like the rest of internal/platform, it knows the OS and nothing about the
// application: the caller says which directory, and nothing here knows what
// lives in it.
package privdir

import (
	"errors"
	"path/filepath"
)

// Ensure creates path if it is missing and makes it private to the current
// user.
//
// It returns an error ONLY when the directory cannot be created. A directory
// that is ours and merely wider than private (unix mode bits from an earlier
// writer, a Windows DACL inherited from the parent) is tightened. One that
// cannot be made private (a filesystem without ACLs, a share, no right to
// change it) or that is not ours to change (another owner, a symlink, a DACL
// someone protected on purpose) is logged as a warning and is not an error:
// refusing to start over a filesystem quirk would be worse than the status
// quo, and the warning is the part that was missing. The per-OS
// implementation says exactly what is changed and what is left alone.
func Ensure(path string) error { return ensure(path) }

// ErrNotPrivate is Verify's refusal: the directory is not private to the
// current user.
var ErrNotPrivate = errors.New("directory is not private to this user")

// Verify reports whether path is, right now, a directory private to the
// current user, and changes nothing. It returns nil, or an error wrapping
// ErrNotPrivate that names the path, why, and where there is one the fix.
//
// Ensure is best-effort by design: it warns and carries on over a directory
// it cannot make private. Verify is for the caller that must not carry on,
// the one that is about to write or read a secret whose ONLY protection is
// the directory (on Windows a file's mode bits mean nothing). Call Ensure
// first, so that what Ensure can fix is fixed, then Verify.
//
// Refused on every OS: a symlink (or junction) in place of the directory,
// since whoever controls its parent can repoint it; something that is not a
// directory; an owner who is not this user. Unix: any group or other bit.
// Windows: an owner other than this user, SYSTEM or Administrators, a NULL
// DACL, or an allow ACE for any account Ensure's warning also counts as
// foreign (SYSTEM, Administrators, CREATOR OWNER and OWNER RIGHTS are not).
//
// path is cleaned first: Lstat of `link/` (or `link\` on Windows) resolves
// the link, so a trailing separator, which shell tab-completion appends,
// would otherwise get a symlinked directory past the symlink refusal.
func Verify(path string) error { return verify(filepath.Clean(path)) }
