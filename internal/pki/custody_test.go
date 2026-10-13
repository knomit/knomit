package pki

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeKeychain is an in-memory Keychain with injectable failures.
type fakeKeychain struct {
	items                  map[string]string
	getErr, setErr, delErr error
}

func newFakeKeychain() *fakeKeychain { return &fakeKeychain{items: map[string]string{}} }

func (k *fakeKeychain) Get(s, a string) (string, error) {
	if k.getErr != nil {
		return "", k.getErr
	}
	v, ok := k.items[s+"\x00"+a]
	if !ok {
		return "", ErrKeychainItemNotFound
	}
	return v, nil
}

func (k *fakeKeychain) Set(s, a, v string) error {
	if k.setErr != nil {
		return k.setErr
	}
	k.items[s+"\x00"+a] = v
	return nil
}

func (k *fakeKeychain) Delete(s, a string) error {
	if k.delErr != nil {
		return k.delErr
	}
	if _, ok := k.items[s+"\x00"+a]; !ok {
		return ErrKeychainItemNotFound
	}
	delete(k.items, s+"\x00"+a)
	return nil
}

func (k *fakeKeychain) has(root Root) bool {
	id, _ := RootID(root.Cert)
	_, ok := k.items[KeychainService+"\x00"+KeychainAccount(id)]
	return ok
}

var custodyPass = []byte("correct horse")

// countingPass returns a Passphrase and how many times it was called.
func countingPass(p []byte) (Passphrase, *int) {
	n := new(int)
	return func() ([]byte, error) { *n++; return p, nil }, n
}

// signsAs asserts that root's signer signs a fresh nonce verifiably under
// want, the original root public key.
func signsAs(t *testing.T, root Root, want ed25519.PublicKey) {
	t.Helper()
	nonce := make([]byte, 32)
	rand.Read(nonce)
	sig, err := root.Signer.Sign(rand.Reader, nonce, crypto.Hash(0))
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(want, nonce, sig) {
		t.Fatal("a signature by the opened key does not verify with the original root public key")
	}
}

func TestCreateRoot_StoresInTheKeychainUnderTheRootID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "master")
	kc := newFakeKeychain()
	pass, calls := countingPass(custodyPass)
	root, src, err := CreateRoot(dir, "knomit-master-test", time.Hour, kc, pass)
	if err != nil {
		t.Fatal(err)
	}
	if src != RootFromKeychain || *calls != 0 {
		t.Fatalf("source %q, passphrase asked %d times; want keychain, 0", src, *calls)
	}
	id, _ := RootID(root.Cert)
	pub := root.Cert.PublicKey.(ed25519.PublicKey)
	if id != Fingerprint(pub) {
		t.Fatalf("root id %s is not the fingerprint of the root key", id)
	}
	if _, ok := kc.items["knomit\x00fleet-root/"+id]; !ok || len(kc.items) != 1 {
		t.Fatalf("keychain items %v; want exactly knomit / fleet-root/%s", keys(kc.items), id)
	}
	if _, err := os.Stat(filepath.Join(dir, RootKeyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("root.key written although the keychain took the key: %v", err)
	}
	opened, osrc, err := OpenRoot(dir, kc, nil)
	if err != nil || osrc != RootFromKeychain {
		t.Fatalf("OpenRoot: %v, source %q", err, osrc)
	}
	signsAs(t, opened, pub)
	if _, _, err := CreateRoot(dir, "again", time.Hour, kc, pass); err == nil || !strings.Contains(err.Error(), "refusing to replace") {
		t.Fatalf("second CreateRoot over the same dir: %v", err)
	}
}

func TestCreateRoot_KeychainRefusedFallsBackToTheFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "master")
	kc := newFakeKeychain()
	kc.setErr = errors.New("no Secret Service")

	if _, _, err := CreateRoot(dir, "cn", time.Hour, kc, nil); err == nil || !strings.Contains(err.Error(), "no Secret Service") {
		t.Fatalf("no keychain and no passphrase: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, RootCertFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused CreateRoot left root.crt behind: %v", err)
	}

	pass, calls := countingPass(custodyPass)
	root, src, err := CreateRoot(dir, "cn", time.Hour, kc, pass)
	if err != nil || src != RootFromFile || *calls != 1 {
		t.Fatalf("fallback: %v, source %q, passphrase asked %d times", err, src, *calls)
	}
	loaded, err := LoadRoot(dir, custodyPass)
	if err != nil {
		t.Fatal(err)
	}
	signsAs(t, loaded, root.Cert.PublicKey.(ed25519.PublicKey))
}

func TestOpenRoot_KeychainErrorFallsBackToTheFile(t *testing.T) {
	dir := t.TempDir()
	root, err := NewRoot(dir, "knomit-master-test", custodyPass, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	kc := newFakeKeychain()
	kc.getErr = errors.New("keychain locked")
	pass, calls := countingPass(custodyPass)
	opened, src, err := OpenRoot(dir, kc, pass)
	if err != nil {
		t.Fatal(err)
	}
	if src != RootFromFile || *calls != 1 {
		t.Fatalf("source %q, passphrase asked %d times; want file, 1", src, *calls)
	}
	signsAs(t, opened, root.Cert.PublicKey.(ed25519.PublicKey))

	// The keychain item wins when it is there: the passphrase is not asked.
	kc.getErr = nil
	secret, _ := rootSecret(root.Signer.(ed25519.PrivateKey), "cn")
	id, _ := RootID(root.Cert)
	kc.items[KeychainService+"\x00"+KeychainAccount(id)] = string(secret)
	pass2, calls2 := countingPass([]byte("wrong"))
	if _, src, err := OpenRoot(dir, kc, pass2); err != nil || src != RootFromKeychain || *calls2 != 0 {
		t.Fatalf("with the item present: %v, source %q, passphrase asked %d times", err, src, *calls2)
	}

	// An item that holds ANOTHER key is an error, never a reason to use
	// the file.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	bad, _ := rootSecret(other, "cn")
	kc.items[KeychainService+"\x00"+KeychainAccount(id)] = string(bad)
	pass3, calls3 := countingPass(custodyPass)
	if _, _, err := OpenRoot(dir, kc, pass3); err == nil || !strings.Contains(err.Error(), "does not hold the key") || *calls3 != 0 {
		t.Fatalf("mismatched item: %v (passphrase asked %d times)", err, *calls3)
	}
}

func TestOpenRoot_NeitherKeychainNorFileIsAClearError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "master")
	kc := newFakeKeychain()
	if _, _, err := CreateRoot(dir, "cn", time.Hour, kc, nil); err != nil {
		t.Fatal(err)
	}
	empty := newFakeKeychain()
	_, src, err := OpenRoot(dir, empty, func() ([]byte, error) { t.Fatal("passphrase asked with no file to open"); return nil, nil })
	if !errors.Is(err, ErrNoRootKey) || src != "" {
		t.Fatalf("err %v, source %q; want ErrNoRootKey", err, src)
	}
	for _, want := range []string{"fleet-root/", RootKeyFile, "absent"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
	if _, _, err := OpenRoot(t.TempDir(), kc, nil); err == nil || !strings.Contains(err.Error(), "no fleet root") {
		t.Fatalf("a dir with no root.crt: %v", err)
	}
}

func TestExportImport_RoundTripMovesTheKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "master")
	kc := newFakeKeychain()
	root, _, err := CreateRoot(dir, "knomit-master-test", time.Hour, kc, nil)
	if err != nil {
		t.Fatal(err)
	}
	origPub := root.Cert.PublicKey.(ed25519.PublicKey)
	out := filepath.Join(t.TempDir(), "root.export")

	res, err := ExportRoot(dir, out, kc, custodyPass, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.From != RootFromKeychain || !res.RemovedKeychain || res.RemovedFile {
		t.Fatalf("export result %+v; want from keychain, keychain item removed", res)
	}
	if kc.has(root) || len(kc.items) != 0 {
		t.Fatalf("the keychain still holds %v after a move", keys(kc.items))
	}
	if _, _, err := OpenRoot(dir, kc, nil); !errors.Is(err, ErrNoRootKey) {
		t.Fatalf("the old master can still open the root after a move: %v", err)
	}
	if fi, err := os.Stat(out); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("export file: %v %v", fi, err)
	}
	// An export never overwrites a file.
	if _, err := ExportRoot(dir, out, kc, custodyPass, true); err == nil {
		t.Fatal("a second export over an existing file succeeded")
	}

	newDir := filepath.Join(t.TempDir(), "master")
	kc2 := newFakeKeychain()
	if _, _, err := ImportRoot(newDir, out, kc2, []byte("wrong")); err == nil {
		t.Fatal("import with the wrong passphrase succeeded")
	}
	imported, src, err := ImportRoot(newDir, out, kc2, custodyPass)
	if err != nil || src != RootFromKeychain || !kc2.has(root) {
		t.Fatalf("import: %v, source %q, item present %v", err, src, kc2.has(root))
	}
	if !bytes.Equal(imported.Cert.Raw, root.Cert.Raw) {
		t.Fatal("import did not carry the root certificate")
	}
	opened, src, err := OpenRoot(newDir, kc2, nil)
	if err != nil || src != RootFromKeychain {
		t.Fatalf("OpenRoot after import: %v, %q", err, src)
	}
	signsAs(t, opened, origPub)
}

func TestExportRoot_KeepLeavesTheLocalCopy(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "master")
	kc := newFakeKeychain()
	root, _, err := CreateRoot(dir, "cn", time.Hour, kc, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ExportRoot(dir, filepath.Join(t.TempDir(), "backup"), kc, custodyPass, true)
	if err != nil || res.RemovedKeychain || res.RemovedFile {
		t.Fatalf("keep: %v %+v", err, res)
	}
	if !kc.has(root) {
		t.Fatal("--keep removed the keychain item")
	}
	if _, src, err := OpenRoot(dir, kc, nil); err != nil || src != RootFromKeychain {
		t.Fatalf("OpenRoot after a kept export: %v %q", err, src)
	}
}

func TestExportRoot_MovesAFileHeldKey(t *testing.T) {
	dir := t.TempDir()
	root, err := NewRoot(dir, "cn", custodyPass, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	kc := newFakeKeychain()
	out := filepath.Join(t.TempDir(), "export")
	res, err := ExportRoot(dir, out, kc, custodyPass, false)
	if err != nil || res.From != RootFromFile || !res.RemovedFile || res.RemovedKeychain {
		t.Fatalf("export: %v %+v", err, res)
	}
	if _, err := os.Stat(filepath.Join(dir, RootKeyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("root.key survived a move: %v", err)
	}
	// Keychain refuses on the new machine: the import falls back to root.key.
	newDir := t.TempDir()
	kc2 := newFakeKeychain()
	kc2.setErr = errors.New("headless")
	if _, src, err := ImportRoot(newDir, out, kc2, custodyPass); err != nil || src != RootFromFile {
		t.Fatalf("import fallback: %v %q", err, src)
	}
	loaded, err := LoadRoot(newDir, custodyPass)
	if err != nil {
		t.Fatal(err)
	}
	signsAs(t, loaded, root.Cert.PublicKey.(ed25519.PublicKey))
}

func TestImportRoot_RefusesADirHoldingAnotherRoot(t *testing.T) {
	a := t.TempDir()
	if _, err := NewRoot(a, "a", custodyPass, time.Hour); err != nil {
		t.Fatal(err)
	}
	b := t.TempDir()
	if _, err := NewRoot(b, "b", custodyPass, time.Hour); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "a.export")
	if _, err := ExportRoot(a, out, newFakeKeychain(), custodyPass, true); err != nil {
		t.Fatal(err)
	}
	kc := newFakeKeychain()
	if _, _, err := ImportRoot(b, out, kc, custodyPass); err == nil || !strings.Contains(err.Error(), "different fleet root") || len(kc.items) != 0 {
		t.Fatalf("import over another root: %v (items %v)", err, keys(kc.items))
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, strings.ReplaceAll(k, "\x00", " / "))
	}
	return out
}
