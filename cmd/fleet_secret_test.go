package cmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	transportclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/control"
	"knomit/internal/testsupport/gitfixture"
	"knomit/test/testenv/gitserver"
)

const forgeToken = "FORGE-zz9-stored-secret"

// httpsForge seeds root/name (one commit on main, pushes enabled) and serves
// root over https, requiring x-token:forgeToken. go-git trusts the server's
// certificate for the test's duration (its https transport is swapped, as
// countStockHTTPS does). It returns the server and the repo's URL.
func httpsForge(t *testing.T, root, name string) (*gitserver.Server, string, string) {
	t.Helper()
	bare := filepath.Join(root, name)
	work := filepath.Join(t.TempDir(), "seed")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitfixture.InitBare(t, bare, "main")
	for _, args := range [][]string{
		{"-C", bare, "config", "http.receivepack", "true"},
		{"init", "--initial-branch=main", work},
		{"-C", work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "seed"},
		{"-C", work, "push", "-q", bare, "main"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	srv := gitserver.NewTLS(t, root)
	t.Cleanup(srv.Close)
	srv.Fault().RequireBasicAuth("x-token", forgeToken)
	prev := transportclient.Protocols["https"]
	transportclient.InstallProtocol("https", githttp.NewClient(srv.Client()))
	t.Cleanup(func() { transportclient.InstallProtocol("https", prev) })
	return srv, srv.URL + "/" + name, bare
}

func storeSecret(t *testing.T, n *fleetNode, name string) string {
	t.Helper()
	meta, err := n.mgr.Secrets().Create(control.SecretInput{Name: name, Type: control.SecretTypeToken,
		Hosts: []string{"127.0.0.1"}, Value: control.SecretValue{Token: forgeToken}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return meta.ID
}

func requireLinkedFleet(t *testing.T, n *fleetNode, url, secretID string) {
	t.Helper()
	o, err := n.mgr.Origins().Get(n.fleet(t).UID())
	if err != nil || o == nil {
		t.Fatalf("%s: origin: %v %v", n.id, o, err)
	}
	if o.URL != url || o.SecretID != secretID || o.AuthToken != "" || o.AuthMethod != "token" {
		t.Fatalf("%s: origin %+v; want %s linked to %s with no token", n.id, *o, url, secretID)
	}
}

func acceptedBy(srv *gitserver.Server) int {
	n := 0
	for _, a := range srv.Fault().AuthAttempts() {
		if a.OK {
			n++
		}
	}
	return n
}

// A fleet created on a forge with a stored secret, joined by a member with
// its own stored secret: both copies link their secret, and both reach the
// forge with it (the master publishes main, the member pushes its record).
func TestFleetSecret_CreateAndJoinForge(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	srv, url, bare := httpsForge(t, filepath.Join(dir, "forge"), "fleet.git")

	alpha := newFleetNode(t, dir, "alpha")
	aID := storeSecret(t, alpha, "alpha-forge")
	if _, err := alpha.mgr.CreateFleet(ctx, repos.FleetCreateSpec{Origin: &repos.OriginSpec{URL: url, SecretID: aID}}); err != nil {
		t.Fatal(err)
	}
	requireLinkedFleet(t, alpha, url, aID)
	if err := alpha.fleet(t).OriginRoundForTest(ctx); err != nil {
		t.Fatal(err)
	}
	if bareRev(t, bare, "main") != bareRev(t, bare, "agent/alpha") {
		t.Fatal("the master did not publish main with its secret")
	}

	bravo := newFleetNode(t, dir, "bravo")
	bID := storeSecret(t, bravo, "bravo-forge")
	enr, err := alpha.mgr.EnrollFleetMember(ctx, "bravo", bravo.keyLine(), "")
	if err != nil || !enr.Published {
		t.Fatalf("enroll: %v %+v", err, enr)
	}
	if _, err := bravo.mgr.JoinFleet(ctx, repos.FleetJoinSpec{Bundle: []byte(enr.Bundle), SecretID: bID}); err != nil {
		t.Fatalf("join: %v", err)
	}
	requireLinkedFleet(t, bravo, url, bID)
	if bareRev(t, bare, "agent/bravo") == "" {
		t.Fatal("bravo's record did not reach the forge")
	}
	if acceptedBy(srv) == 0 {
		t.Fatal("no request carried the secret")
	}
}

// "Update access" on a member: a stored secret links it (change
// credential), a follow with no credential keeps the link (unchanged), a
// pasted credential replaces it.
func TestFleetSecret_UpdateAccess(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_, url, _ := httpsForge(t, filepath.Join(dir, "forge"), "fleet.git")
	alpha := newFleetNode(t, dir, "alpha")
	if _, err := alpha.mgr.CreateFleet(ctx, repos.FleetCreateSpec{Origin: &repos.OriginSpec{URL: url, AuthMethod: "token", AuthToken: forgeToken}}); err != nil {
		t.Fatal(err)
	}
	id := storeSecret(t, alpha, "alpha-forge")

	st, err := alpha.mgr.FollowFleetHost(ctx, repos.OriginSpec{SecretID: id})
	if err != nil || st.Change != repos.FleetFollowCredential {
		t.Fatalf("link: %v (%q)", err, st.Change)
	}
	requireLinkedFleet(t, alpha, url, id)

	st, err = alpha.mgr.FollowFleetHost(ctx, repos.OriginSpec{})
	if err != nil || st.Change != repos.FleetFollowUnchanged {
		t.Fatalf("no credential: %v (%q)", err, st.Change)
	}
	requireLinkedFleet(t, alpha, url, id)

	st, err = alpha.mgr.FollowFleetHost(ctx, repos.OriginSpec{AuthMethod: "token", AuthToken: forgeToken})
	if err != nil || st.Change != repos.FleetFollowCredential {
		t.Fatalf("pasted: %v (%q)", err, st.Change)
	}
	if err := alpha.mgr.Secrets().Delete(id); err != nil {
		t.Fatalf("the pasted credential did not unlink: %v", err)
	}
}

// The master moves the fleet onto another forge with a stored secret: its
// copy links the secret and the seed reached the new forge with it.
func TestFleetSecret_Move(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	forgeDir := filepath.Join(dir, "forge")
	srv, url, _ := httpsForge(t, forgeDir, "fleet.git")
	alpha := newFleetNode(t, dir, "alpha")
	if _, err := alpha.mgr.CreateFleet(ctx, repos.FleetCreateSpec{Origin: &repos.OriginSpec{URL: url, AuthMethod: "token", AuthToken: forgeToken}}); err != nil {
		t.Fatal(err)
	}
	if err := alpha.fleet(t).OriginRoundForTest(ctx); err != nil {
		t.Fatal(err)
	}
	// An EMPTY second repository on the same server.
	moved := filepath.Join(forgeDir, "moved.git")
	gitfixture.InitBare(t, moved, "main")
	if out, err := exec.Command("git", "-C", moved, "config", "http.receivepack", "true").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	movedURL := srv.URL + "/moved.git"
	id := storeSecret(t, alpha, "new-forge")
	st, err := alpha.mgr.FollowFleetHost(ctx, repos.OriginSpec{URL: movedURL, SecretID: id})
	if err != nil || st.Change != repos.FleetFollowMoved {
		t.Fatalf("move: %v (%q)", err, st.Change)
	}
	requireLinkedFleet(t, alpha, movedURL, id)
	if bareRev(t, moved, "main") == "" {
		t.Fatal("the seed did not reach the new forge")
	}
}

// A hub-hosted fleet (an instance, a peer) takes no stored secret: the join
// check and the join both answer auth_invalid and install nothing.
func TestFleetSecret_PeerRefused(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	alpha := newFleetNode(t, dir, "alpha")
	if _, err := alpha.mgr.CreateFleet(ctx, repos.FleetCreateSpec{}); err != nil {
		t.Fatal(err)
	}
	alpha.serve(t)
	bravo := newFleetNode(t, dir, "bravo")
	id := storeSecret(t, bravo, "hub")
	enr, err := alpha.mgr.EnrollFleetMember(ctx, "bravo", bravo.keyLine(), "")
	if err != nil || !enr.Published {
		t.Fatalf("enroll: %v %+v", err, enr)
	}
	_, err = bravo.mgr.CheckFleetJoin(ctx, repos.FleetJoinSpec{Bundle: []byte(enr.Bundle), SecretID: id})
	if code := fleetCodeOf(t, err); code != "auth_invalid" {
		t.Fatalf("check with a secret: %s (%v)", code, err)
	}
	bravo.asClient(t)
	_, err = bravo.mgr.JoinFleet(ctx, repos.FleetJoinSpec{Bundle: []byte(enr.Bundle), SecretID: id})
	if code := fleetCodeOf(t, err); code != "auth_invalid" {
		t.Fatalf("join with a secret: %s (%v)", code, err)
	}
	var fe *repos.FleetError
	if !errors.As(err, &fe) {
		t.Fatalf("join: %T %v", err, err)
	}
	if bravo.mgr.Get("fleet") != nil {
		t.Fatal("the refused join left a fleet repository behind")
	}
}
