package pki_test

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"knomit/internal/pki"
	"knomit/internal/pki/pkitest"
)

// bundleFor is what `knomit identity enroll` hands the instance: the
// instance certificate, the root certificate and the fleet's current CRL.
func bundleFor(t *testing.T, f *pkitest.Fleet, m pkitest.Member) []byte {
	t.Helper()
	rootPEM, err := os.ReadFile(filepath.Join(f.Dir, pki.RootCertFile))
	if err != nil {
		t.Fatal(err)
	}
	crlPEM, err := os.ReadFile(filepath.Join(f.Dir, pki.CRLFile))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Join([][]byte{m.CertPEM, rootPEM, crlPEM}, nil)
}

// bumpCRL reissues the fleet's CRL at number n, so bundles enrolled after it
// carry n.
func bumpCRL(t *testing.T, f *pkitest.Fleet, n int64) {
	t.Helper()
	if _, err := pki.IssueCRL(f.Dir, f.Root, nil, big.NewInt(n), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
}

// snapshot is every file under dir with its mode, mtime and bytes, so "nothing changed" can
// be asserted as equality rather than as the absence of one expected file.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		rel, _ := filepath.Rel(dir, p)
		out[rel] = fmt.Sprintf("%v %d %s", fi.Mode(), fi.ModTime().UnixNano(), b)
		return nil
	})
	return out
}

func sameTree(t *testing.T, before, after map[string]string) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("pki dir changed: %d files before, %d after (%v)", len(before), len(after), keys(after))
	}
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("pki dir changed: %s differs", k)
		}
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestInstallBundle_WritesTheThreeFilesAndTheWatermark(t *testing.T) {
	f := pkitest.New(t)
	m := f.Enroll(t, "laptop", pki.RoleInstance)
	dir := filepath.Join(t.TempDir(), "pki")

	id, err := pki.InstallBundle(dir, m.KeyPath, bundleFor(t, f, m), false)
	if err != nil {
		t.Fatal(err)
	}
	if id.Fingerprint != m.Fingerprint() || id.Host != "laptop" || id.Role != pki.RoleInstance {
		t.Fatalf("identity %+v, want fingerprint %s host laptop", id, m.Fingerprint())
	}
	posix := runtime.GOOS != "windows"
	if fi, err := os.Stat(dir); err != nil || (posix && fi.Mode().Perm() != 0o700) {
		t.Fatalf("pki dir: %v %v", fi, err)
	}
	for _, name := range []string{pki.InstanceCertFile, pki.RootCertFile, pki.CRLFile} {
		if fi, err := os.Stat(filepath.Join(dir, name)); err != nil || (posix && fi.Mode().Perm() != 0o600) {
			t.Fatalf("%s: %v %v", name, fi, err)
		}
	}
	n, err := pki.AcceptedNumber(dir, f.Root.Cert)
	if err != nil || n == nil || n.Int64() != 1 {
		t.Fatalf("watermark %v %v, want 1", n, err)
	}
	if _, err := pki.ServerConfig(dir, m.KeyPath, nil); err != nil {
		t.Fatalf("installed files do not load as a server config: %v", err)
	}
	// No temp file is left behind by the atomic writes.
	for name := range snapshot(t, dir) {
		if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".tmp") {
			t.Fatalf("left a temp file: %s", name)
		}
	}
}

// Every refusal leaves <pki> exactly as it was: each check runs before any
// write. The class of each refusal is a sentinel a UI can map to text
// without showing the bundle, and the CLI's wording is kept.
func TestInstallBundle_RefusalsAreClassifiedAndWriteNothing(t *testing.T) {
	f := pkitest.New(t)
	m := f.Enroll(t, "laptop", pki.RoleInstance)
	dir := filepath.Join(t.TempDir(), "pki")
	if _, err := pki.InstallBundle(dir, m.KeyPath, bundleFor(t, f, m), false); err != nil {
		t.Fatal(err)
	}
	bumpCRL(t, f, 2)
	current := bundleFor(t, f, f.Enroll(t, "laptop", pki.RoleInstance, m.KeyPath))
	if _, err := pki.InstallBundle(dir, m.KeyPath, current, false); err != nil {
		t.Fatal(err) // positive control: CRL #2 moves the watermark to 2
	}

	f1 := pkitest.New(t)
	other := f.Enroll(t, "other", pki.RoleInstance) // another instance's key, same fleet
	foreign := f1.Enroll(t, "laptop", pki.RoleInstance, m.KeyPath)

	// A bundle carrying CRL #1 from THIS fleet, while #2 is held: rollback.
	oldCRL := func() []byte {
		d := t.TempDir()
		root := f.Root
		if _, err := pki.IssueCRL(d, root, nil, big.NewInt(1), time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		crl, _ := os.ReadFile(filepath.Join(d, pki.CRLFile))
		rootPEM, _ := os.ReadFile(filepath.Join(f.Dir, pki.RootCertFile))
		return bytes.Join([][]byte{m.CertPEM, rootPEM, crl}, nil)
	}()

	for _, tc := range []struct {
		name   string
		raw    []byte
		class  error
		phrase string // the wording the CLI prints, kept
	}{
		{"another key", bundleFor(t, f, other), pki.ErrKeyMismatch, "not this instance's"},
		{"older CRL", oldCRL, pki.ErrCRLRollback, "older"},
		{"different root", bundleFor(t, f1, foreign), pki.ErrRootDiffers, "different fleet root"},
		{"private key block", []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"), pki.ErrMalformedBundle, "unexpected"},
		{"not PEM", []byte("hello"), pki.ErrMalformedBundle, "must hold"},
		{"two roots", append(bundleFor(t, f, m), bundleFor(t, f1, foreign)[len(foreign.CertPEM):]...), pki.ErrMalformedBundle, "two root"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// An enrolled home: every file, and the dir's own mode, as it was.
			os.Chmod(dir, 0o750) // a mode the install would reset, so a stray Chmod shows
			before := snapshot(t, dir)
			beforeDir, _ := os.Stat(dir)
			_, err := pki.InstallBundle(dir, m.KeyPath, tc.raw, false)
			if !errors.Is(err, tc.class) {
				t.Fatalf("err %v, want class %v", err, tc.class)
			}
			if !strings.Contains(err.Error(), tc.phrase) {
				t.Fatalf("err %q lost the CLI wording %q", err, tc.phrase)
			}
			sameTree(t, before, snapshot(t, dir))
			if afterDir, _ := os.Stat(dir); afterDir.Mode() != beforeDir.Mode() {
				t.Fatalf("pki dir mode %v -> %v on a refusal", beforeDir.Mode(), afterDir.Mode())
			}
			os.Chmod(dir, 0o700)

			// A fresh home: a refusal creates no pki dir at all. (Root-differs
			// needs an installed root, so it has no fresh-home case.)
			if tc.class == pki.ErrRootDiffers || tc.class == pki.ErrCRLRollback {
				return
			}
			fresh := filepath.Join(t.TempDir(), "pki")
			if _, err := pki.InstallBundle(fresh, m.KeyPath, tc.raw, false); !errors.Is(err, tc.class) {
				t.Fatalf("fresh home: err %v, want class %v", err, tc.class)
			}
			if _, err := os.Stat(fresh); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a refused install created %s (%v)", fresh, err)
			}
		})
	}
}

// The replace-root refusal carries BOTH roots' fingerprints, so a UI can ask
// the user to confirm the move by naming them; the CLI's text is unchanged.
func TestInstallBundle_RootDiffersNamesBothRoots(t *testing.T) {
	f, f1 := pkitest.New(t), pkitest.New(t)
	m := f.Enroll(t, "laptop", pki.RoleInstance)
	dir := filepath.Join(t.TempDir(), "pki")
	if _, err := pki.InstallBundle(dir, m.KeyPath, bundleFor(t, f, m), false); err != nil {
		t.Fatal(err)
	}
	foreign := bundleFor(t, f1, f1.Enroll(t, "laptop", pki.RoleInstance, m.KeyPath))

	_, err := pki.InstallBundle(dir, m.KeyPath, foreign, false)
	var rd *pki.RootDiffersError
	if !errors.As(err, &rd) {
		t.Fatalf("err %v is not a RootDiffersError", err)
	}
	wantOld, _ := pki.RootID(f.Root.Cert)
	wantNew, _ := pki.RootID(f1.Root.Cert)
	if rd.Installed.Fingerprint != wantOld || rd.Bundle.Fingerprint != wantNew || wantOld == wantNew {
		t.Fatalf("root fingerprints %s -> %s, want %s -> %s", rd.Installed.Fingerprint, rd.Bundle.Fingerprint, wantOld, wantNew)
	}
	// pki's own text names the fingerprints and no CLI flag: the CLI formats
	// its message from the typed error (cmd/identity.go), the desktop from
	// the class.
	if strings.Contains(err.Error(), "--replace-root") || !strings.Contains(err.Error(), wantOld) || !strings.Contains(err.Error(), wantNew) {
		t.Fatalf("root-differs text: %s", err)
	}

	// With replaceRoot the same bundle installs, judged against the NEW
	// root's watermark entry.
	if _, err := pki.InstallBundle(dir, m.KeyPath, foreign, true); err != nil {
		t.Fatal(err)
	}
	cur, err := pki.LoadRootCert(filepath.Join(dir, pki.RootCertFile))
	if err != nil || !cur.Equal(f1.Root.Cert) {
		t.Fatalf("root after replace: %v", err)
	}
}

// writeFileAtomic's rename replaces an EXISTING file. On Windows os.Rename
// is MoveFileEx with MOVEFILE_REPLACE_EXISTING; this is the test that says
// so on the Windows CI leg (internal/pki runs there).
func TestInstallBundle_ReinstallReplacesExistingFiles(t *testing.T) {
	f := pkitest.New(t)
	m := f.Enroll(t, "laptop", pki.RoleInstance)
	dir := filepath.Join(t.TempDir(), "pki")
	if _, err := pki.InstallBundle(dir, m.KeyPath, bundleFor(t, f, m), false); err != nil {
		t.Fatal(err)
	}
	m2 := f.Enroll(t, "laptop", pki.RoleInstance, m.KeyPath) // a new serial for the same key
	if _, err := pki.InstallBundle(dir, m.KeyPath, bundleFor(t, f, m2), false); err != nil {
		t.Fatalf("reinstall over existing files: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, pki.InstanceCertFile))
	if !bytes.Equal(got, m2.CertPEM) {
		t.Fatal("instance.crt was not replaced by the second install")
	}
}

func TestSplitBundle_ReturnsTheRootForConfirmation(t *testing.T) {
	f := pkitest.New(t)
	m := f.Enroll(t, "laptop", pki.RoleInstance)
	b, err := pki.SplitBundle(bundleFor(t, f, m))
	if err != nil {
		t.Fatal(err)
	}
	if !b.Root.Equal(f.Root.Cert) || !b.Leaf.Equal(m.Cert) || b.CRL.Number.Int64() != 1 {
		t.Fatalf("split: %+v", b)
	}
}

func TestStatus_NotEnrolledThenEnrolled(t *testing.T) {
	f := pkitest.New(t)
	m := f.Enroll(t, "laptop", pki.RoleInstance)
	dir := filepath.Join(t.TempDir(), "pki")

	st, err := pki.Status(dir, m.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Fingerprint != m.Fingerprint() || st.Enrolled || st.Cert != nil || st.CRL != nil {
		t.Fatalf("status before install: %+v", st)
	}

	if _, err := pki.InstallBundle(dir, m.KeyPath, bundleFor(t, f, m), false); err != nil {
		t.Fatal(err)
	}
	st, err = pki.Status(dir, m.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Enrolled || st.Cert == nil || st.IdentityErr != nil {
		t.Fatalf("status after install: %+v", st)
	}
	if st.Cert.Fingerprint != m.Fingerprint() || st.Cert.Host != "laptop" ||
		st.Cert.Serial.Cmp(m.Cert.SerialNumber) != 0 || !st.Cert.NotAfter.Equal(m.Cert.NotAfter) {
		t.Fatalf("cert status %+v", st.Cert)
	}
	if st.CRL == nil || st.CRL.Number.Int64() != 1 || st.CRL.NextUpdate.IsZero() {
		t.Fatalf("crl status %+v", st.CRL)
	}
	if st.Root == nil || st.Root.Fingerprint == "" {
		t.Fatalf("root status %+v", st.Root)
	}
}

func TestStatus_KeyUnreadableIsAnError(t *testing.T) {
	if _, err := pki.Status(t.TempDir(), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("Status with no key succeeded")
	}
}

func TestPrincipalKind(t *testing.T) {
	if pki.PrincipalKind(pki.RoleOperator) != "operator" || pki.PrincipalKind(pki.RoleInstance) != "instance" {
		t.Fatal("principal kinds")
	}
}

// An installed root.crt that exists but does not load is NOT "no root
// installed": treating it so would skip the same-root check and let any
// fleet's bundle in. It fails closed, and nothing is written.
func TestInstallBundle_UnreadableInstalledRootFailsClosed(t *testing.T) {
	f, f1 := pkitest.New(t), pkitest.New(t)
	m := f.Enroll(t, "laptop", pki.RoleInstance)
	dir := filepath.Join(t.TempDir(), "pki")
	if _, err := pki.InstallBundle(dir, m.KeyPath, bundleFor(t, f, m), false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, pki.RootCertFile), []byte("garbled"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, dir)
	foreign := bundleFor(t, f1, f1.Enroll(t, "laptop", pki.RoleInstance, m.KeyPath))
	_, err := pki.InstallBundle(dir, m.KeyPath, foreign, false)
	if err == nil || errors.Is(err, pki.ErrRootDiffers) {
		t.Fatalf("installed over an unreadable root.crt: %v", err)
	}
	sameTree(t, before, snapshot(t, dir))
	// With replaceRoot the operator has said "whatever is there, replace it".
	if _, err := pki.InstallBundle(dir, m.KeyPath, foreign, true); err != nil {
		t.Fatalf("replace over an unreadable root.crt: %v", err)
	}
}
