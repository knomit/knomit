package fileuri_test

import (
	"net/url"
	"runtime"
	"testing"

	"knomit/internal/platform/fileuri"
)

// TestNew pins the exact strings, on every OS. The cases are deliberately not
// platform-gated: the bug this package fixes was a construction that produced
// a valid-looking URI for Unix paths and a broken one for Windows paths, so a
// test that only ever saw its own platform's paths would have passed
// throughout. filepath.ToSlash is a no-op on Unix, so the Windows cases here
// exercise the same code either way except for the separator rewrite.
func TestNew(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{"unix absolute", "/home/pba/core.db", "file:///home/pba/core.db"},
		{"unix absolute with space", "/home/pba/core 1.db", "file:///home/pba/core%201.db"},
		{"windows drive forward slashes", "C:/Users/pba/core.db", "file:///C:/Users/pba/core.db"},
		{"windows drive lowercase", "d:/tmp/x.db", "file:///d:/tmp/x.db"},
		{"unc forward slashes", "//srv/share/core.db", "file://srv/share/core.db"},
		// No share component, so not a UNC path anyone can open: it keeps the
		// empty authority instead of acquiring "srv" as a host.
		{"unc host only", "//srv", "file:////srv"},
		{"percent in name", "/tmp/100%.db", "file:///tmp/100%25.db"},
		{"hash in name", "/tmp/a#b.db", "file:///tmp/a%23b.db"},
		{"question mark in name", "/tmp/a?b.db", "file:///tmp/a%3Fb.db"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := fileuri.New(tc.path); got != tc.want {
				t.Errorf("New(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestNewConvertsBackslashes is the Windows-separator half, kept separate
// because the backslash spellings are only meaningful where ToSlash rewrites
// them. On Unix a backslash is an ordinary filename character, so these run on
// Windows only.
func TestNewConvertsBackslashes(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("backslash is a literal filename character off Windows, so ToSlash leaves it alone")
	}
	for _, tc := range []struct{ path, want string }{
		{"C:\\Users\\pba\\core.db", "file:///C:/Users/pba/core.db"},
		{"C:\\Users\\pba\\core 1.db", "file:///C:/Users/pba/core%201.db"},
		{"\\\\srv\\share\\core.db", "file://srv/share/core.db"},
	} {
		if got := fileuri.New(tc.path); got != tc.want {
			t.Errorf("New(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestNewIsParseable is the property the "invalid port" failure violated:
// whatever New returns must survive url.Parse as scheme file. `file://C:\...`
// — the old git-remote spelling — fails here, which is the whole point.
func TestNewIsParseable(t *testing.T) {
	for _, path := range []string{
		"/home/pba/core.db",
		"C:/Users/pba/core 1.db",
		"//srv/share/core.db",
		"/tmp/a b#c?d%e.db",
	} {
		u, err := url.Parse(fileuri.New(path))
		if err != nil {
			t.Fatalf("url.Parse(New(%q)) = %v", path, err)
		}
		if u.Scheme != "file" {
			t.Errorf("New(%q): scheme = %q, want file", path, u.Scheme)
		}
	}
}

// TestNewKeepsTheDriveLetterOutOfTheAuthority is the specific regression: a
// drive letter parsed as a host makes ":" a port separator and "\Users..." a
// bad port.
func TestNewKeepsTheDriveLetterOutOfTheAuthority(t *testing.T) {
	u, err := url.Parse(fileuri.New("C:/Users/pba/core.db"))
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	if u.Host != "" {
		t.Errorf("host = %q, want empty — a drive letter in the authority is the `invalid port` bug", u.Host)
	}
	if u.Path != "/C:/Users/pba/core.db" {
		t.Errorf("path = %q, want /C:/Users/pba/core.db", u.Path)
	}
}

// TestNewRoundTripsThePath checks the escaping is reversible, so a home
// directory with a space or a percent in it names the same file afterwards.
func TestNewRoundTripsThePath(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"/home/pba/core 1.db", "/home/pba/core 1.db"},
		{"/tmp/100%.db", "/tmp/100%.db"},
		{"C:/Users/pba/core 1.db", "/C:/Users/pba/core 1.db"},
	} {
		u, err := url.Parse(fileuri.New(tc.path))
		if err != nil {
			t.Fatalf("url.Parse(New(%q)) = %v", tc.path, err)
		}
		if u.Path != tc.want {
			t.Errorf("New(%q) parsed back to path %q, want %q", tc.path, u.Path, tc.want)
		}
	}
}

// TestNewNamesTheUNCHost pins that a UNC path keeps its host instead of
// folding it into the path, where it would name a directory on the local disk.
func TestNewNamesTheUNCHost(t *testing.T) {
	u, err := url.Parse(fileuri.New("//srv/share/core.db"))
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	if u.Host != "srv" {
		t.Errorf("host = %q, want srv", u.Host)
	}
	if u.Path != "/share/core.db" {
		t.Errorf("path = %q, want /share/core.db", u.Path)
	}
}

// TestNewLeavesRelativePathsRelative documents the deliberate non-promotion: a
// relative path must not acquire a leading slash and start naming a different
// file. Callers wanting an absolute URI call filepath.Abs themselves.
func TestNewLeavesRelativePathsRelative(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"core.db", "core.db"},
		{"sub/core.db", "sub/core.db"},
		// Drive-relative: a colon with no separator after it. `C:x.db` means
		// "x.db in the current directory of drive C", which a file: URI cannot
		// express at all. The "./" is url.URL.String refusing to emit a first
		// segment containing a colon, which would re-parse as a scheme — so
		// the result stays a relative URI instead of becoming "file:C:x.db",
		// whose scheme would be "c". See New's doc for why no caller meets it.
		{"C:x.db", "./C:x.db"},
		{"C:core.db", "./C:core.db"},
	} {
		if got := fileuri.New(tc.path); got != tc.want {
			t.Errorf("New(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestPathRoundTripsNew is the property both directions have to agree on: a
// path turned into a URI and back must name the same file.
func TestPathRoundTripsNew(t *testing.T) {
	paths := []string{
		"/home/pba/core.db",
		"/home/pba/core 1.db",
		"/tmp/100%.db",
	}
	if runtime.GOOS == "windows" {
		paths = []string{
			`C:\Users\pba\core.db`,
			`C:\Users\pba\core 1.db`,
			`C:\tmp\100%.db`,
			`\\srv\share\core.db`,
		}
	}
	for _, path := range paths {
		got, ok := fileuri.Path(fileuri.New(path))
		if !ok {
			t.Fatalf("Path(New(%q)) reported not-a-file-URI", path)
		}
		if got != path {
			t.Errorf("Path(New(%q)) = %q, want %q", path, got, path)
		}
	}
}

// TestPathStripsTheDriveSlash pins the specific defect: url.Parse leaves a
// slash in front of the drive letter and os.Stat cannot use it.
func TestPathStripsTheDriveSlash(t *testing.T) {
	got, ok := fileuri.Path("file:///C:/Users/pba/core.db")
	if !ok {
		t.Fatal("reported not-a-file-URI")
	}
	want := "C:/Users/pba/core.db"
	if runtime.GOOS == "windows" {
		want = `C:\Users\pba\core.db`
	}
	if got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}

// TestPathRejectsNonFileURIs keeps the origin gate from treating a network
// remote as a local path.
func TestPathRejectsNonFileURIs(t *testing.T) {
	for _, uri := range []string{
		"https://example.com/repo.git",
		"ssh://git@example.com/repo.git",
		"git@example.com:owner/repo.git",
		"/plain/path",
		"",
	} {
		if got, ok := fileuri.Path(uri); ok {
			t.Errorf("Path(%q) = %q, true; want false", uri, got)
		}
	}
}
