package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"knomit/internal/pki"
)

// cliKeychain is an in-memory pki.Keychain for the CLI tests.
type cliKeychain struct {
	items  map[string]string
	setErr error
}

func (k *cliKeychain) Get(s, a string) (string, error) {
	v, ok := k.items[s+"/"+a]
	if !ok {
		return "", pki.ErrKeychainItemNotFound
	}
	return v, nil
}

func (k *cliKeychain) Set(s, a, v string) error {
	if k.setErr != nil {
		return k.setErr
	}
	k.items[s+"/"+a] = v
	return nil
}

func (k *cliKeychain) Delete(s, a string) error {
	if _, ok := k.items[s+"/"+a]; !ok {
		return pki.ErrKeychainItemNotFound
	}
	delete(k.items, s+"/"+a)
	return nil
}

// useKeychain points the identity commands at kc for one test.
func useKeychain(t *testing.T, kc pki.Keychain) {
	t.Helper()
	prev := rootKeychain
	rootKeychain = kc
	t.Cleanup(func() { rootKeychain = prev })
}

// onTerminal makes export-root believe it runs on a terminal.
func onTerminal(t *testing.T) {
	t.Helper()
	prev := stdinIsTerminal
	stdinIsTerminal = func(*cobra.Command) bool { return true }
	t.Cleanup(func() { stdinIsTerminal = prev })
}

func rootItem(t *testing.T, dir string) string {
	t.Helper()
	c, err := pki.LoadRootCert(filepath.Join(dir, pki.RootCertFile))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := pki.RootID(c)
	return pki.KeychainService + "/" + pki.KeychainAccount(id)
}

// The custody flow end to end on the CLI: init-master puts the key in the
// keychain (no root.key), enroll works with no passphrase, export-root
// moves the key out so enroll then fails, import-root brings it back into
// another master directory, and enroll works there.
func TestIdentity_RootCustodyExportMovesImportRestores(t *testing.T) {
	kc := &cliKeychain{items: map[string]string{}}
	useKeychain(t, kc)
	dir, passFile := master(t)
	item := rootItem(t, dir)
	if _, ok := kc.items[item]; !ok || len(kc.items) != 1 {
		t.Fatalf("keychain after init-master: %v; want only %s", kc.items, item)
	}
	if _, err := os.Stat(filepath.Join(dir, pki.RootKeyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("init-master with a keychain wrote root.key: %v", err)
	}
	_, _, pubPath := instanceHome(t, "laptop")
	out, err := run(t, "", "identity", "enroll", "--dir", dir, "--pubkey", pubPath)
	if err != nil || !strings.Contains(out, "from the OS keychain") {
		t.Fatalf("enroll from the keychain without a passphrase: %v\n%s", err, out)
	}

	export := filepath.Join(t.TempDir(), "root.export")
	// Refused without --out and without a terminal; the key stays put.
	if _, err := run(t, "", "identity", "export-root", "--dir", dir, "--passphrase-file", passFile); err == nil || !strings.Contains(err.Error(), "--out is required") {
		t.Fatalf("export without --out: %v", err)
	}
	if _, err := run(t, "", "identity", "export-root", "--dir", dir, "--out", export, "--passphrase-file", passFile); err == nil || !strings.Contains(err.Error(), "only on a terminal") {
		t.Fatalf("export off a terminal: %v", err)
	}
	if _, err := os.Stat(export); !errors.Is(err, os.ErrNotExist) || len(kc.items) != 1 {
		t.Fatalf("a refused export wrote %v or moved the key (items %d)", err, len(kc.items))
	}

	onTerminal(t)
	out, err = run(t, "", "identity", "export-root", "--dir", dir, "--out", export, "--passphrase-file", passFile)
	if err != nil || !strings.Contains(out, "removed the local copy") {
		t.Fatalf("export-root: %v\n%s", err, out)
	}
	if len(kc.items) != 0 {
		t.Fatalf("export-root (a move) left %v in the keychain", kc.items)
	}
	if _, err := run(t, "", "identity", "enroll", "--dir", dir, "--pubkey", pubPath); !errors.Is(err, pki.ErrNoRootKey) {
		t.Fatalf("enroll after the key moved away: %v, want ErrNoRootKey", err)
	}

	newDir := filepath.Join(t.TempDir(), "master")
	out, err = run(t, "", "identity", "import-root", export, "--dir", newDir, "--passphrase-file", passFile)
	if err != nil || !strings.Contains(out, "OS keychain") {
		t.Fatalf("import-root: %v\n%s", err, out)
	}
	if _, ok := kc.items[item]; !ok {
		t.Fatalf("import-root did not file the key under %s: %v", item, kc.items)
	}
	// enroll in the new master dir needs the ledgers it does not have yet
	// only for the CRL; give it the old CRL, as a takeover would from main.
	crl, _ := os.ReadFile(filepath.Join(dir, pki.CRLFile))
	os.WriteFile(filepath.Join(newDir, pki.CRLFile), crl, 0o644)
	if out, err := run(t, "", "identity", "enroll", "--dir", newDir, "--pubkey", pubPath); err != nil || !strings.Contains(out, "BEGIN X509 CRL") {
		t.Fatalf("enroll after import: %v\n%s", err, out)
	}
}

func TestIdentity_ExportRootKeepLeavesTheKey(t *testing.T) {
	kc := &cliKeychain{items: map[string]string{}}
	useKeychain(t, kc)
	onTerminal(t)
	dir, passFile := master(t)
	out, err := run(t, "", "identity", "export-root", "--dir", dir, "--keep", "--out", filepath.Join(t.TempDir(), "backup"), "--passphrase-file", passFile)
	if err != nil || !strings.Contains(out, "kept the local copy") {
		t.Fatalf("export --keep: %v\n%s", err, out)
	}
	if _, ok := kc.items[rootItem(t, dir)]; !ok {
		t.Fatal("export --keep removed the keychain item")
	}
}

// --dir is optional: the master directory defaults to <home>/master.
func TestIdentity_MasterDirDefaultsUnderHome(t *testing.T) {
	useKeychain(t, &cliKeychain{items: map[string]string{}})
	home := t.TempDir()
	useHome(t, home)
	if out, err := run(t, "", "identity", "init-master", "--cn", "knomit-master-test"); err != nil {
		t.Fatalf("init-master without --dir: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home, masterDirName, pki.RootCertFile)); err != nil {
		t.Fatalf("no root.crt under <home>/%s: %v", masterDirName, err)
	}
	_, _, pubPath := instanceHome(t, "laptop")
	if out, err := run(t, "", "identity", "enroll", "--pubkey", pubPath); err != nil || !strings.Contains(out, "BEGIN CERTIFICATE") {
		t.Fatalf("enroll without --dir: %v\n%s", err, out)
	}
}
