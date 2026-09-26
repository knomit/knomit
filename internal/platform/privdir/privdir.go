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

// Ensure creates path if it is missing and makes it private to the current
// user.
//
// It returns an error ONLY when the directory cannot be created. A directory
// that exists but cannot be made private (a filesystem without ACLs, a share,
// no right to change the DACL) or that the user deliberately left wider is
// logged as a warning and is not an error: refusing to start over a
// filesystem quirk would be worse than the status quo, and the warning is the
// part that was missing. What "private" means per OS, and what is left alone,
// is in the per-OS implementation.
func Ensure(path string) error { return ensure(path) }
