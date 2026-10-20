package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
)

// A change of origin (user ruling 2026-10-18, #544): the switch itself is the
// signal. At the moment of a switch local main is the OLD origin's consensus;
// a new origin whose main is behind (or diverged from) local main is simply
// another repository's state, not a removal: removals happen on the SAME
// origin. So the first sync with a different origin is the ordinary
// merge-based origin sync (fetch, take its main, merge it into the agent
// branch, push this copy's branch); the rewind replay
// (reconcileAgentRebase) runs only when the SAME origin's main moved back.
// Unrelated histories (no common commit) keep the existing behaviour (issue
// #541).
//
// "The same origin" is the identity recorded under originIdentityKey
// (originIdentity of the stored URL: credential-free, normalised spellings),
// compared with the current origin's. It is recorded only at the end of a
// Sync round whose fetch and reconcile succeeded, so a first round with a new
// origin that fails is still treated as a switch next round. With nothing
// recorded (a repository synced before this existed) a round is not a
// switch.
const originIdentityKey = "origin_identity"

// readOriginIdentity returns the recorded origin identity, "" when none is.
func (rh *repoHandler) readOriginIdentity() (string, error) {
	var v string
	err := rh.DB().QueryRow(`SELECT value FROM meta WHERE key = ?`, originIdentityKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read origin identity: %w", err)
	}
	return v, nil
}

// recordOriginIdentity records id (an originIdentity, never a raw URL).
func (rh *repoHandler) recordOriginIdentity(id string) error {
	if _, err := rh.DB().Exec(`INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`, originIdentityKey, id); err != nil {
		return fmt.Errorf("record origin identity: %w", err)
	}
	return nil
}

// branchSharesHistory reports whether agentBranch and local upstreamMain have
// a common commit.
func (rh *repoHandler) branchSharesHistory(agentBranch, upstreamMain string) (bool, error) {
	a, err := rh.ResolveRef(context.Background(), agentBranch)
	if err != nil {
		return false, err
	}
	m, err := rh.ResolveRef(context.Background(), upstreamMain)
	if err != nil {
		return false, err
	}
	ac, err := rh.Repository().CommitObject(a)
	if err != nil {
		return false, err
	}
	mc, err := rh.Repository().CommitObject(m)
	if err != nil {
		return false, err
	}
	bases, err := ac.MergeBase(mc)
	if err != nil {
		return false, err
	}
	return len(bases) > 0, nil
}

// originIdentity is the part of an origin URL that names WHICH origin it is.
// These spellings of one origin give one identity:
//   - credentials (user info, which may hold a token) are dropped, also from a
//     URL that does not parse (everything up to its LAST "@");
//   - the scheme and host are lower-cased and a default port (https 443,
//     http 80, ssh 22) is dropped;
//   - the path is cleaned (`/./`, `/../`, doubled slashes), and a trailing
//     `.git` and a trailing slash are dropped;
//   - the scp form (`user@host:owner/repo`) is the ssh URL of the same host
//     and path (`ssh://host/owner/repo`);
//   - a bare absolute path is the file:// URL of that path.
//
// Every other difference is a different origin: the path's case, http versus
// https, https versus ssh, another host name for the same server, a
// non-default port, a symlinked path. This is what is recorded, so no
// credential is ever written to the meta table or logged.
//
// It has siblings that answer "the same origin?" for other purposes:
// repos.sameFleetURL (the fleet's credential-change test) and the web UI's
// own comparison; they are not required to agree with this one.
func originIdentity(raw string) string {
	raw = strings.TrimSpace(raw)
	switch {
	case strings.Contains(raw, "://"):
		u, err := url.Parse(raw)
		if err != nil {
			return strings.TrimSuffix(stripUserinfo(raw), "/")
		}
		scheme := strings.ToLower(u.Scheme)
		host := strings.ToLower(u.Hostname())
		if port := u.Port(); port != "" && !isDefaultPort(scheme, port) {
			host += ":" + port
		}
		return scheme + "://" + host + cleanOriginPath(u.EscapedPath())
	case isScpLike(raw):
		hostPart, p, _ := scpSplit(raw)
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		return "ssh://" + hostPart + cleanOriginPath(p)
	case strings.HasPrefix(raw, "/"):
		return "file://" + cleanOriginPath(raw)
	case len(raw) >= 3 && raw[1] == ':' && (raw[2] == '/' || raw[2] == '\\'):
		// A Windows drive path, C:\kb or C:/kb.
		return "file://" + cleanOriginPath("/"+strings.ReplaceAll(raw, `\`, "/"))
	default:
		return strings.TrimSuffix(stripUserinfo(raw), "/")
	}
}

// cleanOriginPath cleans a URL path and drops a trailing ".git" and slash.
func cleanOriginPath(p string) string {
	if p == "" {
		return ""
	}
	p = path.Clean(p)
	p = strings.TrimSuffix(p, ".git")
	p = strings.TrimSuffix(p, "/")
	return p
}

func isDefaultPort(scheme, port string) bool {
	return scheme == "https" && port == "443" || scheme == "http" && port == "80" || scheme == "ssh" && port == "22"
}

// isScpLike reports the scp form, [user@]host:path: no scheme, a colon whose
// left side is a host (no slash or backslash, longer than a drive letter).
func isScpLike(raw string) bool {
	_, _, ok := scpSplit(raw)
	return ok
}

// scpSplit splits the scp form [user[:secret]@]host:path into its host and
// path, without the user info: everything up to the last "@" before the path
// is dropped first, so a secret in it is never kept.
func scpSplit(raw string) (host, p string, ok bool) {
	s := raw
	head := s
	if i := strings.IndexAny(s, `/\`); i >= 0 {
		head = s[:i]
	}
	if at := strings.LastIndex(head, "@"); at >= 0 {
		s = s[at+1:]
	}
	host, p, ok = strings.Cut(s, ":")
	if !ok || strings.ContainsAny(host, `/\`) || len(host) <= 1 {
		return "", "", false
	}
	return strings.ToLower(host), p, true
}

// stripUserinfo drops everything after the scheme up to the LAST "@" in the
// string, wherever it is, so a secret holding "/", "?", "#" or "@" is not
// kept. Used where url.Parse refused the string.
func stripUserinfo(s string) string {
	prefix, rest := "", s
	if i := strings.Index(s, "://"); i >= 0 {
		prefix, rest = s[:i+3], s[i+3:]
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	return prefix + rest
}
