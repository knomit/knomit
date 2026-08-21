package knomitapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// corpusServer serves the three queries SessionContext makes, so a test can
// assert which endpoint supplied which block.
type corpusServer struct {
	principles string
	invariants string
	recent     string
	hits       map[string]int
}

func (c *corpusServer) start(t *testing.T) {
	t.Helper()
	if c.hits == nil {
		c.hits = map[string]int{}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		w.Header().Set("Content-Type", "application/hal+json")
		switch {
		case strings.Contains(q.Get("path"), "kb/principles/"):
			c.hits["principles"]++
			w.Write([]byte(c.principles))
		case strings.Contains(q.Get("path"), "kb/invariants/"):
			c.hits["invariants"]++
			w.Write([]byte(c.invariants))
		default:
			c.hits["recent"]++
			w.Write([]byte(c.recent))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("KNOMIT_BASE_URL", srv.URL)
}

func facts(items ...string) string {
	return `{"_embedded":{"facts":[` + strings.Join(items, ",") + `]}}`
}

func principle(path, title string) string {
	return `{"path":"` + path + `","title":"` + title + `","domain":["global"],"entities":["designer"]}`
}

func plain(path, title string) string {
	return `{"path":"` + path + `","title":"` + title + `","domain":[],"entities":[]}`
}

// REGRESSION: principles were filtered out of a `sort=recent&limit=200` page,
// so a principle written long ago never appeared. Measured on this project's
// own corpus, 14 global principles existed and 1 fell inside the window.
// SessionContext must ask for them directly instead.
func TestSessionContext_PrinciplesComeFromTargetedQueryNotRecentWindow(t *testing.T) {
	c := &corpusServer{
		principles: facts(
			principle("kb/principles/philosophy/a/1.md", "Old principle one"),
			principle("kb/principles/philosophy/b/2.md", "Old principle two"),
		),
		// The recent page contains NO principles at all — the realistic case.
		recent: facts(plain("kb/gotchas/x/3.md", "Some recent gotcha")),
	}
	c.start(t)

	text, stats := SessionContext("r", "b")
	if stats.Globals != 2 {
		t.Errorf("Globals = %d, want 2 (principles must not depend on recency)", stats.Globals)
	}
	for _, want := range []string{"Old principle one", "Old principle two"} {
		if !strings.Contains(text, want) {
			t.Errorf("block missing %q:\n%s", want, text)
		}
	}
	if c.hits["principles"] == 0 {
		t.Error("no targeted principles query was made")
	}
}

// REGRESSION: the recent list was drawn from the same unfiltered window used
// for principles with no exclusion, so a principle among the 5 most recent
// facts was printed twice in one block.
func TestSessionContext_PrincipleNotRepeatedUnderRecentWork(t *testing.T) {
	dup := "kb/principles/philosophy/review-is-a-gate/x.md"
	c := &corpusServer{
		principles: facts(principle(dup, "Review is a gate")),
		recent: facts(
			principle(dup, "Review is a gate"), // same fact, freshly written
			plain("kb/gotchas/y/9.md", "A real recent item"),
		),
	}
	c.start(t)

	text, stats := SessionContext("r", "b")
	if got := strings.Count(text, "Review is a gate"); got != 1 {
		t.Errorf("title appears %d times, want 1:\n%s", got, text)
	}
	if stats.Recent != 1 || !strings.Contains(text, "A real recent item") {
		t.Errorf("recent list should keep the non-duplicate entry:\n%s", text)
	}
}

// The invariants fallback only fires when there are no global principles, and
// it too must come from a targeted query.
func TestSessionContext_InvariantsFallbackOnlyWithoutPrinciples(t *testing.T) {
	c := &corpusServer{
		principles: facts(),
		invariants: facts(plain("kb/invariants/store/a.md", "An invariant")),
		recent:     facts(plain("kb/gotchas/z/1.md", "Recent thing")),
	}
	c.start(t)

	text, stats := SessionContext("r", "b")
	if stats.InvariantsFallback != 1 || !strings.Contains(text, "LOAD-BEARING INVARIANTS:") {
		t.Errorf("fallback did not fire:\n%s", text)
	}
	if strings.Contains(text, "PROJECT PRINCIPLES:") {
		t.Error("principles header shown with no principles")
	}
}

func TestSessionContext_NoFactsAnywhere_Skips(t *testing.T) {
	c := &corpusServer{principles: facts(), invariants: facts(), recent: facts()}
	c.start(t)

	text, stats := SessionContext("r", "b")
	if text != "" || stats.SkipReason != "no_facts" {
		t.Errorf("got (%q,%q), want (\"\",no_facts)", text, stats.SkipReason)
	}
}

func TestSessionContext_ServerDown_Skips(t *testing.T) {
	t.Setenv("KNOMIT_BASE_URL", "http://127.0.0.1:1")
	text, stats := SessionContext("r", "b")
	if text != "" || stats.SkipReason != "no_facts" {
		t.Errorf("got (%q,%q), want (\"\",no_facts)", text, stats.SkipReason)
	}
}
