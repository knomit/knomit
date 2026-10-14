package cmd

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"knomit/internal/fact"
	"knomit/internal/pki"
	"knomit/internal/store"
)

// `knomit verify ci` over two plain checkouts — a knowledge-base checkout
// (here a clone of the fleet, where members self-register) and a fleet
// checkout holding the member records and the certificate artifacts — gives
// the verdicts the in-process receive-pack test pins
// (store.TestReceivePack_RulesABMatchCheckRange): joiner admitted (rule A),
// gone revoked (rule B), stranger no-record. The commits are made and signed
// with stock git, as an operator's CI checkout has them. The text output and
// --json both say so, and --json is exactly store.CheckRange's verdict.
//
// SABOTAGE: make CheckRange build its judge with rule A off → joiner is
// REFUSED no-record → red.
func TestVerifyCI_RulesABOverPlainCheckouts(t *testing.T) {
	for _, tool := range []string{"git", "ssh-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	tmp := t.TempDir()
	git := func(dir string, env []string, args ...string) string {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null"), env...)
		out, err := c.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
		return strings.TrimSpace(string(out))
	}
	write := func(dir, p, body string) {
		t.Helper()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, p), []byte(body), 0o644))
	}
	type agent struct {
		signer  ssh.Signer
		pub     ed25519.PublicKey
		keyPath string
	}
	newAgent := func(name string) agent {
		seed := sha256.Sum256([]byte("verify ci rules ab " + name))
		priv := ed25519.NewKeyFromSeed(seed[:])
		s, err := ssh.NewSignerFromKey(priv)
		require.NoError(t, err)
		block, err := ssh.MarshalPrivateKey(priv, name)
		require.NoError(t, err)
		path := filepath.Join(tmp, name+"_ed25519")
		require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(block), 0o600))
		require.NoError(t, os.WriteFile(path+".pub", ssh.MarshalAuthorizedKey(s.PublicKey()), 0o644))
		return agent{signer: s, pub: priv.Public().(ed25519.PublicKey), keyPath: path}
	}
	member := func(id string, a agent) string {
		return "---\nkind: pragmatic\ntype: policy\n---\n# member " + id + "\n\n" +
			fact.RenderMember(fact.Member{Agent: id, State: fact.MemberActive, Key: a.signer.PublicKey(), Host: "h"})
	}

	root, err := pki.NewRoot(filepath.Join(tmp, "root"), "fleet-test", []byte("pw"), 10*365*24*time.Hour)
	require.NoError(t, err)
	leaf := func(a agent) (string, *big.Int) {
		is, err := pki.IssueLeaf(root, a.pub, "label", pki.RoleInstance, 365*24*time.Hour, time.Now())
		require.NoError(t, err)
		n, ok := new(big.Int).SetString(is.Record.Serial, 16)
		require.True(t, ok)
		return string(is.CertPEM), n
	}
	agents := map[string]agent{"boss": newAgent("boss"), "joiner": newAgent("joiner"), "gone": newAgent("gone"), "stranger": newAgent("stranger")}
	jl, _ := leaf(agents["joiner"])
	gl, gs := leaf(agents["gone"])
	crl, err := pki.SignCRL(root, []pki.Revoked{{Serial: gs, At: time.Now()}}, big.NewInt(1), time.Now(), time.Now().Add(30*24*time.Hour))
	require.NoError(t, err)

	// The fleet checkout: main holds the records and the artifacts (D7).
	fleet := filepath.Join(tmp, "fleet")
	require.NoError(t, os.MkdirAll(fleet, 0o755))
	git(fleet, nil, "init", "-q", "-b", "main")
	write(fleet, fact.OntologyFile, "id: fleet\nname: Fleet\nattributes:\n  verify_signatures: enforce\ntopics:\n  members:\n    description: d\n")
	write(fleet, "kb/members/boss/1.md", member("boss", agents["boss"]))
	write(fleet, "kb/members/gone/1.md", member("gone", agents["gone"]))
	write(fleet, "artifacts/fleet/root.crt", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Cert.Raw})))
	write(fleet, "artifacts/fleet/crl.pem", string(crl))
	write(fleet, "artifacts/fleet/certs/joiner.pem", jl)
	write(fleet, "artifacts/fleet/certs/gone.pem", gl)
	git(fleet, nil, "add", "-A")
	git(fleet, []string{"GIT_AUTHOR_NAME=root", "GIT_AUTHOR_EMAIL=root@example.com", "GIT_COMMITTER_NAME=root", "GIT_COMMITTER_EMAIL=root@example.com"},
		"commit", "-q", "-m", "fleet")

	// The knowledge-base checkout: a clone of the fleet, one candidate branch
	// per agent writing its own record, signed by its own key.
	kb := filepath.Join(tmp, "kb")
	git(tmp, nil, "clone", "-q", fleet, kb)
	want := map[string]string{"joiner": "", "gone": store.RuleRevoked, "stranger": store.RuleNoRecord}
	for id := range want {
		a := agents[id]
		git(kb, nil, "checkout", "-q", "-b", "cand-"+id, "origin/main")
		write(kb, "kb/members/"+id+"/1.md", member(id, a)+"\nupdated\n")
		git(kb, nil, "add", "-A")
		email := id + "+learn@agents.knomit.io"
		git(kb, []string{"GIT_AUTHOR_NAME=" + id, "GIT_AUTHOR_EMAIL=" + email, "GIT_COMMITTER_NAME=" + id, "GIT_COMMITTER_EMAIL=" + email},
			"-c", "gpg.format=ssh", "-c", "user.signingkey="+a.keyPath, "commit", "-q", "-S", "-m", "register "+id)
	}

	run := func(args ...string) (string, int) {
		c := verifyCICmd()
		var out bytes.Buffer
		c.SetOut(&out)
		c.SetArgs(args)
		err := c.Execute()
		if err == nil {
			return out.String(), 0
		}
		var coded *ExitCodeError
		require.ErrorAs(t, err, &coded)
		return out.String(), coded.ExitCode()
	}
	ran := 0
	for id, rule := range want {
		base := []string{"--dir", kb, "--upstream", "origin/main", "--candidate", "cand-" + id, "--fleet", fleet}
		text, code := run(base...)
		jsonOut, jcode := run(append(base, "--json")...)
		require.Equal(t, code, jcode, id)

		var got store.RangeVerdict
		require.NoError(t, json.Unmarshal([]byte(jsonOut), &got), jsonOut)
		direct, err := store.CheckRange(store.RangeInput{KBDir: kb, Main: "origin/main", Candidate: "cand-" + id, FleetDir: fleet, FleetRev: "HEAD"})
		require.NoError(t, err)
		require.Equal(t, direct, got, "%s: --json is CheckRange's verdict", id)
		require.Equal(t, store.VerifyEnforce, got.Mode)
		require.Equal(t, 1, got.Checked, id)

		if rule == "" {
			require.Equal(t, 0, code, "%s: %s", id, text)
			require.Empty(t, got.Refused, id)
			require.Contains(t, text, "mode enforce, 1 new commit(s), 0 refused", id)
		} else {
			require.Equal(t, 1, code, "%s: %s", id, text)
			require.Len(t, got.Refused, 1, id)
			require.Equal(t, rule, got.Refused[0].Rule, id)
			require.Contains(t, text, "REFUSED "+got.Refused[0].Commit[:8]+"  "+rule+":", id)
		}
		ran++
	}
	require.Equal(t, 3, ran)
}
