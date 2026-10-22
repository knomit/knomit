package cmd

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"strings"
	"testing"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
)

// mainLeaf is id's leaf on n's copy of the fleet's main.
func mainLeaf(t *testing.T, n *fleetNode, id string) *x509.Certificate {
	t.Helper()
	var raw []byte
	if err := n.fleet(t).WithRead(func(s *store.Service) {
		b, ok, err := s.FileAt("main", "artifacts/fleet/certs/"+id+".pem", 1<<16)
		if err != nil || !ok {
			t.Fatalf("%s: no leaf for %s on main (%v)", n.id, id, err)
		}
		raw = b
	}); err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(raw)
	if blk == nil {
		t.Fatalf("%s: %s's leaf is not PEM", n.id, id)
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The PR #528 walk finding, as a regression test over real mTLS hubs: alpha
// (hub, master) revokes charlie, charlie leaves, alpha enrolls charlie's SAME
// key again and charlie rejoins; then the master role moves to charlie
// (alpha's root files imported, alpha stands down, charlie hosts the fleet)
// and alpha, now a member, follows charlie. The walk saw alpha's push
// refused by charlie's judge (artifact-replace: certs/charlie.pem replaced
// with a leaf whose NotBefore is not later). Alpha's branch must reach the
// new hub: its record change is admitted and merged into charlie's main.
func TestFleetWalk_OldMastersBranchIsAdmittedAfterARejoinAndASwitch(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	alpha := newFleetNode(t, dir, "alpha")
	if _, err := alpha.mgr.CreateFleet(ctx, repos.FleetCreateSpec{}); err != nil {
		t.Fatal(err)
	}
	alpha.serve(t)
	charlie := newFleetNode(t, dir, "charlie")
	charlie.join(t, alpha)
	hubPublishes(t, alpha, recordPath("charlie"), "state: active")
	charlie.round(t)

	// Revoke, leave, re-enroll the same key, rejoin, all within a second or
	// two of the first enroll: the walk's timing.
	first := mainLeaf(t, alpha, "charlie")
	if _, err := alpha.mgr.RevokeFleetMember(ctx, "charlie", "walk"); err != nil {
		t.Fatal(err)
	}
	charlie.asClient(t)
	if _, err := charlie.mgr.LeaveFleet(ctx); err != nil {
		t.Fatal(err)
	}
	charlie.join(t, alpha)
	// The cause the walk met: a re-enroll in the same second as the leaf it
	// replaces issued a leaf with the SAME NotBefore, which no judge's
	// leaf-renewal admits, and the hub's own commits reached main unjudged
	// (fixed by #533: renewalIssueTime and the own-commit judge). The
	// replacement on main must be strictly later.
	second := mainLeaf(t, alpha, "charlie")
	if second.SerialNumber.Cmp(first.SerialNumber) == 0 || !second.NotBefore.After(first.NotBefore) {
		t.Fatalf("the re-enrolled leaf on main: serial %s NotBefore %s; the revoked one: serial %s NotBefore %s — want a later NotBefore",
			second.SerialNumber.Text(16), second.NotBefore, first.SerialNumber.Text(16), first.NotBefore)
	}
	hubPublishes(t, alpha, recordPath("charlie"), "state: active")
	charlie.round(t)
	if s := memberStates(t, alpha)["charlie"]; s != fact.MemberActive {
		t.Fatalf("fixture: charlie after the rejoin: %q", s)
	}

	// The master switch to charlie.
	files, err := alpha.mgr.FleetRootFiles()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := charlie.mgr.InstallFleetRoot(files.Files); err != nil {
		t.Fatal(err)
	}
	if err := alpha.mgr.StandDown(ctx); err != nil {
		t.Fatalf("alpha stands down: %v", err)
	}
	charlie.round(t)
	charlie.serve(t)
	if rec := charlie.localAPI(http.MethodPost, "/fleet/host", "{}"); rec.Code != http.StatusOK {
		t.Fatalf("charlie hosts the fleet: %d %s", rec.Code, rec.Body.String())
	}
	alpha.asClient(t)
	if rec := alpha.localAPI(http.MethodPost, "/fleet/follow", `{"url":"`+charlie.fleetURL()+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("alpha follows charlie: %d %s", rec.Code, rec.Body.String())
	}

	// Alpha's branch reaches the new hub and is merged.
	editOwnRecord(t, alpha, alpha.cfg.ExternalAddresses[0], "https://alpha.moved.example")
	alpha.round(t)
	if st, _ := alpha.mgr.FleetStatus(ctx); st.LastError != "" {
		t.Fatalf("alpha's push to the new hub was refused: %s", st.LastError)
	}
	if r := charlie.mgr.FleetRefusals()["agent/alpha"]; r.Reason != "" {
		t.Fatalf("the new hub refused alpha's branch: %+v", r)
	}
	hubPublishes(t, charlie, recordPath("alpha"), "https://alpha.moved.example")
	if s := memberStates(t, charlie)["alpha"]; s != fact.MemberActive {
		t.Fatalf("alpha on the new hub's main: %q", s)
	}
	if strings.Contains(readAt(t, charlie.fleet(t), "main", recordPath("alpha")), "alpha.moved.example") == false {
		t.Fatal("alpha's record change is not on the new hub's main")
	}
}
