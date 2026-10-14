package pki

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"knomit/internal/platform/privdir"
)

// posixModes gates file-MODE assertions. Windows has no POSIX permission
// bits — os.Stat reports 0666/0444 whatever WriteFile was asked for — so a
// mode check there can neither pass nor mean anything. The existence and
// content halves of those assertions still run everywhere.
var posixModes = runtime.GOOS != "windows"

func newTestRoot(t *testing.T) (string, Root) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "fleet")
	r, err := NewRoot(dir, "knomit-fleet-root-test", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return dir, r
}

func parsePEMCert(t *testing.T, b []byte) *x509.Certificate {
	t.Helper()
	blk, rest := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
		t.Fatalf("want exactly one CERTIFICATE block, got %q", b)
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The root key is a plain file, like an SSH key: an unencrypted OpenSSH
// private key, 0600, in a directory private to the user, and LoadRoot opens
// it asking for nothing.
func TestNewRoot_IsSelfSignedCAInAPlainOwnerOnlyKeyFile(t *testing.T) {
	dir, r := newTestRoot(t)
	if !r.Cert.IsCA || !r.Cert.BasicConstraintsValid {
		t.Fatal("root is not a CA")
	}
	if r.Cert.KeyUsage != x509.KeyUsageCertSign|x509.KeyUsageCRLSign {
		t.Fatalf("root KeyUsage = %v", r.Cert.KeyUsage)
	}
	if err := r.Cert.CheckSignatureFrom(r.Cert); err != nil {
		t.Fatalf("root not self-signed: %v", err)
	}
	back, err := LoadRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Cert.Equal(r.Cert) {
		t.Fatal("reloaded root certificate differs")
	}
	if !back.Signer.Public().(ed25519.PublicKey).Equal(r.Cert.PublicKey) {
		t.Fatal("reloaded root key does not match its certificate")
	}
	// Unencrypted OpenSSH: the same parser that reads an instance's
	// id_ed25519 (no passphrase) reads it, and it is the root's key.
	keyPath := filepath.Join(dir, RootKeyFile)
	raw, _ := os.ReadFile(keyPath)
	if !strings.Contains(string(raw), "OPENSSH PRIVATE KEY") {
		t.Fatalf("root.key is not OpenSSH format: %.40q", raw)
	}
	_, pub, err := LoadSigner(keyPath)
	if err != nil {
		t.Fatalf("root.key does not parse as a plain OpenSSH key: %v", err)
	}
	if !pub.Equal(r.Cert.PublicKey) {
		t.Fatal("root.key holds a key other than the root's")
	}
	if posixModes {
		if fi, _ := os.Stat(keyPath); fi.Mode().Perm() != 0o600 {
			t.Fatalf("root.key mode %v, want 0600", fi.Mode().Perm())
		}
		if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
			t.Fatalf("fleet root dir mode %v, want 0700", fi.Mode().Perm())
		}
	}
	if c, err := LoadRootCert(filepath.Join(dir, RootCertFile)); err != nil || !c.Equal(r.Cert) {
		t.Fatalf("LoadRootCert: %v", err)
	}
}

// A fleet root directory that already exists wider than private (made by
// hand, or by an older build) is tightened before the key is written into
// it: a plain key in a 0755 directory would be readable by its neighbours'
// listing and, under a wide umask, by them.
func TestNewRoot_TightensAnExistingWideDirectory(t *testing.T) {
	if !posixModes {
		t.Skip("mode bits; the Windows DACL is asserted in root_windows_test.go")
	}
	dir := filepath.Join(t.TempDir(), "fleet")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { // past the umask
		t.Fatal(err)
	}
	if _, err := NewRoot(dir, "knomit-fleet-root-test", time.Hour); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("fleet root dir mode %v after NewRoot, want 0700", fi.Mode().Perm())
	}
}

func TestNewRoot_RefusesToReplaceARoot(t *testing.T) {
	dir, _ := newTestRoot(t)
	before, _ := os.ReadFile(filepath.Join(dir, RootKeyFile))
	if _, err := NewRoot(dir, "again", time.Hour); err == nil || !strings.Contains(err.Error(), "refusing to replace the fleet root") {
		t.Fatalf("NewRoot over an existing root: %v, want a refusal", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, RootKeyFile))
	if string(before) != string(after) {
		t.Fatal("root.key changed on a refused NewRoot")
	}
	// A root.crt alone (the key was copied away) is a root too: a new key
	// beside it would never pair with it.
	if err := os.Remove(filepath.Join(dir, RootKeyFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRoot(dir, "again", time.Hour); err == nil || !strings.Contains(err.Error(), "refusing to replace the fleet root") {
		t.Fatalf("NewRoot over an existing root.crt: %v, want a refusal", err)
	}
	if _, err := os.Stat(filepath.Join(dir, RootKeyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused NewRoot wrote root.key: %v", err)
	}
}

// No root.key is "not the master", the sentinel callers branch on; a key
// that is not the certificate's is refused; and a passphrase-encrypted key
// (an older build's) is refused, never prompted for.
func TestLoadRoot_Refusals(t *testing.T) {
	dir, _ := newTestRoot(t)
	keyPath := filepath.Join(dir, RootKeyFile)
	good, _ := os.ReadFile(keyPath)

	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRoot(dir); !errors.Is(err, ErrNoRootKey) {
		t.Fatalf("LoadRoot without root.key: %v, want ErrNoRootKey", err)
	}

	_, other, _ := ed25519.GenerateKey(rand.Reader)
	blk, _ := ssh.MarshalPrivateKey(other, "")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(blk), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRoot(dir); err == nil || !strings.Contains(err.Error(), "not a pair") {
		t.Fatalf("LoadRoot with another key: %v, want \"not a pair\"", err)
	}

	k, err := ssh.ParseRawPrivateKey(good)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := ssh.MarshalPrivateKeyWithPassphrase(k, "", []byte("old build"))
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(enc), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadRoot(dir)
	if err == nil || errors.Is(err, ErrNoRootKey) {
		t.Fatalf("LoadRoot with an encrypted root.key: %v, want a refusal that is not ErrNoRootKey", err)
	}
	if fix := "ssh-keygen -p -N '' -f " + keyPath; !strings.Contains(err.Error(), fix) {
		t.Fatalf("encrypted root.key refusal %q does not name the fix %q", err, fix)
	}
}

// symlinkOrSkip makes link -> target, skipping where the OS will not (an
// unprivileged Windows account without developer mode).
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if !posixModes {
			t.Skipf("cannot create a symlink here: %v", err)
		}
		t.Fatal(err)
	}
}

// A fleet root directory that is a symlink is refused before the key is
// written: privdir.Ensure only warns about it, and the link can be
// repointed by whoever controls its parent. Nothing lands in the target.
func TestNewRoot_RefusesASymlinkedFleetDir(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "fleet")
	symlinkOrSkip(t, target, dir)
	_, err := NewRoot(dir, "knomit-fleet-root-test", time.Hour)
	if !errors.Is(err, privdir.ErrNotPrivate) || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("NewRoot into a symlinked dir: %v, want privdir.ErrNotPrivate naming the symlink", err)
	}
	for _, f := range []string{RootKeyFile, RootCertFile} {
		if _, err := os.Lstat(filepath.Join(target, f)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a refused NewRoot wrote %s through the link: %v", f, err)
		}
	}
}

// A root.key that is a DANGLING symlink passes the os.Stat pre-check (Stat
// follows the link and finds nothing), so O_EXCL is what refuses it. Without
// O_EXCL the plain key would be written to wherever the link points.
func TestNewRoot_RefusesADanglingRootKeySymlink(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "fleet")
	if err := privdir.Ensure(dir); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "elsewhere.key")
	symlinkOrSkip(t, target, filepath.Join(dir, RootKeyFile))
	if _, err := os.Stat(filepath.Join(dir, RootKeyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture: the link is not dangling: %v", err)
	}
	if _, err := NewRoot(dir, "knomit-fleet-root-test", time.Hour); err == nil {
		t.Fatal("NewRoot over a dangling root.key symlink: accepted")
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("NewRoot wrote the key through the dangling link to %s: %v", target, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, RootCertFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused NewRoot left root.crt: %v", err)
	}
}

// LoadRoot refuses a fleet root directory that is not private, and says
// how to fix it: a "copy the directory" move (cp -r, an unzip) lands it at
// 0755, and the signer must not run from there.
func TestLoadRoot_RefusesAFleetDirThatIsNotPrivate(t *testing.T) {
	if !posixModes {
		t.Skip("mode bits; the Windows DACL is asserted in root_windows_test.go")
	}
	dir, _ := newTestRoot(t)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := LoadRoot(dir)
	if !errors.Is(err, privdir.ErrNotPrivate) || !strings.Contains(err.Error(), "chmod 700 "+dir) {
		t.Fatalf("LoadRoot from a 0755 fleet dir: %v, want privdir.ErrNotPrivate naming chmod 700", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRoot(dir); err != nil {
		t.Fatalf("LoadRoot after chmod 700: %v", err)
	}

	link := filepath.Join(t.TempDir(), "fleet-link")
	symlinkOrSkip(t, dir, link)
	if _, err := LoadRoot(link); !errors.Is(err, privdir.ErrNotPrivate) {
		t.Fatalf("LoadRoot through a symlinked fleet dir: %v, want privdir.ErrNotPrivate", err)
	}
}

// LoadRoot refuses a root.key that other users can read, the way ssh
// refuses an UNPROTECTED PRIVATE KEY FILE, naming the chmod; and a root.key
// that is a symlink, whatever it points to.
func TestLoadRoot_RefusesAnExposedRootKey(t *testing.T) {
	dir, _ := newTestRoot(t)
	keyPath := filepath.Join(dir, RootKeyFile)
	if posixModes {
		for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o660} {
			if err := os.Chmod(keyPath, mode); err != nil {
				t.Fatal(err)
			}
			_, err := LoadRoot(dir)
			if !errors.Is(err, ErrUnprotectedRootKey) || !strings.Contains(err.Error(), "chmod 600 "+keyPath) {
				t.Fatalf("LoadRoot with root.key %v: %v, want ErrUnprotectedRootKey naming chmod 600", mode, err)
			}
		}
		if err := os.Chmod(keyPath, 0o400); err != nil { // stricter than 0600 is fine
			t.Fatal(err)
		}
		if _, err := LoadRoot(dir); err != nil {
			t.Fatalf("LoadRoot with root.key 0400: %v", err)
		}
		if err := os.Chmod(keyPath, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// The same key, moved out and linked back: a valid pair, refused only
	// because root.key is a link.
	moved := filepath.Join(t.TempDir(), "real.key")
	if err := os.Rename(keyPath, moved); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, moved, keyPath)
	_, err := LoadRoot(dir)
	if !errors.Is(err, ErrUnprotectedRootKey) || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("LoadRoot with a symlinked root.key: %v, want ErrUnprotectedRootKey naming the symlink", err)
	}
}

func TestIssueInstance_LeafChainsToRootWithEmailSANAndIsLogged(t *testing.T) {
	dir, root := newTestRoot(t)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	certPEM, rec, err := IssueInstance(dir, root, pub, "laptop.local@agents.knomit.io", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c := parsePEMCert(t, certPEM)

	pool := x509.NewCertPool()
	pool.AddCert(root.Cert)
	for _, eku := range []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth} {
		if _, err := c.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{eku}}); err != nil {
			t.Fatalf("leaf does not chain to root for EKU %v: %v", eku, err)
		}
	}
	if c.IsCA {
		t.Fatal("instance certificate must be a LEAF")
	}
	if c.KeyUsage != x509.KeyUsageDigitalSignature {
		t.Fatalf("KeyUsage = %v", c.KeyUsage)
	}
	fp := Fingerprint(pub)
	wantSAN := "laptop.local@agents.knomit.io"
	if len(c.URIs) != 0 || len(c.EmailAddresses) != 1 || c.EmailAddresses[0] != wantSAN {
		t.Fatalf("URIs = %v emails = %v, want only the email SAN %s", c.URIs, c.EmailAddresses, wantSAN)
	}
	// No private extension: the identity lives in the email SAN (R1).
	allowed := map[string]bool{
		"2.5.29.14": true, // subjectKeyIdentifier
		"2.5.29.15": true, // keyUsage
		"2.5.29.17": true, // subjectAltName (the email address)
		"2.5.29.19": true, // basicConstraints
		"2.5.29.35": true, // authorityKeyIdentifier
		"2.5.29.37": true, // extKeyUsage
	}
	var ids []string
	for _, ext := range c.Extensions {
		ids = append(ids, ext.Id.String())
		if !allowed[ext.Id.String()] {
			t.Fatalf("leaf carries extension %s outside the standard set", ext.Id)
		}
	}
	if len(ids) < 4 { // positive control: the loop saw the real extensions
		t.Fatalf("only %d extensions seen: %v", len(ids), ids)
	}
	if !c.PublicKey.(ed25519.PublicKey).Equal(pub) {
		t.Fatal("certificate wraps a different key")
	}
	if c.SerialNumber.BitLen() < 64 {
		t.Fatalf("serial has only %d bits", c.SerialNumber.BitLen())
	}

	// Issuance log: exactly one line, same serial, full fingerprint.
	f, err := os.Open(filepath.Join(dir, IssuedLogFile))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []Issued
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var got Issued
		if err := json.Unmarshal(sc.Bytes(), &got); err != nil {
			t.Fatalf("bad log line %q: %v", sc.Text(), err)
		}
		lines = append(lines, got)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("issued.jsonl has %d lines, want 1", len(lines))
	}
	if lines[0].Serial != c.SerialNumber.Text(16) || lines[0].Serial != rec.Serial {
		t.Fatalf("logged serial %s, cert %s, returned %s", lines[0].Serial, c.SerialNumber.Text(16), rec.Serial)
	}
	if lines[0].Fingerprint != fp || lines[0].Email != wantSAN || !lines[0].NotAfter.Equal(c.NotAfter) {
		t.Fatalf("log record %+v", lines[0])
	}
	if fi, _ := os.Stat(filepath.Join(dir, IssuedLogFile)); posixModes && fi.Mode().Perm() != 0o600 {
		t.Fatalf("issued.jsonl mode %v", fi.Mode().Perm())
	}

	// A second issuance APPENDS.
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, _, err := IssueInstance(dir, root, pub2, "other@example.com", time.Hour); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, IssuedLogFile))
	if n := strings.Count(string(raw), "\n"); n != 2 {
		t.Fatalf("after two issuances the log has %d lines", n)
	}
}

func TestIssueInstance_RefusesAnUnusableEmail(t *testing.T) {
	dir, root := newTestRoot(t)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	for _, email := range []string{"h", "a+b@agents.knomit.io", "a b@agents.knomit.io", "@example.com"} {
		if _, _, err := IssueInstance(dir, root, pub, email, time.Hour); err == nil {
			t.Fatalf("issued a certificate for email %q", email)
		}
	}
	// Positive control: a hyphenated id is fine.
	if _, _, err := IssueInstance(dir, root, pub, "my-lap-top@agents.knomit.io", time.Hour); err != nil {
		t.Fatalf("hyphenated host refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, IssuedLogFile)); err != nil {
		t.Fatal("the one valid issuance was not logged")
	}
}
