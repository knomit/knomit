package knomitapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/rs/zerolog/log"
)

// FactSummary is the subset of a knomit fact the hooks read.
type FactSummary struct {
	Path     string   `json:"path"`
	Title    string   `json:"title"`
	Entities []string `json:"entities"`
	Domain   []string `json:"domain"`
}

// RecentFactsURL builds the recent-facts URL used for the "Recent work" list.
//
// Escaped as defence in depth: callers already reject names that fail
// repos.IsValidName, but this file is the last place a scope name becomes a
// URL path segment, and a traversal- or query-shaped value reaching the
// server would resolve to a different resource entirely.
func RecentFactsURL(repo, branch string, limit int) string {
	return fmt.Sprintf("%s/api/v1/repos/%s/branches/%s/facts?sort=recent&limit=%d",
		BaseURL(), url.PathEscape(repo), url.PathEscape(EncodeBranch(branch)), limit)
}

// GlobalPrinciplesURL asks the server for designer-authored, project-wide
// principles DIRECTLY.
//
// These must not be filtered out of a recent-N window. Principles are written
// rarely and then sit still, so on any repo with normal churn none of them fall
// inside a recent window at all — measured on this project's own corpus, 14
// global principles existed and exactly 1 appeared in a recent-200 page, so the
// block silently reported a twentieth of what it should. The two targeted
// queries this function and InvariantFactsURL build are also far smaller than
// the 200-row page they replace.
func GlobalPrinciplesURL(repo, branch string, limit int) string {
	return fmt.Sprintf("%s/api/v1/repos/%s/branches/%s/facts?path=%s&entity=designer&domain=global&limit=%d",
		BaseURL(), url.PathEscape(repo), url.PathEscape(EncodeBranch(branch)),
		url.QueryEscape("kb/principles/"), limit)
}

// InvariantFactsURL asks for load-bearing invariants directly, for the rollout
// fallback used when a repo has no global principles yet.
func InvariantFactsURL(repo, branch string, limit int) string {
	return fmt.Sprintf("%s/api/v1/repos/%s/branches/%s/facts?path=%s&sort=recent&limit=%d",
		BaseURL(), url.PathEscape(repo), url.PathEscape(EncodeBranch(branch)),
		url.QueryEscape("kb/invariants/"), limit)
}

// FetchFacts calls a /facts HAL endpoint and returns the embedded collection.
// Returns nil on any error; each failure path logs at Warn so a dead server is
// visible in the bridge log rather than indistinguishable from an empty result.
func FetchFacts(u string) []FactSummary {
	resp, err := HTTPClient.Get(u) //nolint:noctx
	if err != nil {
		log.Warn().Err(err).Str("url", u).Msg("FetchFacts: GET failed")
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Warn().Int("status", resp.StatusCode).Str("url", u).Msg("FetchFacts: non-200")
		return nil
	}
	var body struct {
		Embedded struct {
			Facts []FactSummary `json:"facts"`
		} `json:"_embedded"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		log.Warn().Err(err).Str("url", u).Msg("FetchFacts: decode failed")
		return nil
	}
	return body.Embedded.Facts
}

// FilterGlobalPrinciples returns facts under kb/principles/ whose Entities
// contain "designer" and Domain contains "global", up to max. The two-axis
// filter is what makes a principle "global": designer-authored AND scoped
// project-wide rather than to a single subarea.
func FilterGlobalPrinciples(facts []FactSummary, max int) []FactSummary {
	out := make([]FactSummary, 0, max)
	for _, f := range facts {
		if !strings.HasPrefix(f.Path, "kb/principles/") {
			continue
		}
		if !containsString(f.Entities, "designer") {
			continue
		}
		if !containsString(f.Domain, "global") {
			continue
		}
		out = append(out, f)
		if len(out) >= max {
			break
		}
	}
	return out
}

// FilterByPathPrefix returns up to max facts whose Path starts with prefix.
func FilterByPathPrefix(facts []FactSummary, prefix string, max int) []FactSummary {
	out := make([]FactSummary, 0, max)
	for _, f := range facts {
		if strings.HasPrefix(f.Path, prefix) {
			out = append(out, f)
			if len(out) >= max {
				break
			}
		}
	}
	return out
}

// TopN returns the first n facts, or all of them if there are fewer.
func TopN(facts []FactSummary, n int) []FactSummary {
	if len(facts) <= n {
		return facts
	}
	return facts[:n]
}

// PrincipleShortPath returns "<bucket>/<slug>" from a fact path like
// "kb/principles/<bucket>/<slug>/<uuid>.md". Falls back to the raw path on
// shape mismatch.
func PrincipleShortPath(p string) string {
	const prefix = "kb/principles/"
	if !strings.HasPrefix(p, prefix) {
		return p
	}
	rest := strings.TrimPrefix(p, prefix)
	parts := strings.Split(rest, "/")
	if len(parts) < 2 {
		return rest
	}
	return parts[0] + "/" + parts[1]
}

func containsString(xs []string, target string) bool {
	for _, x := range xs {
		if x == target {
			return true
		}
	}
	return false
}
