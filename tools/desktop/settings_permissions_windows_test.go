//go:build desktop && windows

package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"

	"knomit/internal/platform/privdir"
)

// TestApplySettingsFilePermissions_Windows is the Windows twin of
// TestApplySettingsFilePermissions (knomit#301). There, "private" is a mode
// bit on the file; here it is the data root's DACL, which a knomit.toml
// written through the temp file + rename inherits. So the assertion is on
// the file's ACEs: nobody but the current user and SYSTEM.
//
// The parent is given a deliberately BROAD protected DACL (BUILTIN\Users
// read+execute, inheritable). t.TempDir() alone is under the profile, whose
// inherited DACL is already owner-only, and the test would pass without
// privdir doing anything.
func TestApplySettingsFilePermissions_Windows(t *testing.T) {
	parent := t.TempDir()
	me := currentUserSID(t)
	setProtectedDACL(t, parent, "D:P(A;OICI;FA;;;"+me+")(A;OICI;FA;;;SY)(A;OICI;0x1200a9;;;BU)")

	home := filepath.Join(parent, "home")
	if err := privdir.Ensure(home); err != nil {
		t.Fatal(err)
	}
	s := Settings{Port: "19278", LogLevel: "info", LogFormat: "console"}

	fresh := filepath.Join(home, "fresh.toml")
	if err := applySettings(s, fresh, &stubToggler{}, nil); err != nil {
		t.Fatal(err)
	}
	assertOnlyOwnerAndSystem(t, fresh, me)

	// The Windows meaning of "an existing file keeps its mode": rewriting it
	// through the temp file does not widen it.
	existing := filepath.Join(home, "existing.toml")
	if err := os.WriteFile(existing, []byte("port = \"19278\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := applySettings(s, existing, &stubToggler{}, nil); err != nil {
		t.Fatal(err)
	}
	assertOnlyOwnerAndSystem(t, existing, me)
}

func assertOnlyOwnerAndSystem(t *testing.T, path, me string) {
	t.Helper()
	in, err := privdir.Inspect(path)
	if err != nil {
		t.Fatal(err)
	}
	if in.NullDACL || len(in.ACEs) == 0 {
		t.Fatalf("%s has no DACL entries (%+v): a NULL DACL grants everyone everything", path, in)
	}
	for _, a := range in.ACEs {
		if a.SID != me && a.SID != "S-1-5-18" {
			t.Errorf("%s grants %s (mask %#x): only %s and SYSTEM may appear", path, a.SID, a.Mask, me)
		}
	}
}

func currentUserSID(t *testing.T) string {
	t.Helper()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return u.User.Sid.String()
}

func setProtectedDACL(t *testing.T, path, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}
