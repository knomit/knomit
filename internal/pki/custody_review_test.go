package pki

import (
	"crypto/ed25519"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// keychainRoot makes a master dir whose root key is ONLY in kc (the default
// after init-master): there is no root.key.
func keychainRoot(t *testing.T) (string, *fakeKeychain, Root) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "master")
	kc := newFakeKeychain()
	root, src, err := CreateRoot(dir, "knomit-master-test", time.Hour, kc, nil)
	if err != nil || src != RootFromKeychain {
		t.Fatalf("CreateRoot: %v %q", err, src)
	}
	return dir, kc, root
}

// stillOpens asserts the root key is still held (keychain item present, and
// OpenRoot signs as the original key) and that no root.key was created.
func stillOpens(t *testing.T, dir string, kc *fakeKeychain, root Root) {
	t.Helper()
	if !kc.has(root) {
		t.Fatal("the keychain item is gone")
	}
	opened, src, err := OpenRoot(dir, kc, nil)
	if err != nil || src != RootFromKeychain {
		t.Fatalf("the root no longer opens: %v %q", err, src)
	}
	signsAs(t, opened, root.Cert.PublicKey.(ed25519.PublicKey))
}

// Review finding 1: --out <dir>/root.key used to write the export there and
// then delete it as "the local root.key", together with the keychain item.
func TestExportRoot_RefusesToExportOntoTheMastersRootKey(t *testing.T) {
	dir, kc, root := keychainRoot(t)
	keyPath := filepath.Join(dir, RootKeyFile)

	link := filepath.Join(t.TempDir(), "link-to-master")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string]string{
		"same path":         keyPath,
		"unclean spelling":  filepath.Join(dir, ".", "sub", "..", RootKeyFile),
		"symlinked dir":     filepath.Join(link, RootKeyFile),
		"relative from dir": "", // filled below, after Chdir
	} {
		if name == "relative from dir" {
			t.Chdir(dir)
			out = RootKeyFile
		}
		res, err := ExportRoot(dir, out, kc, custodyPass, false)
		if !errors.Is(err, ErrExportOntoRootKey) {
			t.Fatalf("%s: err %v (result %+v), want ErrExportOntoRootKey", name, err, res)
		}
		if res.RemovedKeychain || res.RemovedFile {
			t.Fatalf("%s: removed something: %+v", name, res)
		}
		if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: the refused export was written as root.key: %v", name, err)
		}
		stillOpens(t, dir, kc, root)
	}
}

// The post-write os.SameFile check: on a case-insensitive filesystem (the
// macOS default) <dir>/ROOT.KEY is <dir>/root.key though no path spelling
// says so. The file written is left in place and nothing is removed.
func TestExportRoot_SameFileCheckCatchesAnAliasedSpelling(t *testing.T) {
	dir, kc, root := keychainRoot(t)
	probe := filepath.Join(dir, "case-probe")
	os.WriteFile(probe, nil, 0o600)
	if _, err := os.Stat(filepath.Join(dir, "CASE-PROBE")); err != nil {
		t.Skip("case-sensitive filesystem: no aliased spelling to test")
	}
	res, err := ExportRoot(dir, filepath.Join(dir, "ROOT.KEY"), kc, custodyPass, false)
	if !errors.Is(err, ErrExportOntoRootKey) || !strings.Contains(err.Error(), "nothing was removed") {
		t.Fatalf("aliased spelling: err %v, want ErrExportOntoRootKey from the post-write check", err)
	}
	if res.RemovedKeychain || res.RemovedFile {
		t.Fatalf("removed something: %+v", res)
	}
	stillOpens(t, dir, kc, root)
	// What was written is a valid root.key for the same root.
	if loaded, err := LoadRoot(dir, custodyPass); err != nil {
		t.Fatalf("the file left behind is not a usable root.key: %v", err)
	} else {
		signsAs(t, loaded, root.Cert.PublicKey.(ed25519.PublicKey))
	}
}

// Review finding 5: the key read from the file because the keychain was
// unreadable, and the keychain then refusing the delete, must be reported
// (the item may still be there), not counted as a clean move.
func TestExportRoot_KeychainDeleteFailureIsReported(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewRoot(dir, "cn", custodyPass, time.Hour); err != nil {
		t.Fatal(err)
	}
	kc := newFakeKeychain()
	kc.getErr = errors.New("keychain locked")
	kc.delErr = errors.New("keychain locked")
	res, err := ExportRoot(dir, filepath.Join(t.TempDir(), "export"), kc, custodyPass, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.From != RootFromFile || !res.RemovedFile || res.RemovedKeychain {
		t.Fatalf("result %+v; want from file, root.key removed, keychain not", res)
	}
	if res.KeychainErr == nil || !strings.Contains(res.KeychainErr.Error(), "locked") {
		t.Fatalf("KeychainErr = %v; want the delete failure", res.KeychainErr)
	}
	// A not-found delete is a clean move: no KeychainErr.
	dir2 := t.TempDir()
	NewRoot(dir2, "cn", custodyPass, time.Hour)
	res2, err := ExportRoot(dir2, filepath.Join(t.TempDir(), "export"), newFakeKeychain(), custodyPass, false)
	if err != nil || res2.KeychainErr != nil || !res2.RemovedFile {
		t.Fatalf("clean file move: %v %+v", err, res2)
	}
}

// Review finding 9: a failed import must not leave its root.crt beside an
// older root.key of another root.
func TestImportRoot_FailureLeavesNoNewRootCert(t *testing.T) {
	src := t.TempDir()
	if _, err := NewRoot(src, "a", custodyPass, time.Hour); err != nil {
		t.Fatal(err)
	}
	export := filepath.Join(t.TempDir(), "a.export")
	if _, err := ExportRoot(src, export, newFakeKeychain(), custodyPass, true); err != nil {
		t.Fatal(err)
	}
	// dst: an orphan root.key of ANOTHER root, no root.crt, a keychain that
	// refuses — the file fallback's O_EXCL write fails.
	other := t.TempDir()
	if _, err := NewRoot(other, "b", custodyPass, time.Hour); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	orphan, _ := os.ReadFile(filepath.Join(other, RootKeyFile))
	os.WriteFile(filepath.Join(dst, RootKeyFile), orphan, 0o600)
	kc := newFakeKeychain()
	kc.setErr = errors.New("headless")
	if _, _, err := ImportRoot(dst, export, kc, custodyPass); err == nil || !strings.Contains(err.Error(), "file fallback failed") {
		t.Fatalf("import over an orphan root.key: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, RootCertFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a failed import left root.crt behind: %v", err)
	}
	if now, _ := os.ReadFile(filepath.Join(dst, RootKeyFile)); string(now) != string(orphan) {
		t.Fatal("the orphan root.key was changed")
	}
}

// Review finding 8: remaining*3 overflowed time.Duration past ~97 years,
// so a long-lived certificate was "due" from its first day.
func TestRenewalWindow_LongLifetimeDoesNotOverflow(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	life := 200 * 365 * 24 * time.Hour // fits in a Duration; three times it does not
	cert := &x509.Certificate{NotBefore: start, NotAfter: start.Add(life)}
	if NeedsRenewal(cert, start.Add(time.Hour)) {
		t.Fatal("a 200-year certificate is due for renewal on its first day")
	}
	if !NeedsRenewal(cert, start.Add(life-life/4)) {
		t.Fatal("a 200-year certificate with a quarter left is not due")
	}
}
