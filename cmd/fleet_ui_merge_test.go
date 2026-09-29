package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"knomit/internal/config"
	"knomit/internal/pki"
	"knomit/internal/pki/pkitest"
	"knomit/internal/platform/fileuri"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// TestFleetOrigin_PeerBranchMergedInTheUIAndReclonable is the F11 UI merge end
// to end, on the production stack:
//
//  1. peer A pushes its own branch to host B over F11 (mTLS /git);
//  2. B's operator merges it through the REST call the web UI makes, on B's
//     plain listener as the loopback operator;
//  3. B's agent branch now holds A's fact, under a merge commit B signed;
//  4. B pushes to its origin (what B's sync does), and a fresh re-clone of B
//     with B's key SUCCEEDS — E4 trusts what B's signed merge brought in
//     (the chain-of-trust ruling), with no forge merge and no verify accept;
//  5. the same peer commit put straight onto origin's copy of B's agent
//     branch, with no merge by B, is still refused.
func TestFleetOrigin_PeerBranchMergedInTheUIAndReclonable(t *testing.T) {
	ctx := context.Background()
	f := pkitest.New(t)
	host := newSignedFleetServer(t, f)

	// 1. A clones B over the fleet, writes a fact, pushes its own branch.
	aDir, aKey, aMember, _ := newFleetFetcherHome(t, f, "alpha")
	if err := pki.InstallGitTransport(aDir, aKey); err != nil {
		t.Fatalf("install: %v", err)
	}
	aBranch := "agent/alpha-" + pki.Short(aMember.Fingerprint())
	aMgr := repos.New(ctx, repos.Deps{Cfg: config.Config{Home: t.TempDir(), OntologyRoot: "kb"},
		KeyPath: aKey, Signer: keySigner(t, aKey), AgentBranch: aBranch, DisableBackgroundSync: true})
	if err := aMgr.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { aMgr.Close() })
	riA, err := aMgr.Create(ctx, repos.CreateSpec{Name: "kb", Mode: "clone",
		Origin: &repos.OriginSpec{URL: pki.GitScheme + "://" + host.tlsAddr + "/git/kb", AuthMethod: "cert"}}, nil)
	if err != nil {
		t.Fatalf("clone from the host: %v", err)
	}
	var aTip string
	if err := riA.WithRead(func(s *store.Service) {
		if _, err := s.Facts().WriteFact(ctx, aBranch, "kb/merged.md", factBody("merged"), "merged", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Facts().WriteFact(ctx, aBranch, "kb/merged2.md", factBody("merged2"), "merged2", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Remote().Push(ctx, aBranch, nil); err != nil {
			t.Fatalf("push to the host: %v", err)
		}
		aTip, err = s.Branches().HeadCommit(ctx, aBranch)
		if err != nil {
			t.Fatal(err)
		}
	}); err != nil {
		t.Fatal(err)
	}

	// 2. The operator's merge, exactly as the UI sends it.
	url := "http://" + host.plain + "/api/v1/repos/kb/branches/" + strings.ReplaceAll(aBranch, "/", ":") + "/merge"
	resp, err := http.Post(url, "application/json", strings.NewReader(`{"expected_tip":"`+aTip+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || res["mode"] != "merge" || res["into"] != fleetServerAgent {
		t.Fatalf("merge: %d %v", resp.StatusCode, res)
	}

	// 3. B's agent branch holds A's fact under a merge commit B signed.
	bare := filepath.Join(t.TempDir(), "origin.git")
	if _, err := gogit.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}
	if err := host.kb.WithRead(func(s *store.Service) {
		got, err := s.Facts().ReadFact(ctx, fleetServerAgent, "kb/merged.md", nil)
		if err != nil || !strings.Contains(got.Content, "merged") {
			t.Fatalf("B's agent branch lacks the peer's fact after the merge: %v", err)
		}
		tip, err := s.Branches().HeadCommit(ctx, fleetServerAgent)
		if err != nil {
			t.Fatal(err)
		}
		if tip != res["new_tip"] {
			t.Fatalf("agent tip %s, merge reported %v", tip, res["new_tip"])
		}

		// 4. B's sync pushes its agent branch (and the origin has B's main).
		if err := s.ConfigureRemote(fileuri.New(bare), "main", fleetServerAgent); err != nil {
			t.Fatal(err)
		}
		for _, br := range []string{"main", fleetServerAgent} {
			if _, err := s.Remote().Push(ctx, br, nil); err != nil {
				t.Fatalf("push %s to origin: %v", br, err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}

	origin, err := gogit.PlainOpen(bare)
	if err != nil {
		t.Fatal(err)
	}
	agentRef, err := origin.Reference(plumbing.NewBranchReferenceName(fleetServerAgent), true)
	if err != nil {
		t.Fatalf("origin has no %s: %v", fleetServerAgent, err)
	}
	mergeCommit, err := object.GetCommit(origin.Storer, agentRef.Hash())
	if err != nil {
		t.Fatal(err)
	}
	if len(mergeCommit.ParentHashes) != 2 || mergeCommit.ParentHashes[1].String() != aTip {
		t.Fatalf("origin's %s tip is not B's merge [B, A]: parents %v", fleetServerAgent, mergeCommit.ParentHashes)
	}

	reclone := func() error {
		svc, err := store.Open(filepath.Join(t.TempDir(), "reclone.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = svc.Close() })
		svc.SetSigner(host.signer)
		_, _, err = svc.InitFromRemote(fileuri.New(bare), nil, "main", fleetServerAgent, nil, nil)
		return err
	}
	if err := reclone(); err != nil {
		t.Fatalf("a re-clone of B must adopt the peer commits B's signed merge brought in: %v", err)
	}

	// 5. The same peer commit, put on origin's copy of B's agent branch
	// WITHOUT a merge by B, is still refused.
	if err := origin.Storer.SetReference(plumbing.NewHashReference(
		plumbing.NewBranchReferenceName(fleetServerAgent), plumbing.NewHash(aTip))); err != nil {
		t.Fatal(err)
	}
	if err := reclone(); !errors.Is(err, store.ErrForeignLineage) {
		t.Fatalf("a peer commit B never merged must still be refused, got %v", err)
	}
}
