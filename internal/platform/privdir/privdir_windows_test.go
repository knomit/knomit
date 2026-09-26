//go:build windows

package privdir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

const (
	usersSID  = "S-1-5-32-545" // BUILTIN\Users
	fileAll   = 0x1f01ff       // FILE_ALL_ACCESS, what "FA" (and "GA") read back as
	inheritOI = windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE
)

// broadParent returns a directory whose DACL is PROTECTED and grants
// BUILTIN\Users read+execute, inheritable, besides the owner and SYSTEM.
//
// t.TempDir() alone would make every test here vacuous: it is under %TEMP%,
// inside the profile, whose inherited DACL is already owner-only, so a
// directory Ensure never touched would pass the same assertions. This is the
// C:\-like parent that exposed the real bug (knomit#301).
func broadParent(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	setDACL(t, dir, "D:P(A;OICI;FA;;;"+me(t)+")(A;OICI;FA;;;SY)(A;OICI;0x1200a9;;;BU)")
	in := inspect(t, dir)
	if !hasSID(in, usersSID) {
		t.Fatalf("fixture: parent does not grant BUILTIN\\Users anything: %+v", in)
	}
	return dir
}

func setDACL(t *testing.T, path, sddl string) {
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

func me(t *testing.T) string {
	t.Helper()
	sid, err := ownSID()
	if err != nil {
		t.Fatal(err)
	}
	return sid
}

func inspect(t *testing.T, path string) Inspection {
	t.Helper()
	in, err := Inspect(path)
	if err != nil {
		t.Fatalf("Inspect(%s): %v", path, err)
	}
	return in
}

func sddlOf(t *testing.T, path string) string {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	return sd.String()
}

func hasSID(in Inspection, sid string) bool {
	for _, a := range in.ACEs {
		if a.SID == sid {
			return true
		}
	}
	return false
}

// assertPrivateDir: protected, exactly owner FA and SYSTEM FA, both
// inheritable to files and directories, neither inherited from the parent.
func assertPrivateDir(t *testing.T, path string) {
	t.Helper()
	in := inspect(t, path)
	if !in.Protected || in.NullDACL || len(in.ACEs) != 2 {
		t.Fatalf("%s is not a protected two-ACE DACL: %+v", path, in)
	}
	want := map[string]bool{me(t): true, sidSystem: true}
	for _, a := range in.ACEs {
		if !want[a.SID] || !a.Allow || a.Mask != fileAll || a.Inherited || a.Flags != inheritOI {
			t.Errorf("%s: unexpected ACE %+v (want allow FA with exactly OI|CI, not inherited, for %s or SYSTEM)", path, a, me(t))
		}
		delete(want, a.SID)
	}
}

// assertInheritsPrivate: a file under a private directory carries only the
// two inherited ACEs, and nothing for BUILTIN\Users.
func assertInheritsPrivate(t *testing.T, path string) {
	t.Helper()
	in := inspect(t, path)
	if in.NullDACL || len(in.ACEs) != 2 || hasSID(in, usersSID) {
		t.Fatalf("%s does not carry only the private directory's two ACEs: %+v", path, in)
	}
	want := map[string]bool{me(t): true, sidSystem: true}
	for _, a := range in.ACEs {
		if !want[a.SID] || !a.Allow || a.Mask != fileAll || !a.Inherited {
			t.Errorf("%s: unexpected or repeated ACE %+v (want one inherited allow FA each for %s and SYSTEM)", path, a, me(t))
		}
		delete(want, a.SID)
	}
}

// (a) A NEW directory under a broad parent is made private, and a file then
// written into it with Go's ordinary os.WriteFile inherits that.
func TestEnsure_NewDirIsPrivateAndFilesInherit(t *testing.T) {
	home := filepath.Join(broadParent(t), "home")

	if err := Ensure(home); err != nil {
		t.Fatal(err)
	}
	assertPrivateDir(t, home)

	f := filepath.Join(home, "secret")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertInheritsPrivate(t, f)
}

// (b) An EXISTING directory that inherited the broad DACL, with a file
// already inside it: Ensure makes the directory private, and the change
// propagates to the file that was there first.
func TestEnsure_ExistingDirIsFixedAndChildrenFollow(t *testing.T) {
	home := filepath.Join(broadParent(t), "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(home, "old")
	if err := os.WriteFile(old, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !hasSID(inspect(t, old), usersSID) {
		t.Fatalf("fixture: the pre-existing file does not inherit the Users ACE: %+v", inspect(t, old))
	}

	if err := Ensure(home); err != nil {
		t.Fatal(err)
	}
	assertPrivateDir(t, home)
	assertInheritsPrivate(t, old)
}

// (c) Idempotent: a second Ensure (the next boot) changes nothing.
func TestEnsure_Idempotent(t *testing.T) {
	home := filepath.Join(broadParent(t), "home")
	if err := Ensure(home); err != nil {
		t.Fatal(err)
	}
	before := sddlOf(t, home)
	if err := Ensure(home); err != nil {
		t.Fatal(err)
	}
	if after := sddlOf(t, home); after != before {
		t.Errorf("second Ensure changed the DACL:\nbefore %s\nafter  %s", before, after)
	}
}

// (d) A DACL that is already protected was chosen by someone, and is left
// alone even though it grants Users read: a boot must not overwrite it.
func TestEnsure_LeavesAProtectedCustomDACLAlone(t *testing.T) {
	home := filepath.Join(broadParent(t), "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	setDACL(t, home, "D:P(A;OICI;FA;;;"+me(t)+")(A;OICI;FA;;;SY)(A;OICI;0x1200a9;;;BU)")
	before := sddlOf(t, home)
	logs := captureLog(t)

	if err := Ensure(home); err != nil {
		t.Fatal(err)
	}
	if after := sddlOf(t, home); after != before {
		t.Errorf("Ensure rewrote a deliberately protected DACL:\nbefore %s\nafter  %s", before, after)
	}
	if !hasSID(inspect(t, home), usersSID) {
		t.Error("the custom Users ACE is gone")
	}
	assertForeignGrantWarned(t, logs.String(), usersSID)
}

// (e) A directory pre-created protected with access for ANOTHER account only
// (the shape a data root made by someone else can have): left alone, and
// reported, naming who it grants.
func TestEnsure_WarnsOnAProtectedDACLForAnotherAccountOnly(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	setDACL(t, home, "D:P(A;OICI;0x1200a9;;;BU)")
	// Registered after t.TempDir, so it runs before its RemoveAll: give the
	// owner (us, who keep WRITE_DAC) access back, or cleanup cannot delete it.
	t.Cleanup(func() { setDACL(t, home, "D:P(A;OICI;FA;;;"+me(t)+")") })
	before := sddlOf(t, home)
	logs := captureLog(t)

	if err := Ensure(home); err != nil {
		t.Fatal(err)
	}
	if after := sddlOf(t, home); after != before {
		t.Errorf("Ensure rewrote a protected DACL:\nbefore %s\nafter  %s", before, after)
	}
	assertForeignGrantWarned(t, logs.String(), usersSID)
}

func assertForeignGrantWarned(t *testing.T, out, sid string) {
	t.Helper()
	if !strings.Contains(out, `"level":"warn"`) ||
		!strings.Contains(out, "data root DACL is protected and grants "+sid+"; leaving it as set, but this is not private to you") {
		t.Errorf("no warning that the protected DACL grants %s: %q", sid, out)
	}
}

// Not built here, because a test cannot create them: a root OWNED by another
// account (needs a second account, or SeRestorePrivilege), and a process
// running as a service SID. ensure's owner check and isServiceSID are the
// code paths; the SIDs they compare against are the constants in
// privdir_windows.go.
