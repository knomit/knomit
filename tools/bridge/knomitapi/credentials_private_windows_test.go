//go:build windows

package knomitapi

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"

	"knomit/internal/platform/privdir"
)

// `kb login` can be the first thing ever run on a machine, so SaveCredentials
// makes the data root private itself (ensureCredentialsDir), and the token
// file inherits it. The parent grants BUILTIN\Users read, inheritably, so a
// root nobody made private would carry that ACE: t.TempDir() alone is under
// the profile and already owner-only.
func TestSaveCredentials_MakesTheDataRootPrivate(t *testing.T) {
	parent := t.TempDir()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	me := u.User.Sid.String()
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + me + ")(A;OICI;FA;;;SY)(A;OICI;0x1200a9;;;BU)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(parent, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(parent, "home")
	isolateHomeEnv(t, home)

	base, _ := url.Parse("https://example.com")
	if err := SaveCredentials(base, &Credentials{AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatal(err)
	}

	in, err := privdir.Inspect(home)
	if err != nil {
		t.Fatal(err)
	}
	if !in.Protected || len(in.ACEs) != 2 {
		t.Fatalf("data root after SaveCredentials is not a protected two-ACE DACL: %+v", in)
	}
	p, err := CredentialsPath(base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
	fin, err := privdir.Inspect(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range fin.ACEs {
		if a.SID != me && a.SID != "S-1-5-18" {
			t.Errorf("token file grants %s (mask %#x)", a.SID, a.Mask)
		}
	}
}
