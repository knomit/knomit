package cmd

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"knomit/internal/oauth"
	"knomit/internal/pki"
)

// `knomit oauth approve|deny --sign` runs on the OPERATOR's machine (F19
// phase 3b, Task 3, ruling W2): it fetches the waiting request's public
// description, prints it for the operator to read, checks the named
// instance against the master's own issuance and revocation logs, and signs
// with the master key. `approve --signed` delivers the JSON from anywhere.

// runSplit runs the real root command and keeps stdout (the JSON) apart from
// stderr (what the operator reads).
func runSplit(t *testing.T, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := RootCmd()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err = root.Execute()
	return out.String(), errOut.String(), err
}

const signID = "YsKa_Y0SRYJwz2giIdaCrw2A6XpDaX2V0M8JqSFZN3M"

// fakeIssuer serves one waiting request's description and records what is
// POSTed to /oauth/approve.
type fakeIssuer struct {
	srv      *httptest.Server
	desc     oauth.Description
	mu       sync.Mutex
	approved []byte
}

func newFakeIssuer(t *testing.T, clientName, redirect string) *fakeIssuer {
	t.Helper()
	f := &fakeIssuer{}
	p := oauth.Pending{ID: signID, ClientID: "https://claude.ai/oauth/claude-code-client-metadata", ClientName: clientName,
		RedirectURI: redirect, Resource: "http://localhost:19491/api/v1/repos/kb/mcp",
		Scopes: []string{"read", "write", "push:own", "merge:main", "operator"}}
	f.desc = oauth.Description{ID: p.ID, ClientID: p.ClientID, ClientName: p.ClientName, RedirectURI: p.RedirectURI,
		Resource: p.Resource, Scopes: p.Scopes, ExpiresAt: time.Now().Add(9 * time.Minute).UTC(), Digest: oauth.RequestDigest(p)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/oauth/pending/"+signID:
			_ = json.NewEncoder(w).Encode(f.desc)
		case r.Method == http.MethodPost && r.URL.Path == "/oauth/approve":
			b, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.approved = b
			f.mu.Unlock()
			_, _ = w.Write([]byte(`{"id":"` + signID + `","decision":"approved"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// signSetup: a master, an instance enrolled by it, and the instance's
// fingerprint.
func signSetup(t *testing.T) (dir, passFile, fp string) {
	t.Helper()
	dir, passFile = master(t)
	_, pub, pubPath := instanceHome(t, "headless")
	enroll(t, dir, passFile, pubPath)
	return dir, passFile, pki.Fingerprint(pub)
}

func rootPub(t *testing.T, dir string) ed25519.PublicKey {
	t.Helper()
	c, err := pki.LoadRootCert(filepath.Join(dir, pki.RootCertFile))
	if err != nil {
		t.Fatal(err)
	}
	return c.PublicKey.(ed25519.PublicKey)
}

func TestOAuthSign_PrintsThenSignsWhatItPrinted(t *testing.T) {
	dir, passFile, fp := signSetup(t)
	fi := newFakeIssuer(t, "Claude Code", "http://localhost:52346/callback")
	stdout, stderr, err := runSplit(t, "", "oauth", "approve", signID, "--sign", fi.srv.URL, "--dir", dir,
		"--instance", fp, "--as", "laptop", "--passphrase-file", passFile, "--yes")
	if err != nil {
		t.Fatalf("sign: %v\n%s", err, stderr)
	}
	// The redirect host comes first: it is where the code will go.
	if i, j := strings.Index(stderr, `"localhost:52346"`), strings.Index(stderr, `"Claude Code"`); i < 0 || j < 0 || i > j {
		t.Fatalf("printout does not lead with the redirect host:\n%s", stderr)
	}
	for _, want := range []string{fi.desc.ClientID, fi.desc.Resource, "read write push:own merge:main operator", `"laptop"`, `"read write"`, "headless"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("printout lacks %q:\n%s", want, stderr)
		}
	}
	var ss oauth.SignedStatement
	if err := json.Unmarshal([]byte(stdout), &ss); err != nil {
		t.Fatalf("stdout is not the signed JSON: %v\n%s", err, stdout)
	}
	if ss.Verb != oauth.VerbApprove || ss.Instance != fp || ss.ID != signID || ss.Digest != fi.desc.Digest ||
		ss.Subject != "laptop" || strings.Join(ss.Scopes, ",") != "read,write" || ss.Issuer != fi.srv.URL {
		t.Fatalf("statement = %+v", ss)
	}
	if left := time.Until(time.Unix(ss.Expires, 0)); left < 9*time.Minute || left > 10*time.Minute+time.Second {
		t.Fatalf("expires in %s, want about 10 min", left)
	}
	b, err := ss.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	sig, _ := base64.StdEncoding.DecodeString(ss.Signature)
	if !ed25519.Verify(rootPub(t, dir), b, sig) {
		t.Fatal("the signature does not verify against the master's root")
	}
}

// Correction 2: the printout uses the same escaping as `knomit oauth
// pending`, so a hostile client_name cannot hide or reorder what the
// operator reads.
func TestOAuthSign_PrintoutIsEscaped(t *testing.T) {
	dir, passFile, fp := signSetup(t)
	hostile := "Claude Code\x1b[8m hidden\u202Eedoc"
	fi := newFakeIssuer(t, hostile, "https://evil.example/cb")
	_, stderr, err := runSplit(t, "", "oauth", "approve", signID, "--sign", fi.srv.URL, "--dir", dir,
		"--instance", fp, "--as", "laptop", "--passphrase-file", passFile, "--yes")
	if err != nil {
		t.Fatalf("sign: %v\n%s", err, stderr)
	}
	if strings.ContainsRune(stderr, 0x1b) || strings.ContainsRune(stderr, '\u202e') {
		t.Fatalf("raw control or bidi bytes reached the terminal:\n%q", stderr)
	}
	if !strings.Contains(stderr, `"Claude Code\x1b[8m hidden\u202eedoc"`) {
		t.Fatalf("hostile name not printed %%q-escaped:\n%s", stderr)
	}
	if !strings.Contains(stderr, `"evil.example"`) {
		t.Fatalf("redirect host missing:\n%s", stderr)
	}
}

// W2: the instance fingerprint comes from the operator, and must be one this
// master issued and has not revoked — never from the public description.
func TestOAuthSign_InstanceMustBeIssuedAndNotRevoked(t *testing.T) {
	dir, passFile, fp := signSetup(t)
	fi := newFakeIssuer(t, "Claude Code", "http://localhost:52346/callback")
	sign := func(instance string) (string, error) {
		_, stderr, err := runSplit(t, "", "oauth", "approve", signID, "--sign", fi.srv.URL, "--dir", dir,
			"--instance", instance, "--as", "laptop", "--passphrase-file", passFile, "--yes")
		return stderr, err
	}
	if _, err := sign(strings.Repeat("a", 64)); err == nil || !strings.Contains(err.Error(), "never issued") {
		t.Fatalf("unknown instance: %v", err)
	}
	if out, err := run(t, "", "identity", "revoke", "--dir", dir, "--fingerprint", fp, "--passphrase-file", passFile); err != nil {
		t.Fatalf("revoke: %v\n%s", err, out)
	}
	if _, err := sign(fp); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked instance: %v", err)
	}
}

// The served digest must be the digest of the served fields; a description
// that disagrees with itself is refused rather than signed.
func TestOAuthSign_RefusesAnInconsistentDescription(t *testing.T) {
	dir, passFile, fp := signSetup(t)
	fi := newFakeIssuer(t, "Claude Code", "http://localhost:52346/callback")
	fi.desc.RedirectURI = "https://evil.example/cb" // digest still covers the localhost one
	_, _, err := runSplit(t, "", "oauth", "approve", signID, "--sign", fi.srv.URL, "--dir", dir,
		"--instance", fp, "--as", "laptop", "--passphrase-file", passFile, "--yes")
	if err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("inconsistent description: %v", err)
	}
}

// Reading the passphrase on stdin AFTER the printout is the confirmation;
// a passphrase from a file signs unattended, so it needs --yes.
func TestOAuthSign_UnattendedNeedsYes(t *testing.T) {
	dir, passFile, fp := signSetup(t)
	fi := newFakeIssuer(t, "Claude Code", "http://localhost:52346/callback")
	_, _, err := runSplit(t, "", "oauth", "approve", signID, "--sign", fi.srv.URL, "--dir", dir,
		"--instance", fp, "--as", "laptop", "--passphrase-file", passFile)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("file passphrase without --yes: %v", err)
	}
	pass, _ := os.ReadFile(passFile)
	stdout, stderr, err := runSplit(t, string(pass), "oauth", "approve", signID, "--sign", fi.srv.URL, "--dir", dir,
		"--instance", fp, "--as", "laptop", "--passphrase-file", "-")
	if err != nil || !strings.Contains(stdout, `"signature"`) || !strings.Contains(stderr, "passphrase") {
		t.Fatalf("stdin passphrase: %v\nstdout %s\nstderr %s", err, stdout, stderr)
	}
}

func TestOAuthSign_Deny(t *testing.T) {
	dir, passFile, fp := signSetup(t)
	fi := newFakeIssuer(t, "Claude Code", "http://localhost:52346/callback")
	stdout, stderr, err := runSplit(t, "", "oauth", "deny", signID, "--sign", fi.srv.URL, "--dir", dir,
		"--instance", fp, "--passphrase-file", passFile, "--yes")
	if err != nil {
		t.Fatalf("deny --sign: %v\n%s", err, stderr)
	}
	var ss oauth.SignedStatement
	if err := json.Unmarshal([]byte(stdout), &ss); err != nil || ss.Verb != oauth.VerbDeny || ss.Subject != "" {
		t.Fatalf("deny statement %+v: %v", ss, err)
	}
}

// --signed delivers the JSON as it is, to the issuer it names (or --issuer),
// from any machine.
func TestOAuthSigned_Delivers(t *testing.T) {
	dir, passFile, fp := signSetup(t)
	fi := newFakeIssuer(t, "Claude Code", "http://localhost:52346/callback")
	blob, _, err := runSplit(t, "", "oauth", "approve", signID, "--sign", fi.srv.URL, "--dir", dir,
		"--instance", fp, "--as", "laptop", "--passphrase-file", passFile, "--yes")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "approval.json")
	if err := os.WriteFile(file, []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err := runSplit(t, "", "oauth", "approve", "--signed", file)
	if err != nil || !strings.Contains(out, "approved") {
		t.Fatalf("--signed: %v %s", err, out)
	}
	fi.mu.Lock()
	got := fi.approved
	fi.mu.Unlock()
	var a, b oauth.SignedStatement
	_ = json.Unmarshal(got, &a)
	_ = json.Unmarshal([]byte(blob), &b)
	if a.Signature == "" || a.Signature != b.Signature || a.Digest != b.Digest {
		t.Fatalf("delivered %s, want the signed blob", got)
	}
	// From stdin, too.
	if out, _, err := runSplit(t, blob, "oauth", "approve", "--signed", "-"); err != nil || !strings.Contains(out, "approved") {
		t.Fatalf("--signed -: %v %s", err, out)
	}
}
