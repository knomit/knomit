package oauth

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"knomit/internal/foundation/authz"
)

const day = 24 * time.Hour

// newCredStore is a store with the design §7 lifetimes, the authz bindings
// store seeded, and a settable clock.
func newCredStore(t *testing.T) (*Store, *authz.Store, *clock) {
	t.Helper()
	db := openControlDB(t)
	bindings := authz.NewStore(db)
	if err := bindings.SeedRoles(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Unix(1_790_000_000, 0)}
	s := NewStoreWith(db, DefaultLifetimes(), bindings)
	s.now = c.now
	return s, bindings, c
}

func owner(t *testing.T) authz.Principal {
	t.Helper()
	p, err := authz.Owner()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultLifetimes_MatchDesign(t *testing.T) {
	l := DefaultLifetimes()
	if l.Pending != 10*time.Minute || l.Link != 2*time.Minute || l.SessionIdle != 30*day || l.SessionAbsolute != 90*day ||
		l.Access != 2*time.Hour || l.RefreshIdle != 30*day || l.RefreshAbsolute != 90*day || l.PATDefault != 90*day || l.PATMax != 365*day {
		t.Fatalf("DefaultLifetimes = %+v", l)
	}
}

func TestSession_IdleAndAbsoluteExpiry(t *testing.T) {
	ctx := context.Background()
	s, _, c := newCredStore(t)
	p := owner(t)

	cookie, sess, err := s.CreateSession(ctx, p, "Firefox")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Principal != p || sess.UserAgent != "Firefox" || !sess.AbsoluteExpiry.Equal(c.now().Add(90*day)) {
		t.Fatalf("session = %+v", sess)
	}
	c.add(29 * day)
	got, err := s.LookupSession(ctx, cookie)
	if err != nil || got.Principal != p || got.ID != sess.ID {
		t.Fatalf("+29 d: LookupSession = %+v, %v; want the session", got, err)
	}

	// Idle: 31 days with no use since the lookup above.
	c.add(31 * day)
	if _, err := s.LookupSession(ctx, cookie); !errors.Is(err, ErrInvalid) {
		t.Fatalf("+31 d idle: err = %v, want ErrInvalid", err)
	}

	// Absolute: touched every 20 days, it still ends at 90 days.
	cookie2, _, err := s.CreateSession(ctx, p, "")
	if err != nil {
		t.Fatal(err)
	}
	for elapsed := 20 * day; elapsed < 90*day; elapsed += 20 * day {
		c.add(20 * day)
		if _, err := s.LookupSession(ctx, cookie2); err != nil {
			t.Fatalf("touched at +%v: %v", elapsed, err)
		}
	}
	c.add(11 * day) // +91 d, last touch 11 d ago
	if _, err := s.LookupSession(ctx, cookie2); !errors.Is(err, ErrInvalid) {
		t.Fatalf("+91 d: err = %v, want ErrInvalid (absolute)", err)
	}
}

func TestSession_UnknownAndRevokedAreInvalid(t *testing.T) {
	ctx := context.Background()
	s, _, _ := newCredStore(t)
	if _, err := s.LookupSession(ctx, "nope"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown cookie: %v", err)
	}
	if _, err := s.LookupSession(ctx, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty cookie: %v", err)
	}
	cookie, sess, err := s.CreateSession(ctx, owner(t), "")
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.ListSessions(ctx)
	if err != nil || len(list) != 1 || list[0].ID != sess.ID {
		t.Fatalf("ListSessions = %+v, %v", list, err)
	}
	if err := s.RevokeSession(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LookupSession(ctx, cookie); !errors.Is(err, ErrInvalid) {
		t.Fatalf("revoked: %v", err)
	}
	if list, _ := s.ListSessions(ctx); len(list) != 0 {
		t.Fatalf("a revoked session is still listed: %+v", list)
	}
	if err := s.RevokeSession(ctx, sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second revoke: %v, want ErrNotFound", err)
	}
}

func TestSession_RefusesNonCanonicalPrincipal(t *testing.T) {
	s, _, _ := newCredStore(t)
	for _, p := range []authz.Principal{{}, {Kind: authz.KindOS, ID: "uid:007"}, {Kind: authz.KindNode, ID: "fp"}} {
		if _, _, err := s.CreateSession(context.Background(), p, ""); err == nil {
			t.Errorf("CreateSession(%+v) accepted", p)
		}
	}
}

func TestSession_TouchAtMostOncePerMinute(t *testing.T) {
	ctx := context.Background()
	s, _, c := newCredStore(t)
	cookie, sess, err := s.CreateSession(ctx, owner(t), "")
	if err != nil {
		t.Fatal(err)
	}
	c.add(30 * time.Second)
	got, _ := s.LookupSession(ctx, cookie)
	if !got.LastSeen.Equal(sess.LastSeen) {
		t.Fatalf("touched within a minute: %v -> %v", sess.LastSeen, got.LastSeen)
	}
	c.add(31 * time.Second)
	got, _ = s.LookupSession(ctx, cookie)
	if !got.LastSeen.Equal(c.now()) {
		t.Fatalf("not touched after a minute: %v, now %v", got.LastSeen, c.now())
	}
}

func TestLink_SingleUseAndTwoMinutes(t *testing.T) {
	ctx := context.Background()
	s, _, c := newCredStore(t)
	p := owner(t)

	tok, exp, err := s.MintLink(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if !exp.Equal(c.now().Add(2 * time.Minute)) {
		t.Fatalf("expires = %v, want now+2m", exp)
	}

	// Two concurrent consumers: exactly one wins.
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		wins     int
		failures []error
	)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.ConsumeLink(ctx, tok)
			mu.Lock()
			defer mu.Unlock()
			if err == nil && got == p {
				wins++
			} else {
				failures = append(failures, err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 || len(failures) != 1 || !errors.Is(failures[0], ErrInvalid) {
		t.Fatalf("wins = %d, failures = %v; want exactly one win and one ErrInvalid", wins, failures)
	}

	tok2, _, err := s.MintLink(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	c.add(2 * time.Minute)
	if _, err := s.ConsumeLink(ctx, tok2); !errors.Is(err, ErrInvalid) {
		t.Fatalf("at 2 minutes: %v, want ErrInvalid", err)
	}
	if _, err := s.ConsumeLink(ctx, "garbage"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown link: %v", err)
	}
}

func TestPAT_RequiresExpiryWithinMax(t *testing.T) {
	ctx := context.Background()
	s, _, c := newCredStore(t)
	by := owner(t)
	for _, exp := range []time.Time{{}, c.now(), c.now().Add(-time.Hour), c.now().Add(366 * day)} {
		if _, _, err := s.CreatePAT(ctx, authz.RoleReader, "ci", exp, by); err == nil {
			t.Errorf("expiry %v accepted", exp)
		}
	}
	if _, pat, err := s.CreatePAT(ctx, authz.RoleReader, "ci", c.now().Add(365*day), by); err != nil || !pat.Expires.Equal(c.now().Add(365*day)) {
		t.Fatalf("a one-year PAT: %+v, %v", pat, err)
	}
}

func TestPAT_NeverAdminNeverGuest(t *testing.T) {
	ctx := context.Background()
	s, bindings, c := newCredStore(t)
	for _, r := range []authz.Role{authz.RoleAdmin, authz.RoleGuest, authz.RoleNode, "owner", "bogus"} {
		if _, _, err := s.CreatePAT(ctx, r, "x", c.now().Add(day), owner(t)); err == nil {
			t.Errorf("role %q accepted", r)
		}
	}
	if pats, _ := s.ListPATs(ctx); len(pats) != 0 {
		t.Fatalf("a refused PAT was stored: %+v", pats)
	}
	if bs, _ := bindings.Bindings(ctx); len(bs) != 0 {
		t.Fatalf("a refused PAT was bound: %+v", bs)
	}
	for _, r := range authz.TokenRoles() {
		secret, pat, err := s.CreatePAT(ctx, r, "x", c.now().Add(day), owner(t))
		if err != nil {
			t.Fatalf("role %s: %v", r, err)
		}
		if !strings.HasPrefix(secret, PATPrefix) || pat.Role != r {
			t.Fatalf("role %s: secret %q pat %+v", r, secret, pat)
		}
	}
}

func TestPAT_VerifyBindsAndRevokeRemovesBinding(t *testing.T) {
	ctx := context.Background()
	s, bindings, c := newCredStore(t)
	secret, pat, err := s.CreatePAT(ctx, authz.RoleContributor, "laptop", c.now().Add(day), owner(t))
	if err != nil {
		t.Fatal(err)
	}
	tp := authz.Principal{Kind: authz.KindToken, ID: pat.ID}
	bs, _ := bindings.Bindings(ctx)
	if len(bs) != 1 || bs[0].Principal.String() != tp.String() || bs[0].Role != authz.RoleContributor {
		t.Fatalf("bindings after create = %+v", bs)
	}

	got, err := s.VerifyPAT(ctx, secret)
	if err != nil || got.ID != pat.ID || got.Role != authz.RoleContributor || got.Label != "laptop" {
		t.Fatalf("VerifyPAT = %+v, %v", got, err)
	}
	if got.Principal() != tp {
		t.Fatalf("Principal = %+v, want %+v", got.Principal(), tp)
	}
	for _, bad := range []string{"", "knomit_pat_", secret + "x", strings.TrimPrefix(secret, PATPrefix)} {
		if _, err := s.VerifyPAT(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("VerifyPAT(%q) = %v, want ErrInvalid", bad, err)
		}
	}

	if err := s.RevokePAT(ctx, pat.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyPAT(ctx, secret); !errors.Is(err, ErrInvalid) {
		t.Fatalf("revoked: %v", err)
	}
	if bs, _ := bindings.Bindings(ctx); len(bs) != 0 {
		t.Fatalf("binding survived revoke: %+v", bs)
	}
	if err := s.RevokePAT(ctx, pat.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second revoke: %v", err)
	}
}

func TestPAT_ExpiredIsInvalid(t *testing.T) {
	ctx := context.Background()
	s, _, c := newCredStore(t)
	secret, _, err := s.CreatePAT(ctx, authz.RoleReader, "x", c.now().Add(time.Hour), owner(t))
	if err != nil {
		t.Fatal(err)
	}
	c.add(time.Hour)
	if _, err := s.VerifyPAT(ctx, secret); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expired: %v", err)
	}
}

// TestPAT_StoredHashed: neither a PAT, a session cookie nor a link token
// appears in any column of any credential table.
func TestPAT_StoredHashed(t *testing.T) {
	ctx := context.Background()
	s, _, c := newCredStore(t)
	pat, _, err := s.CreatePAT(ctx, authz.RoleReader, "x", c.now().Add(day), owner(t))
	if err != nil {
		t.Fatal(err)
	}
	cookie, _, err := s.CreateSession(ctx, owner(t), "")
	if err != nil {
		t.Fatal(err)
	}
	link, _, err := s.MintLink(ctx, owner(t))
	if err != nil {
		t.Fatal(err)
	}
	secrets := []string{pat, strings.TrimPrefix(pat, PATPrefix), cookie, link}
	for _, table := range []string{"auth_tokens", "auth_sessions", "auth_login_links", "auth_role_bindings"} {
		rows, err := s.db.QueryContext(ctx, `SELECT * FROM `+table)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		n := 0
		for rows.Next() {
			n++
			vals := make([]sql.NullString, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for i, v := range vals {
				for _, sec := range secrets {
					if strings.Contains(v.String, sec) {
						t.Errorf("%s.%s holds a secret", table, cols[i])
					}
				}
			}
		}
		rows.Close()
		if n == 0 {
			t.Errorf("%s is empty: the check is vacuous", table)
		}
	}
}
