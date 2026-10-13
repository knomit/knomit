package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/pki"
)

// Review finding 2: the export passphrase is typed a second time, and a
// mismatch aborts before anything is written or removed.
func TestIdentity_ExportPassphraseMustBeConfirmed(t *testing.T) {
	kc := &cliKeychain{items: map[string]string{}}
	useKeychain(t, kc)
	onTerminal(t)
	dir, passFile := master(t)
	item := rootItem(t, dir)
	export := filepath.Join(t.TempDir(), "root.export")

	for name, c := range map[string]struct{ stdin, passFile string }{
		"typed twice, differing":   {"first\nsecond\n", "-"},
		"file, confirmed wrongly":  {"correct horsf\n", passFile},
		"file, no confirmation":    {"", passFile},
		"typed once, then nothing": {"first\n", "-"},
	} {
		_, err := run(t, c.stdin, "identity", "export-root", "--dir", dir, "--out", export, "--passphrase-file", c.passFile)
		if err == nil || !strings.Contains(err.Error(), "not confirmed") {
			t.Fatalf("%s: err %v, want a confirmation refusal", name, err)
		}
		if _, err := os.Stat(export); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: the export was written: %v", name, err)
		}
		if _, ok := kc.items[item]; !ok {
			t.Fatalf("%s: the keychain item was removed", name)
		}
	}

	// Typed twice, the same: the move happens, and the typed passphrase
	// opens the export on import.
	if out, err := run(t, "new pass\nnew pass\n", "identity", "export-root", "--dir", dir, "--out", export, "--passphrase-file", "-"); err != nil {
		t.Fatalf("confirmed export: %v\n%s", err, out)
	}
	if len(kc.items) != 0 {
		t.Fatalf("confirmed export left %v", kc.items)
	}
	if out, err := run(t, "new pass\n", "identity", "import-root", export, "--dir", filepath.Join(t.TempDir(), "m"), "--passphrase-file", "-"); err != nil {
		t.Fatalf("import with the confirmed passphrase: %v\n%s", err, out)
	}
}

// Review finding 1 at the CLI: --out naming the master's root.key is refused
// and the key still opens.
func TestIdentity_ExportOntoRootKeyIsRefused(t *testing.T) {
	kc := &cliKeychain{items: map[string]string{}}
	useKeychain(t, kc)
	onTerminal(t)
	dir, passFile := master(t)
	_, err := run(t, "correct horse\n", "identity", "export-root", "--dir", dir, "--out", filepath.Join(dir, pki.RootKeyFile), "--passphrase-file", passFile)
	if !errors.Is(err, pki.ErrExportOntoRootKey) {
		t.Fatalf("export onto root.key: %v", err)
	}
	_, _, pubPath := instanceHome(t, "laptop")
	if out, err := run(t, "", "identity", "enroll", "--dir", dir, "--pubkey", pubPath); err != nil || !strings.Contains(out, "from the OS keychain") {
		t.Fatalf("the root no longer opens after the refused export: %v\n%s", err, out)
	}
}

// Review finding 5: a keychain that could not be read (key taken from
// root.key) and then could not delete must not be reported as a clean move.
func TestIdentity_ExportWarnsWhenTheKeychainMayStillHoldTheKey(t *testing.T) {
	kc := &cliKeychain{items: map[string]string{}, setErr: errors.New("no Secret Service")}
	useKeychain(t, kc)
	onTerminal(t)
	dir, passFile := master(t) // file custody: root.key
	kc.getErr = errors.New("keychain locked")
	kc.delErr = errors.New("keychain locked")
	out, err := run(t, "correct horse\n", "identity", "export-root", "--dir", dir, "--out", filepath.Join(t.TempDir(), "x"), "--passphrase-file", passFile)
	if err != nil {
		t.Fatalf("export: %v\n%s", err, out)
	}
	if strings.Contains(out, "can no longer sign") {
		t.Fatalf("claimed the machine can no longer sign although the keychain delete failed:\n%s", out)
	}
	for _, want := range []string{"may STILL hold", "keychain locked", "fleet-root/"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output does not say %q:\n%s", want, out)
		}
	}
}

// Review finding 4: the fingerprint `install --root` takes can be printed
// from root.crt alone, with the key moved away.
func TestIdentity_RootFingerprintNeedsOnlyRootCrt(t *testing.T) {
	kc := &cliKeychain{items: map[string]string{}}
	useKeychain(t, kc)
	dir, _ := master(t)
	c, _ := pki.LoadRootCert(filepath.Join(dir, pki.RootCertFile))
	want, _ := pki.RootID(c)
	kc.items = map[string]string{} // no key anywhere
	out, err := run(t, "", "identity", "root-fingerprint", "--dir", dir)
	if err != nil || strings.TrimSpace(out) != want {
		t.Fatalf("root-fingerprint: %v %q, want %s", err, out, want)
	}
	if strings.Contains(rootFingerprintForm, "root.key") {
		t.Fatal("the --root help still sends operators to root.key")
	}
}

// Review finding 3: with the key in the keychain, oauth --sign asks no
// passphrase (it would guard nothing); the confirmation is a y/N answer
// after the printout, or --yes. --dir defaults like the identity commands.
func TestOAuthSign_KeychainCustodyAsksYesNoNotAPassphrase(t *testing.T) {
	useKeychain(t, &cliKeychain{items: map[string]string{}})
	dir, _, fp := signSetup(t)
	fi := newFakeIssuer(t, "Claude Code", "http://localhost:52346/callback")
	args := []string{"oauth", "approve", signID, "--sign", fi.srv.URL, "--dir", dir, "--instance", fp, "--as", "laptop"}

	stdout, stderr, err := runSplit(t, "n\n", args...)
	if err == nil || !strings.Contains(err.Error(), "not confirmed") || strings.Contains(stdout, `"signature"`) {
		t.Fatalf("answer n: %v\nstdout %s", err, stdout)
	}
	if strings.Contains(stderr, "Type the master passphrase") || !strings.Contains(stderr, "[y/N]") {
		t.Fatalf("keychain custody prompted for a passphrase or asked nothing:\n%s", stderr)
	}
	stdout, stderr, err = runSplit(t, "y\n", args...)
	if err != nil || !strings.Contains(stdout, `"signature"`) || strings.Contains(stderr, "Type the master passphrase") {
		t.Fatalf("answer y: %v\nstdout %s\nstderr %s", err, stdout, stderr)
	}
	// --passphrase-file - is accepted and ignored for the key: still a y/N.
	if stdout, _, err := runSplit(t, "y\n", append(args, "--passphrase-file", "-")...); err != nil || !strings.Contains(stdout, `"signature"`) {
		t.Fatalf("with --passphrase-file -: %v\n%s", err, stdout)
	}
	// --yes signs with no question at all.
	if stdout, stderr, err := runSplit(t, "", append(args, "--yes")...); err != nil || !strings.Contains(stdout, `"signature"`) || strings.Contains(stderr, "[y/N]") {
		t.Fatalf("--yes: %v\nstdout %s\nstderr %s", err, stdout, stderr)
	}
}
