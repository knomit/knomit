package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestWriteDestination_Summary pins the sentence the caller actually reads.
// The structured field is the machine-readable half; this is the half a model
// reacts to, and the whole point of the stamp is that an unintended
// destination gets NOTICED rather than returning success indistinguishable
// from an intended one.
func TestWriteDestination_Summary(t *testing.T) {
	t.Run("repo binding names no lens", func(t *testing.T) {
		d := writeDestination{Repo: "knomit", Branch: "agent/main"}
		got := d.summary("2 facts")
		for _, want := range []string{"2 facts", `"knomit"`, `"agent/main"`} {
			if !strings.Contains(got, want) {
				t.Errorf("summary %q missing %q", got, want)
			}
		}
		if strings.Contains(got, "lens") {
			t.Errorf("repo binding mentioned a lens: %q", got)
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

// TestWriteDestination_JSONShape pins the wire shape. `lens` is omitempty so a
// repo-bound write does not carry a misleading empty lens field.
func TestWriteDestination_JSONShape(t *testing.T) {
	raw, err := json.Marshal(writeDestination{Repo: "r", Branch: "agent/main"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "lens") {
		t.Errorf("repo-bound destination emitted a lens key: %s", raw)
	}

	raw, err = json.Marshal(writeDestination{Repo: "r", Branch: "agent/main", Lens: "l"})
	if err != nil {
		t.Fatal(err)
	}
	var back writeDestination
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("round-trip: %v", err)
	}
	if back.Repo != "r" || back.Branch != "agent/main" || back.Lens != "l" {
		t.Errorf("round-trip mismatch: %+v", back)
	}
}

func TestPluralFacts(t *testing.T) {
	for n, want := range map[int]string{0: "0 facts", 1: "1 fact", 2: "2 facts"} {
		if got := pluralFacts(n); got != want {
			t.Errorf("pluralFacts(%d) = %q, want %q", n, got, want)
		}
	}
}
