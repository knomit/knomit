// Package fileuri turns a local filesystem path into a file: URI that both
// SQLite and go-git accept, on every OS this project builds for.
//
// It exists because the obvious construction is wrong on Windows and silently
// right everywhere else. Two spellings were in the tree before this package:
//
//	"file:" + (&url.URL{Path: p}).String()   // cmd/migrate_registry.go
//	"file://" + p                            // git remote fixtures
//
// Given p = `C:\Users\pba\core 1.db` the first yields
// "file:./C:%5CUsers%5Cpba%5Ccore%201.db" — url.URL.String prepends "./" when
// the first segment contains a colon (so the result cannot be misread as a
// scheme) and percent-escapes the separators, because a backslash is not a
// path character in a URL. SQLite decodes the escapes back to backslashes and
// is left with a RELATIVE path, hence "unable to open database file: The
// filename, directory name, or volume label syntax is incorrect."
//
// The second yields `file://C:\Users\...` — there "C:" lands in the authority
// component, so url.Parse rejects it with "invalid port".
//
// Both are correct on Unix, where paths already start with "/" and contain no
// backslashes or colons, which is why neither was noticed until the tree was
// built on Windows.
package fileuri

import (
	"net/url"
	"path/filepath"
	"strings"
)

// New returns path as a file: URI:
//
//	/home/pba/core 1.db        ->  file:///home/pba/core%201.db
//	C:\Users\pba\core 1.db     ->  file:///C:/Users/pba/core%201.db
//	\\srv\share\core.db        ->  file://srv/share/core.db
//
// The leading slash before the drive letter is the form SQLite documents for
// Windows and the one url.Parse round-trips: with it "C:" is a path segment;
// without it "C:" is an authority with a bad port.
//
// A UNC path becomes the RFC 8089 authority form, which is the only spelling
// that names the host rather than losing it. Note that SQLite accepts a file:
// URI only when the authority is empty or "localhost" — a knomit home on a
// UNC share therefore has to be opened by plain path, not through this
// function. Every SQLite caller in this tree opens a path under the user's
// home directory, which is drive-letter-local on Windows.
//
// A RELATIVE path stays relative: prefixing "/" would quietly turn it into an
// absolute one, naming a different file. Callers needing an absolute URI run
// filepath.Abs first, which every caller in this tree does, because SQLite and
// go-git both resolve a relative file: URI against the process working
// directory rather than anything the caller controls.
//
// One relative shape has no faithful URI at all: the Windows DRIVE-RELATIVE
// path `C:x.db`, meaning "x.db in the current directory OF DRIVE C", which is
// not necessarily the process working directory. A file: URI cannot express
// per-drive current directories, so New returns "./C:x.db" — a relative URI
// that resolves against the process cwd and therefore names a different file
// than the input did. It is escaped, not absolute, and not silently promoted;
// that is as close as the format gets. filepath.Abs resolves the drive-
// relative form correctly, so a caller that runs it first — as every caller
// here does — never meets this case.
func New(path string) string {
	p := filepath.ToSlash(path)

	if host, rest, ok := uncParts(p); ok {
		return (&url.URL{Scheme: "file", Host: host, Path: rest}).String()
	}
	if !isAbs(p) {
		return (&url.URL{Path: p}).String()
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // Windows drive-letter path, already slash-separated.
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

// Path is the inverse of New: it turns a file: URI back into a local
// filesystem path in the host's own spelling, and reports false for anything
// that is not a file: URI.
//
// The drive-letter case is the one that matters. url.Parse leaves
// "file:///C:/Users/pba" with Path "/C:/Users/pba", and that leading slash is
// not cosmetic — os.Stat("/C:/Users/pba") fails on Windows. Anything that
// takes a path back out of a file: URI has to strip it, which go-git does
// internally (transport/file.adjustPathForWindows) but knomit's own origin
// gate did not, so a file: origin was compared against local_origin_root with
// a leading slash it could never match.
func Path(uri string) (string, bool) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return "", false
	}
	p := u.Path
	if u.Host != "" {
		p = "//" + u.Host + p // UNC: put the host back on the front.
	}
	return filepath.FromSlash(TrimDriveSlash(p)), true
}

// TrimDriveSlash removes the slash a file: URI carries in front of a Windows
// drive letter ("/C:/x" -> "C:/x"), and leaves every other path alone.
//
// It is exported because a caller that has already split a file: URI — one
// holding a parsed endpoint's path rather than the URI — still needs this one
// rule, and a second copy of it would be a second thing to keep in step with
// go-git's transport/file.adjustPathForWindows, which applies exactly this
// before handing the path to git.
//
// Unconditional rather than GOOS-gated: "/C:/x" is not a path that means
// anything on Unix either, and a URI naming a Windows path may well be read on
// a machine that is not the one that wrote it.
func TrimDriveSlash(p string) string {
	if len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		c := p[1]
		if ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') {
			return p[1:]
		}
	}
	return p
}

// uncParts splits an already-slash-separated UNC path //host/share/rest into
// its host and its /share/rest remainder. A bare "//" or "//host" with no
// share is not a UNC path anyone can open, so it is not treated as one.
func uncParts(p string) (host, rest string, ok bool) {
	if !strings.HasPrefix(p, "//") {
		return "", "", false
	}
	host, rest, found := strings.Cut(strings.TrimPrefix(p, "//"), "/")
	if !found || host == "" || rest == "" {
		return "", "", false
	}
	return host, "/" + rest, true
}

// isAbs reports whether the already-slash-separated p is absolute on ANY
// platform, rather than on the one we happen to be compiled for.
// filepath.IsAbs answers for the host only — it calls `C:/x` relative on Linux
// and `/x` relative on Windows — so a URI built on one OS for a path named by
// another (a config value, a test fixture, a database on a synced volume)
// would come out with the wrong shape.
func isAbs(p string) bool {
	if strings.HasPrefix(p, "/") {
		return true
	}
	// Windows drive-letter: one ASCII letter, a colon, then a separator.
	if len(p) >= 3 && p[1] == ':' && p[2] == '/' {
		c := p[0]
		return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
	}
	return false
}
