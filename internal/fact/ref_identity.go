package fact

import (
	"net/url"
	"strings"
)

// RefIdentity returns a comparison key for raw: two refs that cite the same
// evidence have the same key, whatever their spelling. It is for COMPARING
// only — nothing stored is rewritten from it (#361).
//
// Built on ClassifyRef, so a bare "kb/x.md" and the canonical
// "kb://<localRepoID>/kb/x.md" collide (the learn merge sees the former while
// storage holds the latter — the compare-classified-not-raw invariant).
//
//   - local fact:   path
//   - foreign fact: repo id + path
//   - system file:  path (exact case), plus repo id when foreign
//   - src://:       repo + path + blob. Commit and #L range are ignored — the
//     same blob is the same content. A ref with no blob (legacy) falls back to
//     repo + path + commit.
//   - URL:          lowercased scheme and host, fragment dropped, one trailing
//     '/' dropped, query kept.
//
// A malformed ref keys on its raw string so it still compares equal to itself.
func RefIdentity(raw, localRepoID string) string {
	c := ClassifyRef(raw, localRepoID)
	switch c.Kind {
	case RefLocalFact:
		return "kb|" + c.Path
	case RefForeignFact:
		return "kb|" + c.RepoID + "|" + c.Path
	case RefLocalSystemFile:
		return "file|" + c.Path
	case RefForeignSystemFile:
		return "file|" + c.RepoID + "|" + c.Path
	case RefSourceCode:
		if c.Blob != "" {
			return "src|" + c.RepoID + "|" + c.Path + "|" + c.Blob
		}
		return "src|" + c.RepoID + "|" + c.Path + "||" + c.Commit
	case RefExternalURL:
		return "url|" + normalizeURL(raw)
	}
	return "raw|" + raw
}

func normalizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		// Unparseable: fall back to the old string-level normalization.
		if i := strings.Index(raw, "#"); i >= 0 {
			raw = raw[:i]
		}
		return strings.TrimSuffix(raw, "/")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Fragment, u.RawFragment = "", ""
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawPath = strings.TrimSuffix(u.RawPath, "/")
	return u.String()
}

// HasNovelRef reports whether incoming cites at least one ref whose identity is
// not among existing's. A ref naming selfPath (the merged fact, in either
// stored form) is never novel.
func HasNovelRef(incoming, existing []string, selfPath, localRepoID string) bool {
	have := make(map[string]struct{}, len(existing)+1)
	for _, r := range existing {
		have[RefIdentity(r, localRepoID)] = struct{}{}
	}
	if selfPath != "" {
		have[RefIdentity(selfPath, localRepoID)] = struct{}{}
	}
	for _, r := range incoming {
		if _, ok := have[RefIdentity(r, localRepoID)]; !ok {
			return true
		}
	}
	return false
}
