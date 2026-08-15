package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"knomit/internal/repos"
)

// TestWriteDestination_Summary pins the sentence the caller actually reads.
// The structured field is the machine-readable half; this is the half a model
// reacts to, and the whole point of the stamp is that an unintended
// destination gets NOTICED rather than returning success indistinguishable
// from an intended one.
func TestWriteDestination_Summary(t *testing.T) {
	t.Run("repo binding names no lens", func(t *testing.T) {
		d := writeDestination{Repo: "knomit", RepoID: "3ec012f5b4d2", Branch: "agent/main"}
		got := d.summary("2 facts")
		for _, want := range []string{"2 facts", `"knomit"`, "3ec012f5b4d2", `"agent/main"`} {
			if !strings.Contains(got, want) {
				t.Errorf("summary %q missing %q", got, want)
			}
		}
		if strings.Contains(got, "lens") {
			t.Errorf("repo binding mentioned a lens: %q", got)
		}
	})

	// The name alone does not identify a server: it is per-machine, mutable,
	// and unique only among ONE server's active repos. Two connected knomit
	// servers each holding a repo called "knomit" on "agent/main" is the
	// ordinary case and precisely the one the stamp exists to disambiguate, so
	// the sentence a caller reads must differ between them.
	t.Run("same name on two servers reads differently", func(t *testing.T) {
		a := writeDestination{Repo: "knomit", RepoID: "3ec012f5b4d2", Branch: "agent/main"}
		b := writeDestination{Repo: "knomit", RepoID: "7b4887ce51d9", Branch: "agent/main"}
		if a.summary("1 fact") == b.summary("1 fact") {
			t.Errorf("two different repos produced an identical summary: %q", a.summary("1 fact"))
		}
	})

	// ShortID returns "" when the store is unavailable and identity is
	// genuinely unknown. The sentence must then read as an ordinary sentence,
	// not one with an empty parenthetical in it.
	t.Run("unknown id leaves no empty parenthetical", func(t *testing.T) {
		got := writeDestination{Repo: "knomit", Branch: "agent/main"}.summary("1 fact")
		if strings.Contains(got, "()") || strings.Contains(got, " ()") {
			t.Errorf("empty id left a stray parenthetical: %q", got)
		}
		if !strings.Contains(got, `repo "knomit" on branch "agent/main"`) {
			t.Errorf("summary %q lost its shape without an id", got)
		}
	})

	t.Run("lens binding names the write repo, not the lens, as destination", func(t *testing.T) {
		d := writeDestination{Repo: "knomit-kb", Branch: "agent/main", Lens: "knomit-dev"}
		got := d.summary("1 fact")
		// The repo is where the bytes land; the lens is context for why.
		// Reporting the lens AS the destination would misdescribe a write, since
		// a lens reads a union but writes to exactly one member.
		repoAt := strings.Index(got, `"knomit-kb"`)
		lensAt := strings.Index(got, `"knomit-dev"`)
		if repoAt < 0 || lensAt < 0 {
			t.Fatalf("summary %q must name both the repo and the lens", got)
		}
		if repoAt > lensAt {
			t.Errorf("lens named before the write repo, which reads as the destination: %q", got)
		}
	})
}

// TestWriteDestination_JSONShape pins the wire shape. `lens` and `repo_id` are
// omitempty so a repo-bound write carries no misleading empty lens field, and
// an unresolvable identity is absent rather than an empty string a caller would
// read as an id.
func TestWriteDestination_JSONShape(t *testing.T) {
	raw, err := json.Marshal(writeDestination{Repo: "r", Branch: "agent/main"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "lens") {
		t.Errorf("repo-bound destination emitted a lens key: %s", raw)
	}
	if strings.Contains(string(raw), "repo_id") {
		t.Errorf("unknown identity emitted a repo_id key: %s", raw)
	}

	raw, err = json.Marshal(writeDestination{Repo: "r", RepoID: "3ec012f5b4d2", Branch: "agent/main", Lens: "l"})
	if err != nil {
		t.Fatal(err)
	}
	var back writeDestination
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("round-trip: %v", err)
	}
	if back.Repo != "r" || back.RepoID != "3ec012f5b4d2" || back.Branch != "agent/main" || back.Lens != "l" {
		t.Errorf("round-trip mismatch: %+v", back)
	}
}

// TestDescribeWriteDestination_StampsRepoID pins that the id on the stamp is
// the SAME identifier the caller already addresses facts with — the 12-hex
// root-commit form that appears in kb://<id>/… paths and the knomit_repos mount
// table — and not the registry uid or the display name, which are different
// identifiers with different lifetimes.
func TestDescribeWriteDestination_StampsRepoID(t *testing.T) {
	svc, _, _ := newPrinciplesTestRepo(t)
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:        "stamped",
		UID:         nextTestRepoUID(),
		AgentBranch: "agent/test",
		Svc:         svc,
	})

	d := describeWriteDestination(repos.NewBindingOfRepo(ri, ""))
	if d.Repo != "stamped" || d.Branch != "agent/test" {
		t.Fatalf("unexpected destination: %+v", d)
	}
	if d.RepoID != ri.ShortID() || len(d.RepoID) != 12 {
		t.Errorf("repo_id = %q, want the 12-hex short id %q", d.RepoID, ri.ShortID())
	}
	if d.RepoID == ri.UID() {
		t.Errorf("repo_id is the registry uid; a uid addresses nothing in a kb:// path")
	}
	if d.Lens != "" {
		t.Errorf("a synthesized repo binding named a lens: %q", d.Lens)
	}
}

func TestPluralFacts(t *testing.T) {
	for n, want := range map[int]string{0: "0 facts", 1: "1 fact", 2: "2 facts"} {
		if got := pluralFacts(n); got != want {
			t.Errorf("pluralFacts(%d) = %q, want %q", n, got, want)
		}
	}
}
