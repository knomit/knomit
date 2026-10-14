package cmd

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"knomit/internal/config"
	"knomit/internal/pki"
	"knomit/internal/repos"
)

// run executes the real root command with args and returns stdout+stderr.
func run(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	root := RootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// instanceHome makes a knomit home holding an instance key written the way
// ensureKeyPair writes it, with the .pub line `knomit serve` prints.
func instanceHome(t *testing.T, host string) (home string, pub ed25519.PublicKey, pubPath string) {
	t.Helper()
	home = t.TempDir()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, _ := ssh.MarshalPrivateKey(priv, "")
	os.WriteFile(filepath.Join(home, "id_ed25519"), pem.EncodeToMemory(block), 0o600)
	sshPub, _ := ssh.NewPublicKey(pub)
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " knomit@" + host + "\n"
	pubPath = filepath.Join(home, "id_ed25519.pub")
	os.WriteFile(pubPath, []byte(line), 0o644)
	return home, pub, pubPath
}

// useHome points config.Load at home and neutralises every env override that
// would otherwise reach a developer's REAL key or pki dir from their shell.
func useHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("KNOMIT_HOME", home)
	for _, k := range []string{"KNOMIT_REMOTE_SSH_KEY", "KNOMIT_TLS_ADDR", "KNOMIT_TLS_DIR"} {
		t.Setenv(k, "")
	}
}

func fleetRoot(t *testing.T) (dir, passFile string) {
	t.Helper()
	base := t.TempDir()
	dir = filepath.Join(base, "fleet")
	passFile = filepath.Join(base, "pass")
	os.WriteFile(passFile, []byte("correct horse\n"), 0o600)
	out, err := run(t, "", "identity", "init-root", "--dir", dir, "--cn", "knomit-fleet-root-test", "--passphrase-file", passFile)
	if err != nil || !strings.Contains(out, "BEGIN CERTIFICATE") {
		t.Fatalf("init-root: %v\n%s", err, out)
	}
	return dir, passFile
}

func enroll(t *testing.T, dir, passFile, pubPath string) string {
	t.Helper()
	bundle := filepath.Join(t.TempDir(), "bundle.pem")
	if out, err := run(t, "", "identity", "enroll", "--dir", dir, "--pubkey", pubPath, "--passphrase-file", passFile, "--out", bundle); err != nil {
		t.Fatalf("enroll: %v\n%s", err, out)
	}
	return bundle
}

// The offline flow end to end: init-root → enroll the instance's EXISTING
// key → install on the instance → show names the same fingerprint the branch
// name carries → revoke → the reissued CRL lists the serial.
func TestIdentity_OfflineEnrollmentRoundTrip(t *testing.T) {
	dir, passFile := fleetRoot(t)
	home, pub, pubPath := instanceHome(t, "laptop")
	bundle := enroll(t, dir, passFile, pubPath)

	useHome(t, home)
	out, err := run(t, "", "identity", "install", "--bundle", bundle, "--yes")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	fp := pki.Fingerprint(pub)
	if !strings.Contains(out, "principal: instance:"+fp+"@cert") || !strings.Contains(out, "san: knomit://instance/laptop\n") {
		t.Fatalf("install output:\n%s", out)
	}
	if !strings.Contains(out, "[tls]") { // addr unset: it tells the operator what to add
		t.Fatalf("install did not print the [tls] line to add:\n%s", out)
	}
	pkiDir := filepath.Join(home, "pki")
	// Mode checks are unix-only: Windows has no POSIX permission bits.
	posixModes := runtime.GOOS != "windows"
	if fi, _ := os.Stat(pkiDir); posixModes && fi.Mode().Perm() != 0o700 {
		t.Fatalf("pki dir mode %v", fi.Mode().Perm())
	}
	for _, f := range []string{pki.InstanceCertFile, pki.RootCertFile, pki.CRLFile} {
		if fi, err := os.Stat(filepath.Join(pkiDir, f)); err != nil || (posixModes && fi.Mode().Perm() != 0o600) {
			t.Fatalf("%s: %v %v", f, fi, err)
		}
	}
	if _, err := pki.ServerConfig(pkiDir, filepath.Join(home, "id_ed25519"), nil); err != nil {
		t.Fatalf("installed files do not load as a server config: %v", err)
	}

	// show: the short fingerprint is the branch-name one (identity.go:89-92).
	out, err = run(t, "", "identity", "show")
	if err != nil {
		t.Fatal(err)
	}
	sshPub, _ := ssh.NewPublicKey(pub)
	h := sha256.Sum256(sshPub.Marshal())
	if branchFP := hex.EncodeToString(h[:])[:8]; !strings.Contains(out, "short: "+branchFP) || !strings.Contains(out, "fingerprint: "+fp) {
		t.Fatalf("show does not name the branch fingerprint %s:\n%s", branchFP, out)
	}
	if !strings.Contains(out, "crl_number: 1") || !strings.Contains(out, "tls_listener: off") {
		t.Fatalf("show:\n%s", out)
	}

	// revoke by fingerprint; the reissued CRL lists its serial.
	out, err = run(t, "", "identity", "revoke", "--dir", dir, "--fingerprint", fp, "--passphrase-file", passFile)
	if err != nil {
		t.Fatalf("revoke: %v\n%s", err, out)
	}
	crl, err := pki.LoadCRL(filepath.Join(dir, pki.CRLFile))
	if err != nil {
		t.Fatal(err)
	}
	certRaw, _ := os.ReadFile(filepath.Join(pkiDir, pki.InstanceCertFile))
	leaf := parseCert(t, certRaw)
	if !pki.IsRevoked(crl, leaf.SerialNumber) || crl.Number.Cmp(big.NewInt(2)) != 0 {
		t.Fatalf("CRL Number %v, lists serial: %v", crl.Number, pki.IsRevoked(crl, leaf.SerialNumber))
	}
	// A serial the fleet root never issued is refused rather than revoking nothing.
	if _, err := run(t, "", "identity", "revoke", "--dir", dir, "--serial", "abc123", "--passphrase-file", passFile); err == nil {
		t.Fatal("revoke of an unknown serial succeeded")
	}
}

func TestIdentity_InstallRefusals(t *testing.T) {
	dir, passFile := fleetRoot(t)
	home, _, pubPath := instanceHome(t, "laptop")
	_, _, otherPub := instanceHome(t, "other")
	useHome(t, home)

	// 1. A bundle for ANOTHER instance's key.
	if out, err := run(t, "", "identity", "install", "--bundle", enroll(t, dir, passFile, otherPub)); err == nil || !strings.Contains(err.Error(), "not this instance's") {
		t.Fatalf("installed a bundle for another key: %v\n%s", err, out)
	}

	// 2. Positive control, then an OLDER CRL than the one already held.
	oldBundle := enroll(t, dir, passFile, pubPath) // carries CRL #1
	if _, err := run(t, "", "identity", "install", "--bundle", oldBundle, "--yes"); err != nil {
		t.Fatal(err)
	}
	// Revoke some unrelated certificate so the fleet root's CRL moves to #2,
	// enroll again (bundle carries #2), install it, then try the #1 bundle.
	_, _, strayPub := instanceHome(t, "stray")
	stray := parseCert(t, readFirstCert(t, enroll(t, dir, passFile, strayPub)))
	if _, err := run(t, "", "identity", "revoke", "--dir", dir, "--serial", stray.SerialNumber.Text(16), "--passphrase-file", passFile); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "", "identity", "install", "--bundle", enroll(t, dir, passFile, pubPath)); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "", "identity", "install", "--bundle", oldBundle); err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatalf("installed a bundle with an older CRL: %v", err)
	}

	// 3. A bundle from a DIFFERENT fleet root: refused unless --replace-root.
	dir2, pass2 := fleetRoot(t)
	foreign := enroll(t, dir2, pass2, pubPath)
	if _, err := run(t, "", "identity", "install", "--bundle", foreign); err == nil || !strings.Contains(err.Error(), "--replace-root") {
		t.Fatalf("switched fleets without --replace-root: %v", err)
	}
	// With it, the new fleet's CRL #1 is accepted: it is judged against the
	// NEW root's watermark entry (none yet), not the old root's #2.
	if out, err := run(t, "", "identity", "install", "--bundle", foreign, "--replace-root", "--yes"); err != nil {
		t.Fatalf("--replace-root: %v\n%s", err, out)
	}
	// The bounce: back to the FIRST fleet with its OLD bundle (CRL #1, while
	// that root's watermark is #2). Nothing was reset on the way through the
	// other fleet, so this is still a rollback. (The two fleet roots share a
	// CommonName, so a CN-keyed watermark would not tell them apart either.)
	if _, err := run(t, "", "identity", "install", "--bundle", oldBundle, "--replace-root", "--yes"); err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatalf("A -> B -> old A bundle was accepted: %v", err)
	}
	// Positive control: a CURRENT bundle of the first fleet (CRL #2) moves back.
	if out, err := run(t, "", "identity", "install", "--bundle", enroll(t, dir, passFile, pubPath), "--replace-root", "--yes"); err != nil {
		t.Fatalf("moving back with a current bundle: %v\n%s", err, out)
	}

	// 4. Garbage.
	junk := filepath.Join(t.TempDir(), "junk.pem")
	os.WriteFile(junk, []byte("-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"), 0o600)
	if _, err := run(t, "", "identity", "install", "--bundle", junk); err == nil {
		t.Fatal("installed a bundle holding a private key block")
	}
}

// The ONE intended behaviour change of moving install into pki (knomit#256):
// before it, `identity install` created <home>/pki and reset its mode to 0700
// BEFORE the same-root and CRL checks, so a refused bundle still touched the
// directory. Now a verification failure leaves it exactly as it was — here, a
// root-differs refusal on a home whose pki dir the operator set to 0750.
func TestIdentity_RefusedInstallLeavesThePKIDirAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	dir, passFile := fleetRoot(t)
	home, _, pubPath := instanceHome(t, "laptop")
	useHome(t, home)
	if _, err := run(t, "", "identity", "install", "--bundle", enroll(t, dir, passFile, pubPath), "--yes"); err != nil {
		t.Fatal(err)
	}
	pkiDir := filepath.Join(home, "pki")
	if err := os.Chmod(pkiDir, 0o750); err != nil {
		t.Fatal(err)
	}
	dir2, pass2 := fleetRoot(t)
	if _, err := run(t, "", "identity", "install", "--bundle", enroll(t, dir2, pass2, pubPath)); err == nil || !strings.Contains(err.Error(), "--replace-root") {
		t.Fatalf("switched fleets without --replace-root: %v", err)
	}
	if fi, _ := os.Stat(pkiDir); fi.Mode().Perm() != 0o750 {
		t.Fatalf("a refused install reset the pki dir to %v", fi.Mode().Perm())
	}
}

// knomit#299: a first install prints the bundle's fleet root and principal
// BEFORE installing, and installs only once that root is confirmed. Without
// a terminal to ask on (stdin is the bundle, or not a terminal) and without
// --root or --yes, it refuses, naming the fingerprint and the flags, and
// writes nothing.
func TestIdentity_FirstInstallConfirmsTheRoot(t *testing.T) {
	dir, passFile := fleetRoot(t)
	home, pub, pubPath := instanceHome(t, "laptop")
	useHome(t, home)
	bundle := enroll(t, dir, passFile, pubPath)
	rootCert, err := pki.LoadRootCert(filepath.Join(dir, pki.RootCertFile))
	if err != nil {
		t.Fatal(err)
	}
	rootFP, _ := pki.RootID(rootCert)
	principal := "instance:" + pki.Fingerprint(pub) + "@cert"
	pkiDir := filepath.Join(home, "pki")
	notInstalled := func(t *testing.T) {
		t.Helper()
		if _, err := os.Stat(pkiDir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a refused install created %s (%v)", pkiDir, err)
		}
	}

	t.Run("no terminal, no flag", func(t *testing.T) {
		out, err := run(t, "", "identity", "install", "--bundle", bundle)
		if err == nil || !strings.Contains(err.Error(), rootFP) || !strings.Contains(err.Error(), "--root") || !strings.Contains(err.Error(), "--yes") {
			t.Fatalf("installed an unconfirmed root: %v\n%s", err, out)
		}
		if !strings.Contains(out, "bundle fleet root: "+rootFP) || !strings.Contains(out, "principal: "+principal) {
			t.Fatalf("the root and principal were not shown:\n%s", out)
		}
		notInstalled(t)
	})
	t.Run("bundle on stdin", func(t *testing.T) {
		raw, _ := os.ReadFile(bundle)
		if _, err := run(t, string(raw), "identity", "install"); err == nil || !strings.Contains(err.Error(), "--root") ||
			!strings.Contains(err.Error(), "stdin is the bundle") || !strings.Contains(err.Error(), "--bundle <file>") {
			t.Fatalf("installed an unconfirmed root from stdin: %v", err)
		}
		notInstalled(t)
	})
	t.Run("--root mismatch", func(t *testing.T) {
		wrong := strings.Repeat("0", len(rootFP))
		if _, err := run(t, "", "identity", "install", "--bundle", bundle, "--root", wrong); err == nil || !strings.Contains(err.Error(), "is not the bundle's fleet root") {
			t.Fatalf("installed with a wrong --root: %v", err)
		}
		notInstalled(t)
	})
	t.Run("--root match", func(t *testing.T) {
		if out, err := run(t, "", "identity", "install", "--bundle", bundle, "--root", strings.ToUpper(rootFP)); err != nil {
			t.Fatalf("--root %s: %v\n%s", rootFP, err, out)
		}
		// A renewal under the installed root asks nothing.
		if out, err := run(t, "", "identity", "install", "--bundle", enroll(t, dir, passFile, pubPath)); err != nil {
			t.Fatalf("renewal: %v\n%s", err, out)
		}
	})
}

// The prompt: "y" (or "yes") installs; anything else refuses and writes
// nothing. --yes needs no answer. Driven through installBundle with an
// injected reader, which is what the command passes when stdin is a
// terminal and --bundle is a file.
func TestIdentity_InstallPromptsOnATerminal(t *testing.T) {
	dir, passFile := fleetRoot(t)
	for _, tc := range []struct {
		name, answer string
		yes, ok      bool
	}{
		{"y", "y\n", false, true},
		{"yes", "YES\n", false, true},
		{"n", "n\n", false, false},
		{"empty", "\n", false, false},
		{"eof", "", false, false},
		{"--yes", "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, _, pubPath := instanceHome(t, "laptop")
			useHome(t, home)
			raw, _ := os.ReadFile(enroll(t, dir, passFile, pubPath))
			cfg, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			err = installBundle(&out, cfg, raw, installOpts{yes: tc.yes, prompt: strings.NewReader(tc.answer)})
			if tc.ok != (err == nil) {
				t.Fatalf("answer %q: %v\n%s", tc.answer, err, out.String())
			}
			if !tc.yes && !strings.Contains(out.String(), "[y/N]") {
				t.Fatalf("no prompt:\n%s", out.String())
			}
			if _, serr := os.Stat(filepath.Join(home, "pki", pki.InstanceCertFile)); tc.ok != (serr == nil) {
				t.Fatalf("installed=%v, want %v", serr == nil, tc.ok)
			}
		})
	}
}

// A test's stdin, a pipe and a regular file are not terminals, so none of
// them is prompted on.
func TestIdentity_IsTerminal(t *testing.T) {
	f, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(strings.NewReader("y\n")) || isTerminal(f) {
		t.Fatal("a reader or a regular file counted as a terminal")
	}
}

// An installed root.crt that does not load is refused before the bundle's
// root is shown or asked about, naming the file.
func TestIdentity_UnreadableRootIsRefusedBeforeAsking(t *testing.T) {
	dir, passFile := fleetRoot(t)
	home, _, pubPath := instanceHome(t, "laptop")
	useHome(t, home)
	if _, err := run(t, "", "identity", "install", "--bundle", enroll(t, dir, passFile, pubPath), "--yes"); err != nil {
		t.Fatal(err)
	}
	rootFile := filepath.Join(home, "pki", pki.RootCertFile)
	if err := os.WriteFile(rootFile, []byte("garbled"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "", "identity", "install", "--bundle", enroll(t, dir, passFile, pubPath), "--yes")
	if err == nil || !strings.Contains(err.Error(), rootFile) {
		t.Fatalf("installed over an unreadable root.crt: %v\n%s", err, out)
	}
	if strings.Contains(out, "bundle fleet root:") {
		t.Fatalf("the root was shown before the refusal:\n%s", out)
	}
}

// --replace-root is the permission to move; the new root still needs the
// same confirmation as a first install.
func TestIdentity_ReplaceRootNeedsConfirmationToo(t *testing.T) {
	dir, passFile := fleetRoot(t)
	home, _, pubPath := instanceHome(t, "laptop")
	useHome(t, home)
	if _, err := run(t, "", "identity", "install", "--bundle", enroll(t, dir, passFile, pubPath), "--yes"); err != nil {
		t.Fatal(err)
	}
	dir2, pass2 := fleetRoot(t)
	foreign := enroll(t, dir2, pass2, pubPath)
	out, err := run(t, "", "identity", "install", "--bundle", foreign, "--replace-root")
	if err == nil || !strings.Contains(err.Error(), "--root") || !strings.Contains(out, "installed fleet root: ") {
		t.Fatalf("replaced the root without confirmation: %v\n%s", err, out)
	}
	root2, _ := pki.LoadRootCert(filepath.Join(dir2, pki.RootCertFile))
	fp2, _ := pki.RootID(root2)
	if out, err := run(t, "", "identity", "install", "--bundle", foreign, "--replace-root", "--root", fp2); err != nil {
		t.Fatalf("confirmed replace: %v\n%s", err, out)
	}
}

// The desktop's "Copy public key" line (pki.PublicKeyLine) is accepted by
// `enroll --pubkey` as the same key, and carries the host enroll would put in
// the SAN — so an operator given only that line needs no --host.
func TestIdentity_DesktopPublicKeyLineRoundTripsThroughEnroll(t *testing.T) {
	home, pub, _ := instanceHome(t, "laptop")
	line, err := pki.PublicKeyLine(filepath.Join(home, "id_ed25519"))
	if err != nil {
		t.Fatal(err)
	}
	got, host, err := parseInstancePubkey(line)
	if err != nil {
		t.Fatalf("enroll --pubkey refuses the desktop's line %q: %v", line, err)
	}
	want, _ := os.Hostname()
	if want == "" {
		want = "local"
	}
	if !got.Equal(pub) || host != want {
		t.Fatalf("round trip: key equal %v, host %q, want %q", got.Equal(pub), host, want)
	}
}

// With no usable keychain the root key is in the passphrase-encrypted
// root.key, and enroll needs the right passphrase to open it.
func TestIdentity_EnrollNeedsAPassphraseAndTheRightOne(t *testing.T) {
	useKeychain(t, &cliKeychain{items: map[string]string{}, setErr: errors.New("no Secret Service")})
	dir, _ := fleetRoot(t)
	if _, err := os.Stat(filepath.Join(dir, pki.RootKeyFile)); err != nil {
		t.Fatalf("init-root without a keychain wrote no root.key: %v", err)
	}
	_, _, pubPath := instanceHome(t, "laptop")
	if _, err := run(t, "", "identity", "enroll", "--dir", dir, "--pubkey", pubPath); err == nil {
		t.Fatal("enroll without --passphrase-file succeeded")
	}
	if _, err := run(t, "wrong\n", "identity", "enroll", "--dir", dir, "--pubkey", pubPath, "--passphrase-file", "-"); err == nil {
		t.Fatal("enroll with the wrong passphrase succeeded")
	}
	if out, err := run(t, "correct horse\n", "identity", "enroll", "--dir", dir, "--pubkey", pubPath, "--passphrase-file", "-"); err != nil || !strings.Contains(out, "BEGIN X509 CRL") {
		t.Fatalf("enroll with the passphrase on stdin: %v\n%s", err, out)
	}
}

func TestGrants_AddListRevokeAndTheRefusals(t *testing.T) {
	home := t.TempDir()
	useHome(t, home)
	if _, err := run(t, "", "grants", "list"); err == nil || !strings.Contains(err.Error(), "knomit serve") {
		t.Fatalf("grants on an unserved home: %v", err)
	}
	reg, err := repos.OpenRegistry(filepath.Join(home, "control.db")) // what the first `knomit serve` does
	if err != nil {
		t.Fatal(err)
	}
	reg.Close()

	fp := strings.Repeat("ab", 32)
	inst := "instance:" + fp + "@cert"
	if out, err := run(t, "", "grants", "add", inst, "write", "--by", "op"); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	out, err := run(t, "", "grants", "list", inst)
	if err != nil || !strings.Contains(out, inst+"\twrite\tlive\tby=op") {
		t.Fatalf("list after add: %v\n%s", err, out)
	}

	// revoke read on an instance: refused, by PARSING, with the explanation.
	_, err = run(t, "", "grants", "revoke", inst, "read")
	if err == nil || !strings.Contains(err.Error(), "knomit identity revoke") {
		t.Fatalf("revoke read on an instance: %v", err)
	}
	// Positive control: read on a non-instance principal is revocable.
	if _, err := run(t, "", "grants", "revoke", "operator:"+fp+"@cert", "read"); err != nil {
		t.Fatalf("revoke read on an operator: %v", err)
	}

	for _, bad := range [][]string{
		{"grants", "add", "anonymous@none", "write"},                            // anonymous comes from config
		{"grants", "add", "agent:x@cert", "write"},                              // unknown kind
		{"grants", "add", inst, "wirte"},                                        // unknown permission
		{"grants", "add", "instance@cert", "write"},                             // no id
		{"grants", "add", "instance:" + fp[:8] + "@cert", "write"},              // the 8-hex short form
		{"grants", "add", "instance:" + strings.ToUpper(fp) + "@cert", "write"}, // not lowercase
	} {
		if _, err := run(t, "", bad...); err == nil {
			t.Fatalf("%v succeeded", bad)
		}
	}

	if _, err := run(t, "", "grants", "revoke", inst, "write"); err != nil {
		t.Fatal(err)
	}
	out, _ = run(t, "", "grants", "list")
	if !strings.Contains(out, inst+"\twrite\trevoked ") {
		t.Fatalf("list after revoke:\n%s", out)
	}
}

func readFirstCert(t *testing.T, bundlePath string) []byte {
	t.Helper()
	raw, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func parseCert(t *testing.T, raw []byte) *x509.Certificate {
	t.Helper()
	blk, _ := pem.Decode(raw)
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The --root form (rehearsal finding F3; the user's ruling: one form,
// documented). --root takes the 64-hex SHA-256 of the root key's SSH wire
// encoding; the expected value here is computed INDEPENDENTLY of pki.RootID
// (ssh.NewPublicKey + sha256), and the openssl values from the certificate's
// DER exactly as `openssl x509 -noout -fingerprint -sha256` prints them
// (OpenSSL 3 lowercase `sha256 Fingerprint=`, LibreSSL `SHA256 Fingerprint=`).
// Every other value — the openssl forms, truncated or SHA-1 values, junk — is
// refused naming the form and where to get it, and installs nothing.
// init-root prints the value and show prints the installed root.
// Sabotage: compare --root without the form check (red: the malformed values
// get "is not the bundle's fleet root" instead of the form), or drop the
// init-root line (red).
func TestIdentity_RootFingerprintForm(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "fleet")
	passFile := filepath.Join(base, "pass")
	if err := os.WriteFile(passFile, []byte("correct horse\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	initOut, err := run(t, "", "identity", "init-root", "--dir", dir, "--cn", "knomit-fleet-root-test", "--passphrase-file", passFile)
	if err != nil {
		t.Fatalf("init-root: %v\n%s", err, initOut)
	}
	rootCert, err := pki.LoadRootCert(filepath.Join(dir, pki.RootCertFile))
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(rootCert.PublicKey.(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(sshPub.Marshal())
	rootFP := hex.EncodeToString(sum[:])
	if !strings.Contains(initOut, "# fleet root fingerprint: "+rootFP+" ") {
		t.Fatalf("init-root does not print the root fingerprint %s:\n%s", rootFP, initOut)
	}

	certSum := sha256.Sum256(rootCert.Raw)
	pairs := make([]string, len(certSum))
	for i, b := range certSum {
		pairs[i] = strings.ToUpper(hex.EncodeToString([]byte{b}))
	}
	opensslColon := strings.Join(pairs, ":")
	sha1Pairs := strings.Join(pairs[:20], ":")

	home, _, pubPath := instanceHome(t, "laptop")
	useHome(t, home)
	bundle := enroll(t, dir, passFile, pubPath)
	pkiDir := filepath.Join(home, "pki")

	malformed := map[string]string{
		"openssl 3":           "sha256 Fingerprint=" + opensslColon,
		"libressl":            "SHA256 Fingerprint=" + opensslColon,
		"openssl bare colons": opensslColon,
		"sha1 openssl":        "SHA1 Fingerprint=" + sha1Pairs,
		"truncated 8":         rootFP[:8],
		"truncated 63":        rootFP[:63],
		"sha1 length":         rootFP[:40],
		"too long":            rootFP + "00",
		"junk":                "not-a-fingerprint",
		"non-hex":             strings.Repeat("g", 64),
	}
	for name, v := range malformed {
		_, err := run(t, "", "identity", "install", "--bundle", bundle, "--root", v)
		if err == nil || !strings.Contains(err.Error(), "is not a fleet root fingerprint") ||
			!strings.Contains(err.Error(), "64 hex characters") || !strings.Contains(err.Error(), "init-root") {
			t.Fatalf("%s: --root %q not refused for its form: %v", name, v, err)
		}
		if strings.Contains(v, ":") && !strings.Contains(err.Error(), "hashes the root CERTIFICATE") {
			t.Fatalf("%s: the openssl hint is missing: %v", name, err)
		}
		if _, statErr := os.Stat(pkiDir); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("%s: a refused install created %s", name, pkiDir)
		}
	}

	// The one form: surrounding space and either case.
	if out, err := run(t, "", "identity", "install", "--bundle", bundle, "--root", "  "+strings.ToUpper(rootFP)+"\n"); err != nil {
		t.Fatalf("--root %s: %v\n%s", rootFP, err, out)
	}
	show, err := run(t, "", "identity", "show")
	if err != nil || !strings.Contains(show, "root: "+rootFP+` ("knomit-fleet-root-test")`) {
		t.Fatalf("show does not print the installed root: %v\n%s", err, show)
	}
}

// show prints the agent id the instance persisted in control.db — the one
// app.New resolves and authors commits as — not anything built from the key.
// The stored id here deliberately shares nothing with the key's fingerprint,
// so a show that re-derived <host>-<fp8> would print a different id.
func TestIdentityShow_PrintsPersistedAgentID(t *testing.T) {
	home, pub, _ := instanceHome(t, "laptop")
	const stored = "opaque-agent-0001"
	if _, err := repos.ResolveIdentity(home, repos.Identity{AgentID: stored, Branch: "agent/" + stored, Host: "laptop"}); err != nil {
		t.Fatal(err)
	}
	useHome(t, home)
	out, err := run(t, "", "identity", "show")
	if err != nil {
		t.Fatalf("show: %v\n%s", err, out)
	}
	if !strings.Contains(out, "agent_id: "+stored+"\n") {
		t.Fatalf("show does not print the persisted agent id %q:\n%s", stored, out)
	}
	sshPub, _ := ssh.NewPublicKey(pub)
	h := sha256.Sum256(sshPub.Marshal())
	if fp8 := hex.EncodeToString(h[:])[:8]; strings.Contains(out, "agent_id: laptop-"+fp8) {
		t.Fatalf("show printed an id derived from the key:\n%s", out)
	}
}

// With no control.db yet (the instance never served), show says there is no
// agent id and does NOT create control.db as a side effect.
func TestIdentityShow_NoAgentIDBeforeFirstBoot(t *testing.T) {
	home, _, _ := instanceHome(t, "laptop")
	useHome(t, home)
	out, err := run(t, "", "identity", "show")
	if err != nil {
		t.Fatalf("show: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home, "control.db")); !os.IsNotExist(err) {
		t.Fatalf("show created control.db (stat err %v)", err)
	}
	if !strings.Contains(out, "agent_id: none") {
		t.Fatalf("show without control.db:\n%s", out)
	}
}

// seedControlDB writes home/control.db holding exactly the given DDL/DML and
// nothing else — no repos table, no schema_migrations — so a test can tell
// afterwards whether anything migrated it.
func seedControlDB(t *testing.T, home string, stmts ...string) string {
	t.Helper()
	path := filepath.Join(home, "control.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}
	return path
}

// controlTables lists the tables in the control.db at path.
func controlTables(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

const seedInstanceIdentityTable = `CREATE TABLE instance_identity (
	id INTEGER PRIMARY KEY CHECK (id = 1), agent_id TEXT NOT NULL, branch TEXT NOT NULL,
	created_at INTEGER NOT NULL)`

// show must read the agent id WITHOUT migrating control.db: a newer CLI must
// not run controlUp (lens re-key, migrate.Control) behind the back of an
// instance that last booted, or is running, on an older binary. The seeded
// file holds only instance_identity; after show it must hold only that still.
func TestIdentityShow_DoesNotMigrateControlDB(t *testing.T) {
	home, _, _ := instanceHome(t, "laptop")
	const stored = "opaque-agent-unmigrated"
	path := seedControlDB(t, home, seedInstanceIdentityTable,
		`INSERT INTO instance_identity (id, agent_id, branch, created_at) VALUES (1, '`+stored+`', 'agent/`+stored+`', 1)`)
	useHome(t, home)
	out, err := run(t, "", "identity", "show")
	if err != nil {
		t.Fatalf("show: %v\n%s", err, out)
	}
	if !strings.Contains(out, "agent_id: "+stored+"\n") {
		t.Fatalf("show does not print the stored agent id %q:\n%s", stored, out)
	}
	if got := strings.Join(controlTables(t, path), ","); got != "instance_identity" {
		t.Fatalf("show changed control.db's tables to [%s], want only [instance_identity] (no repos, no schema_migrations)", got)
	}
}

// A home last booted by a binary from before migration 000012 has a
// control.db with no instance_identity table: show says there is no id yet,
// rather than "unreadable: no such table", and creates no table.
func TestIdentityShow_NoInstanceIdentityTable(t *testing.T) {
	home, _, _ := instanceHome(t, "laptop")
	path := seedControlDB(t, home, `CREATE TABLE pre_000012 (x INTEGER)`)
	useHome(t, home)
	out, err := run(t, "", "identity", "show")
	if err != nil {
		t.Fatalf("show: %v\n%s", err, out)
	}
	if !strings.HasPrefix(out, "agent_id: none (assigned when the instance first starts)\nkey: ") {
		t.Fatalf("show with no instance_identity table:\n%s", out)
	}
	if got := strings.Join(controlTables(t, path), ","); got != "pre_000012" {
		t.Fatalf("show changed control.db's tables to [%s]", got)
	}
}

// show reads through the read-only opener, not merely a non-migrating one.
// A read-write open (OpenRegistryNoSchema) of a rollback-journal control.db
// persistently switches it to WAL — header bytes 18-19 go from 1,1 to 2,2 —
// so a show that still reports 1,1 afterwards never opened it for writing.
func TestIdentityShow_OpensControlDBReadOnly(t *testing.T) {
	home, _, _ := instanceHome(t, "laptop")
	const stored = "opaque-agent-readonly"
	path := seedControlDB(t, home, seedInstanceIdentityTable,
		`INSERT INTO instance_identity (id, agent_id, branch, created_at) VALUES (1, '`+stored+`', 'agent/`+stored+`', 1)`)
	header := func() [2]byte {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) < 20 {
			t.Fatalf("control.db is %d bytes, no SQLite header", len(b))
		}
		return [2]byte{b[18], b[19]}
	}
	if got := header(); got != [2]byte{1, 1} {
		t.Fatalf("seeded control.db header bytes 18-19 = %v, want [1 1] (rollback journal)", got)
	}
	useHome(t, home)
	out, err := run(t, "", "identity", "show")
	if err != nil {
		t.Fatalf("show: %v\n%s", err, out)
	}
	if !strings.Contains(out, "agent_id: "+stored+"\n") {
		t.Fatalf("show does not print the stored agent id %q:\n%s", stored, out)
	}
	if got := header(); got != [2]byte{1, 1} {
		t.Fatalf("show changed control.db header bytes 18-19 to %v, want [1 1]: it opened control.db read-write", got)
	}
}

// instance_identity exists but holds no row (a boot that failed between the
// migration and the identity insert): show says there is no id yet.
func TestIdentityShow_NoInstanceIdentityRow(t *testing.T) {
	home, _, _ := instanceHome(t, "laptop")
	seedControlDB(t, home, seedInstanceIdentityTable)
	useHome(t, home)
	out, err := run(t, "", "identity", "show")
	if err != nil {
		t.Fatalf("show: %v\n%s", err, out)
	}
	if !strings.HasPrefix(out, "agent_id: none (assigned when the instance first starts)\nkey: ") {
		t.Fatalf("show with an empty instance_identity:\n%s", out)
	}
}
