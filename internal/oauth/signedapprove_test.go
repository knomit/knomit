package oauth

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"knomit/internal/auth"
)

// Master-key approval (F19 phase 3b, Task 3; consent path 2). The operator
// signs a canonical statement with the fleet master key on their own
// machine; the instance verifies it against the root public key it holds
// and applies the decision. Every refusal is a named error.

const ownFP = "1111111111111111111111111111111111111111111111111111111111111111"

type masterKey struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newMasterKey(t *testing.T) masterKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return masterKey{pub, priv}
}

// signedFixture is the issuer fixture with a fleet root and an instance
// fingerprint, and one parked request.
type signedFixture struct {
	*issuerFixture
	root masterKey
	id   string
	row  Pending
}

func newSignedFixture(t *testing.T) *signedFixture {
	t.Helper()
	f := &signedFixture{issuerFixture: newIssuerFixture(t), root: newMasterKey(t)}
	f.iss.instanceFP = ownFP
	f.iss.fleetRoot = func() (ed25519.PublicKey, error) { return f.root.pub, nil }
	_, ch := pkce()
	f.id = f.authorize(t, f.authorizeQuery(ch))
	var err error
	if f.row, err = f.store.GetPending(context.Background(), f.id); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *signedFixture) statement(verb string) ApprovalStatement {
	s := ApprovalStatement{Verb: verb, Instance: ownFP, ID: f.id, Digest: RequestDigest(f.row),
		Expires: f.clock.now().Add(5 * time.Minute).Unix()}
	if verb == VerbApprove {
		s.Subject, s.Scopes = "laptop", []string{"read", "write"}
	}
	return s
}

func (f *signedFixture) sign(t *testing.T, s ApprovalStatement, key ed25519.PrivateKey) SignedStatement {
	t.Helper()
	ss, err := SignStatement(s, key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return ss
}

// post delivers a signed statement to /oauth/approve and returns the status
// and body.
func (f *signedFixture) post(t *testing.T, ss SignedStatement) (int, string) {
	t.Helper()
	b, _ := json.Marshal(ss)
	resp, err := http.Post(f.srv.URL+"/oauth/approve", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	return resp.StatusCode, out.String()
}

// --- the canonical form (S1, S2) -------------------------------------------

func TestStatementBytes_Canonical(t *testing.T) {
	id := strings.Repeat("A", 43)
	dg := strings.Repeat("d", 64)
	got, err := ApprovalStatement{Verb: VerbApprove, Instance: ownFP, ID: id, Digest: dg, Subject: "laptop",
		Scopes: []string{"write", "read"}, Expires: 1790000600}.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	want := "knomit-oauth-approve/v1\n" + ownFP + "\n" + id + "\n" + dg + "\nlaptop\nread,write\n1790000600"
	if string(got) != want {
		t.Fatalf("approve bytes:\n%q\nwant\n%q", got, want)
	}
	got, err = ApprovalStatement{Verb: VerbDeny, Instance: ownFP, ID: id, Digest: dg, Expires: 1790000600}.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	want = "knomit-oauth-deny/v1\n" + ownFP + "\n" + id + "\n" + dg + "\n1790000600"
	if string(got) != want {
		t.Fatalf("deny bytes:\n%q\nwant\n%q", got, want)
	}
	// S2: the master key also signs X.509 TBS structures and CRLs, which
	// are DER and begin with 0x30. A statement never does.
	if got[0] == 0x30 || !bytes.HasPrefix(got, []byte("knomit-oauth-")) {
		t.Fatal("a statement is not domain-separated from DER")
	}
}

func TestStatementBytes_RefusesMalformed(t *testing.T) {
	ok := ApprovalStatement{Verb: VerbApprove, Instance: ownFP, ID: strings.Repeat("A", 43), Digest: strings.Repeat("d", 64),
		Subject: "laptop", Scopes: []string{"read"}, Expires: 1790000600}
	if _, err := ok.Bytes(); err != nil {
		t.Fatalf("control: %v", err)
	}
	for name, mut := range map[string]func(s *ApprovalStatement){
		"verb":                func(s *ApprovalStatement) { s.Verb = "grant" },
		"subject newline":     func(s *ApprovalStatement) { s.Subject = "laptop\nread" },
		"subject empty":       func(s *ApprovalStatement) { s.Subject = "" },
		"admin scope":         func(s *ApprovalStatement) { s.Scopes = []string{"admin"} },
		"unknown scope":       func(s *ApprovalStatement) { s.Scopes = []string{"everything"} },
		"no scopes":           func(s *ApprovalStatement) { s.Scopes = nil },
		"duplicate scope":     func(s *ApprovalStatement) { s.Scopes = []string{"read", "read"} },
		"fingerprint short":   func(s *ApprovalStatement) { s.Instance = ownFP[:63] },
		"fingerprint upper":   func(s *ApprovalStatement) { s.Instance = strings.ToUpper(strings.Repeat("a", 64)) },
		"digest not hex":      func(s *ApprovalStatement) { s.Digest = strings.Repeat("z", 64) },
		"id shape":            func(s *ApprovalStatement) { s.ID = "short" },
		"id newline":          func(s *ApprovalStatement) { s.ID = strings.Repeat("A", 42) + "\n" },
		"expires zero":        func(s *ApprovalStatement) { s.Expires = 0 },
		"expires negative":    func(s *ApprovalStatement) { s.Expires = -1 },
		"deny with a subject": func(s *ApprovalStatement) { s.Verb = VerbDeny },
	} {
		s := ok
		s.Scopes = append([]string(nil), ok.Scopes...)
		mut(&s)
		if _, err := s.Bytes(); !errors.Is(err, ErrMalformedStatement) {
			t.Errorf("%s: err = %v, want ErrMalformedStatement", name, err)
		}
	}
}

// --- applying it -----------------------------------------------------------

func TestSignedApprove_ApprovesAndTheBrowserCollects(t *testing.T) {
	f := newSignedFixture(t)
	code, body := f.post(t, f.sign(t, f.statement(VerbApprove), f.root.priv))
	if code != http.StatusOK || !strings.Contains(body, `"approved"`) {
		t.Fatalf("approve: %d %s", code, body)
	}
	p, _ := f.store.GetPending(context.Background(), f.id)
	wantBy := "master:" + FleetRootLabel(f.root.pub)
	if p.Decision != DecisionApproved || p.Subject != "laptop" || strings.Join(p.Ceiling, ",") != "read,write" || p.DecidedBy != wantBy {
		t.Fatalf("pending after signed approve: %+v", p)
	}
	rows, _ := f.grants.List(context.Background(), TokenPrincipal("laptop").String())
	if len(rows) != 2 || rows[0].GrantedBy != wantBy {
		t.Fatalf("grants: %+v; want granted_by %q", rows, wantBy)
	}
	resp := f.get(t, "/oauth/authorize/"+f.id+"/wait", nil)
	if loc, _ := url.Parse(resp.Header.Get("Location")); resp.StatusCode != http.StatusFound || loc.Query().Get("code") == "" {
		t.Fatalf("wait after signed approve: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// Path 2 follows R5 like every path: a subject granted before keeps its
// grants, and the answer says so, so the CLI can name the way to widen.
func TestSignedApprove_ReportsGrantsUnchanged(t *testing.T) {
	f := newSignedFixture(t)
	ctx := context.Background()
	if err := f.grants.Grant(ctx, TokenPrincipal("laptop"), "read", "op"); err != nil {
		t.Fatal(err)
	}
	code, body := f.post(t, f.sign(t, f.statement(VerbApprove), f.root.priv))
	if code != http.StatusOK || !strings.Contains(body, `"grants_unchanged":true`) {
		t.Fatalf("approve: %d %s", code, body)
	}
	set, _ := f.grants.For(ctx, TokenPrincipal("laptop"))
	if set.Has("write") {
		t.Fatalf("grants = %v; a signed re-approval must not add write", set)
	}
}

func TestSignedApprove_Deny(t *testing.T) {
	f := newSignedFixture(t)
	code, body := f.post(t, f.sign(t, f.statement(VerbDeny), f.root.priv))
	if code != http.StatusOK || !strings.Contains(body, `"denied"`) {
		t.Fatalf("deny: %d %s", code, body)
	}
	if p, _ := f.store.GetPending(context.Background(), f.id); p.Decision != DecisionDenied {
		t.Fatalf("decision = %q", p.Decision)
	}
}

// Each refusal is its own named error, and none of them decides anything.
func TestSignedApprove_Refusals(t *testing.T) {
	skew := signedApprovalSkew
	cases := []struct {
		name   string
		build  func(f *signedFixture, t *testing.T) SignedStatement
		status int
		err    error
	}{
		{"wrong key", func(f *signedFixture, t *testing.T) SignedStatement {
			return f.sign(t, f.statement(VerbApprove), newMasterKey(t).priv)
		}, http.StatusForbidden, ErrBadSignature},
		{"tampered subject", func(f *signedFixture, t *testing.T) SignedStatement {
			ss := f.sign(t, f.statement(VerbApprove), f.root.priv)
			ss.Subject = "attacker"
			return ss
		}, http.StatusForbidden, ErrBadSignature},
		{"tampered scopes", func(f *signedFixture, t *testing.T) SignedStatement {
			ss := f.sign(t, f.statement(VerbApprove), f.root.priv)
			ss.Scopes = []string{"merge:main", "read", "write"}
			return ss
		}, http.StatusForbidden, ErrBadSignature},
		// A deny signature relabelled as an approval: the verb is in line 1.
		{"verb switched", func(f *signedFixture, t *testing.T) SignedStatement {
			ss := f.sign(t, f.statement(VerbDeny), f.root.priv)
			ss.Verb, ss.Subject, ss.Scopes = VerbApprove, "laptop", []string{"read"}
			return ss
		}, http.StatusForbidden, ErrBadSignature},
		{"another instance", func(f *signedFixture, t *testing.T) SignedStatement {
			s := f.statement(VerbApprove)
			s.Instance = strings.Repeat("2", 64)
			return f.sign(t, s, f.root.priv)
		}, http.StatusForbidden, ErrWrongInstance},
		{"expired beyond the skew", func(f *signedFixture, t *testing.T) SignedStatement {
			s := f.statement(VerbApprove)
			s.Expires = f.clock.now().Add(-skew).Unix()
			return f.sign(t, s, f.root.priv)
		}, http.StatusForbidden, ErrStatementExpired},
		{"expires too far ahead", func(f *signedFixture, t *testing.T) SignedStatement {
			s := f.statement(VerbApprove)
			s.Expires = f.clock.now().Add(maxStatementLifetime + skew + time.Second).Unix()
			return f.sign(t, s, f.root.priv)
		}, http.StatusForbidden, ErrStatementExpired},
		// R4: the operator signed a description a relay made up.
		{"digest of another request", func(f *signedFixture, t *testing.T) SignedStatement {
			s := f.statement(VerbApprove)
			other := f.row
			other.RedirectURI = "https://evil.example/cb"
			s.Digest = RequestDigest(other)
			return f.sign(t, s, f.root.priv)
		}, http.StatusConflict, ErrStatementMismatch},
		{"unknown request", func(f *signedFixture, t *testing.T) SignedStatement {
			s := f.statement(VerbApprove)
			s.ID = strings.Repeat("Z", 43)
			return f.sign(t, s, f.root.priv)
		}, http.StatusNotFound, ErrUnknownPending},
		{"malformed", func(f *signedFixture, t *testing.T) SignedStatement {
			ss := f.sign(t, f.statement(VerbApprove), f.root.priv)
			ss.Subject = "two\nlines"
			return ss
		}, http.StatusBadRequest, ErrMalformedStatement},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newSignedFixture(t)
			code, body := f.post(t, c.build(f, t))
			if code != c.status || !strings.Contains(body, c.err.Error()) {
				t.Fatalf("%d %s; want %d naming %q", code, body, c.status, c.err)
			}
			if p, _ := f.store.GetPending(context.Background(), f.id); p.Decision != "" {
				t.Fatalf("a refused statement decided the request: %+v", p)
			}
		})
	}
}

// S4: the window is now − skew < expires ≤ now + maxStatementLifetime + skew,
// so an operator's clock a few minutes off either way still works.
func TestSignedApprove_SkewWindowEdges(t *testing.T) {
	skew := signedApprovalSkew
	for name, c := range map[string]struct {
		off time.Duration
		ok  bool
	}{
		"just inside the past edge":    {-skew + time.Second, true},
		"on the past edge":             {-skew, false},
		"on the future edge":           {maxStatementLifetime + skew, true},
		"just past the future edge":    {maxStatementLifetime + skew + time.Second, false},
		"an operator clock 4 min slow": {5*time.Minute - 4*time.Minute, true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSignedFixture(t)
			s := f.statement(VerbDeny)
			s.Expires = f.clock.now().Add(c.off).Unix()
			code, body := f.post(t, f.sign(t, s, f.root.priv))
			if (code == http.StatusOK) != c.ok {
				t.Fatalf("%d %s; want ok=%v", code, body, c.ok)
			}
		})
	}
}

// S3: a replayed statement is refused by the table while it is in memory,
// and by the pending row (already decided) after a restart empties it.
func TestSignedApprove_Replay(t *testing.T) {
	f := newSignedFixture(t)
	ss := f.sign(t, f.statement(VerbApprove), f.root.priv)
	if code, body := f.post(t, ss); code != http.StatusOK {
		t.Fatalf("first: %d %s", code, body)
	}
	if code, body := f.post(t, ss); code != http.StatusConflict || !strings.Contains(body, ErrReplayed.Error()) {
		t.Fatalf("replay: %d %s; want 409 %q", code, body, ErrReplayed)
	}
	// A restart: the table is the only in-memory state, and it starts
	// empty; the pending row in control.db survives.
	f.iss.replays = newReplayTable(replayTableMax)
	if code, body := f.post(t, ss); code != http.StatusConflict || !strings.Contains(body, ErrNotPending.Error()) {
		t.Fatalf("replay after restart: %d %s; want 409 %q", code, body, ErrNotPending)
	}
}

// S3: the table is pruned at expiry and bounded; inserting needs a valid
// signature, so the bound is a belt, not an unauthenticated-growth defence.
func TestReplayTable_PrunesAndBounds(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	rt := newReplayTable(3)
	for i := 0; i < 3; i++ {
		if err := rt.add(fmt.Sprint(i), now.Add(time.Minute), now); err != nil {
			t.Fatal(err)
		}
	}
	if err := rt.add("3", now.Add(time.Minute), now); !errors.Is(err, ErrReplayTableFull) {
		t.Fatalf("over the bound: %v", err)
	}
	if err := rt.add("1", now.Add(time.Minute), now); !errors.Is(err, ErrReplayed) {
		t.Fatalf("seen: %v", err)
	}
	later := now.Add(2 * time.Minute)
	if err := rt.add("3", later.Add(time.Minute), later); err != nil {
		t.Fatalf("after the others expired: %v", err)
	}
	if n := rt.len(); n != 1 {
		t.Fatalf("table holds %d after pruning, want 1", n)
	}
}

// S5: an instance with no fleet root (never enrolled) answers a named error,
// never a 500.
func TestSignedApprove_NoFleetRoot(t *testing.T) {
	f := newSignedFixture(t)
	f.iss.fleetRoot = func() (ed25519.PublicKey, error) { return nil, fs.ErrNotExist }
	code, body := f.post(t, f.sign(t, f.statement(VerbApprove), f.root.priv))
	if code != http.StatusServiceUnavailable || !strings.Contains(body, ErrNoFleetRoot.Error()) {
		t.Fatalf("%d %s; want 503 %q", code, body, ErrNoFleetRoot)
	}
	f.iss.fleetRoot = nil // not wired at all (no pki dir configured)
	if code, body := f.post(t, f.sign(t, f.statement(VerbApprove), f.root.priv)); code != http.StatusServiceUnavailable {
		t.Fatalf("unwired: %d %s", code, body)
	}
}

func TestSignedApprove_BodyBounded(t *testing.T) {
	f := newSignedFixture(t)
	resp, err := http.Post(f.srv.URL+"/oauth/approve", "application/json", strings.NewReader(`{"subject":"`+strings.Repeat("a", 64<<10)+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversize body: %d", resp.StatusCode)
	}
}

// The approving principal's spelling is not an auth.Principal a request
// could ever present, so it can never match a grants row by accident.
func TestFleetRootLabel(t *testing.T) {
	l := FleetRootLabel(newMasterKey(t).pub)
	if len(l) != 8 || strings.Trim(l, "0123456789abcdef") != "" {
		t.Fatalf("label %q", l)
	}
	if _, err := auth.ParsePrincipal("master:" + l); err == nil {
		t.Fatal("master:<fp8> parses as a principal")
	}
}
