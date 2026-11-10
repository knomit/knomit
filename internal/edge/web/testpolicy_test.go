package web

import (
	"context"
	"database/sql"
	"net/http"
	"sync"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	storemigrate "knomit/internal/engine/store/migrate"
	"knomit/internal/foundation/authz"
)

// The in-process test seam of the unified authn/z model (plan P2). A test
// states WHO it acts as with asOwner / asRole / asNode / asAnonymous; the
// helper attaches that principal's authz.Policy to the request context, and
// injectedPolicy (nil in production) hands it back to whatever asks.
//
// Until the flip (P4) the old gates still decide everything and nothing reads
// injectedPolicy, so the helpers also have to leave the request looking the way
// the old gates need:
//
//   - asOwner is what fromLoopback used to be: it sets the loopback address
//     AND attaches the owner policy. P4 deletes the address half.
//   - asRole, asNode and asAnonymous attach a policy and touch NOTHING else.
//     They stand for "someone who is not the owner", and the old gates keep
//     refusing such a request only while it keeps whatever address and
//     principal it had. A refusal test that needs the loopback address (the
//     old gate refuses the anonymous LOOPBACK principal for lack of a grant)
//     writes asAnonymous(fromLoopback(r)) so the address stays explicit.
func init() {
	injectedPolicy = func(r *http.Request) (authz.Policy, bool) {
		return authz.PolicyFrom(r.Context())
	}
}

// attachPolicy puts p on r's context in place (like fromLoopback mutates r),
// so a test that keeps using the request it built still sees one request.
func attachPolicy(r *http.Request, p authz.Policy) *http.Request {
	*r = *r.WithContext(authz.WithPolicy(r.Context(), p))
	return r
}

var testOwnerPolicy = func() authz.Policy {
	p, err := authz.Owner()
	if err != nil {
		p = authz.Principal{Kind: authz.KindOS, ID: "uid:1"}
	}
	return authz.Policy{Principal: p, Via: authz.ViaNone, Owner: true}
}()

// asOwner is the implicit owner: every known permission. Before P4 it also
// puts the request on a loopback address (fromLoopback), because the old gates
// still derive their principal from the address.
func asOwner(r *http.Request) *http.Request {
	return attachPolicy(fromLoopback(r), testOwnerPolicy)
}

var (
	roleOnce sync.Once
	roleMat  *authz.Materializer
	roleErr  error
)

// materializer lazily builds a Materializer over a private in-memory
// control.db with the seeded roles, so asRole policies come from the real
// authz.Seed table and cannot drift from it.
func materializer() *authz.Materializer {
	roleOnce.Do(func() {
		var db *sql.DB
		db, roleErr = sql.Open("sqlite3", "file:web-testpolicy?mode=memory&cache=shared&_foreign_keys=on")
		if roleErr != nil {
			return
		}
		db.SetMaxOpenConns(1)
		if roleErr = storemigrate.Control(db); roleErr != nil {
			return
		}
		st := authz.NewStore(db)
		if roleErr = st.SeedRoles(context.Background()); roleErr != nil {
			return
		}
		roleMat = authz.NewMaterializer(st, authz.Principal{}, authz.MaterializerOptions{})
	})
	if roleErr != nil {
		panic("web test policy materializer: " + roleErr.Error())
	}
	return roleMat
}

// asRole is a token principal holding exactly roles. It does not touch the
// address: see the note on init.
func asRole(r *http.Request, roles ...authz.Role) *http.Request {
	p, err := materializer().For(r.Context(), authz.Principal{Kind: authz.KindToken, ID: "test"}, authz.ViaBearer, roles...)
	if err != nil {
		panic("asRole: " + err.Error())
	}
	return attachPolicy(r, p)
}

// asNode is a fleet node with the given certificate fingerprint (the node
// role is implied by the certificate, never bound).
func asNode(r *http.Request, fp string) *http.Request {
	p, err := materializer().For(r.Context(), authz.Principal{Kind: authz.KindNode, ID: fp}, authz.ViaMTLS, authz.RoleNode)
	if err != nil {
		panic("asNode: " + err.Error())
	}
	return attachPolicy(r, p)
}

// asAnonymous is a caller that presented nothing: the zero policy, which
// grants only Public. It does not touch the address: see the note on init.
func asAnonymous(r *http.Request) *http.Request {
	return attachPolicy(r, authz.Policy{Principal: authz.Anonymous(), Via: authz.ViaNone})
}

// asAnonymousOnLoopback is the refusal-test spelling of "the anonymous local
// caller": asAnonymous plus the loopback address, which the old gates need to
// see before they refuse that principal for lack of a grant. P4 deletes the
// address half (and this helper with it, in favour of asAnonymous).
func asAnonymousOnLoopback(r *http.Request) *http.Request {
	return asAnonymous(fromLoopback(r))
}

func TestTestPolicy_Helpers(t *testing.T) {
	get := func() *http.Request { return newTestReq(http.MethodGet, "/x") }

	o := asOwner(get())
	pol, ok := injectedPolicy(o)
	if !ok || !pol.Owner || !pol.Has(authz.KBWrite) {
		t.Fatalf("asOwner: %+v %v", pol, ok)
	}
	if o.RemoteAddr != "127.0.0.1:1" {
		t.Fatalf("asOwner must keep the loopback address until P4, got %q", o.RemoteAddr)
	}

	rd := asRole(get(), authz.RoleReader)
	pol, _ = injectedPolicy(rd)
	if !pol.Has(authz.KBRead) || pol.Has(authz.KBWrite) || pol.Owner {
		t.Fatalf("asRole(reader): %v", pol.Permissions())
	}
	if rd.RemoteAddr == "127.0.0.1:1" {
		t.Fatal("asRole must not set the loopback address")
	}

	nd := asNode(get(), "abc")
	pol, _ = injectedPolicy(nd)
	if !pol.Has(authz.RepoPush) || pol.Has(authz.KBWrite) {
		t.Fatalf("asNode: %v", pol.Permissions())
	}

	an := asAnonymous(get())
	pol, ok = injectedPolicy(an)
	if !ok || pol.Has(authz.KBRead) {
		t.Fatalf("asAnonymous: %v %v", pol.Permissions(), ok)
	}
}

func newTestReq(method, target string) *http.Request {
	r, _ := http.NewRequest(method, target, nil)
	r.RemoteAddr = "192.0.2.1:1234"
	r.Host = "example.com"
	return r
}
