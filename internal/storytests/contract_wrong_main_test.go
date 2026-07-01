//go:build contract

package storytests

import (
	"testing"

	"knomit/internal/store"
	"knomit/internal/testenv"
)

// Cell C — symptom #5: a remote whose default branch is "master" (no "main")
// must be adopted correctly, not silently defaulted to an empty "main".
//
// Scenario: an HTTP remote whose symbolic HEAD is refs/heads/master carries
// real content on master (a fact). A repo connects to it over the production
// InitFromRemote clone path (which auto-detects upstream: prefer "main", else
// the remote's HEAD branch, else fall back to "main"). Since this remote has NO
// "main", detection must fall through to the remote HEAD ("master").
//
// CONTRACT: the repo adopts "master" as upstream (remotes.branch == "master")
// and the master content is present locally — NOT an empty repo produced by
// silently defaulting to a non-existent "main".
//
// Characterization: repo.go InitFromRemote prefers main → detected HEAD →
// "main" fallback; detectRemoteUpstream reads the served symbolic HEAD over
// HTTP. A master-only remote should therefore be detected. Expected GREEN
// (mirrors the file:// G11 test at master_upstream_test.go, over HTTP).
func TestContract_WrongMain_MasterOnlyRemoteAdopted(t *testing.T) {
	sb := testenv.NewStoryboard(t)

	// HTTP remote whose default branch is "master" with real content on it.
	remote := sb.BareRemoteHTTPWithBranch("origin", "master")
	remote.WriteMain("kb/seed.md", testenv.Fact("master-seed"), "seed on master")
	if remote.UpstreamBranch() != "master" {
		t.Fatalf("fixture: expected remote upstream \"master\", got %q", remote.UpstreamBranch())
	}

	// Connect over the production clone path with no explicit upstream, so
	// detection runs.
	repo := sb.Repo("a").Connect(remote)

	// The remotes table must record "master" as the adopted upstream — not a
	// silent rewrite to "main".
	var stored *store.Remote
	repo.Instance().WithRead(func(svc *store.Service) {
		stored, _ = svc.Remote().GetRemote("origin")
	})
	if stored == nil {
		t.Fatal("CONTRACT VIOLATION (symptom #5): origin remote record missing after connect")
	}
	if stored.Branch != "master" {
		t.Fatalf("CONTRACT VIOLATION (symptom #5): master-only remote's upstream was "+
			"recorded as %q, not \"master\" — the product silently defaulted to a "+
			"non-existent \"main\"", stored.Branch)
	}

	// The master content must be present locally: on the local consensus branch
	// (master) AND inherited onto the agent branch. An empty repo here would
	// mean detection silently produced an empty "main".
	if !repo.Branch("master").HasFile("kb/seed.md") {
		t.Fatalf("CONTRACT VIOLATION (symptom #5): local \"master\" branch is missing "+
			"kb/seed.md — the remote's master content was not adopted (empty-main default)")
	}
	if !repo.Branch("agent/test").HasFile("kb/seed.md") {
		t.Fatalf("CONTRACT VIOLATION (symptom #5): agent branch did not inherit the "+
			"master seed — upstream detection did not resolve to master")
	}
	t.Logf("master-only remote adopted correctly: upstream=%q, seed present", stored.Branch)
}
