package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/rs/zerolog/log"
)

// The origin's POSITION is what this copy remembers of where the origin was:
// the watermarks (refs/knomit/agent-base/*, the main commit each agent branch
// last consumed) and the remote-tracking refs (refs/remotes/origin/*, the last
// tips fetched or pushed). Both describe ONE origin. Read against a different
// one they are wrong in the way that loses work: the rewind replay treats
// everything below the watermark (or below the seed taken from the tracking
// ref) as consumed, so own commits the OLD origin took but the new one lacks
// are dropped from the branch; and Push reads a stale refs/remotes/origin/
// <agent branch> as "the origin already has this" and sends nothing.
//
// So the position is tagged with the origin URL it belongs to (meta key
// originPositionURLKey), and the first Sync against a DIFFERENT URL forgets it
// before its fetch (user decision 2026-10-17). That sync then behaves as a
// first sync: with no watermark and no tracking ref there is nothing to seed
// from, the replay walk stops only at the first commit the new origin holds,
// and everything local the new origin lacks is treated as this copy's own and
// pushed.
//
// This is NOT the re-seeding at attach time that kb/invariants/store/
// remote-reconcile/replay-stop/5482c3ab.md (b) warns against: nothing is
// written at attach, and no value is invented. The position is removed, at
// the sync that would otherwise misread it, and the next reconcile writes a
// true one.
//
// The URL compared is the stored origin URL (control.db's, injected through
// Service.SetOrigin), not the derived git URL, and it is compared by identity
// (originIdentity, which lists the spellings that count as the same origin).
// A credential change keeps
// the URL and keeps the position. The check is lazy, at Sync, so every path
// that changes the origin (PUT origin, the connect wizard, a swap, a detach
// and re-attach, the fleet's follow and move) is covered by one place, and a
// change undone before any sync ran (a failed attach's restore) clears
// nothing.
const originPositionURLKey = "origin_position_url"

// originIdentity is the part of an origin URL that names WHICH origin it is.
// These spellings of one origin give one identity:
//   - credentials (user info, which may hold a token) are dropped, also from a
//     URL that does not parse;
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
			// Unparseable: drop the user info by hand, never keep it.
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
		return "ssh://" + strings.ToLower(hostPart) + cleanOriginPath(p)
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

// stripUserinfo removes everything between "://" (or the start, for the scp
// form) and the last "@" before the path, so no credential survives even in
// a string url.Parse refuses.
func stripUserinfo(s string) string {
	prefix, rest := "", s
	if i := strings.Index(s, "://"); i >= 0 {
		prefix, rest = s[:i+3], s[i+3:]
	}
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	if at := strings.LastIndex(rest[:end], "@"); at >= 0 {
		rest = rest[at+1:]
	}
	return prefix + rest
}

// originPositionForgottenKey marks that the position was forgotten and no
// reconcile has completed since. While it is set, the rewind replay keeps
// only this instance's own commits (unpushedCommitsWalk's own-only mode):
// with no watermark, the walk would otherwise also pass what the OLD origin
// wrote and push it to the new one as this copy's work. It is set BEFORE the
// watermarks are deleted and cleared only after a Sync's reconcile succeeded,
// so a round that fails in between leaves it set for the next one.
const originPositionForgottenKey = "origin_position_forgotten"

// setOriginPositionForgotten sets or clears the forgotten mark.
func (rh *repoHandler) setOriginPositionForgotten(on bool) error {
	var err error
	if on {
		_, err = rh.db.Exec(`INSERT OR REPLACE INTO meta(key, value) VALUES (?, '1')`, originPositionForgottenKey)
	} else {
		_, err = rh.db.Exec(`DELETE FROM meta WHERE key = ?`, originPositionForgottenKey)
	}
	if err != nil {
		return fmt.Errorf("origin position forgotten mark: %w", err)
	}
	return nil
}

// originPositionForgotten reports the forgotten mark.
func (rh *repoHandler) originPositionForgotten() (bool, error) {
	var v string
	err := rh.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, originPositionForgottenKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read origin position forgotten mark: %w", err)
	}
	return true, nil
}

// readOriginPositionURL returns the origin URL the position is recorded for,
// "" when none is recorded (a repository made before the tag existed).
func (rh *repoHandler) readOriginPositionURL() (string, error) {
	var recorded string
	err := rh.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, originPositionURLKey).Scan(&recorded)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read origin position url: %w", err)
	}
	return recorded, nil
}

// recordOriginPositionURL records originURL as the origin the position belongs to.
func (rh *repoHandler) recordOriginPositionURL(originURL string) error {
	if _, err := rh.db.Exec(
		`INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`, originPositionURLKey, originIdentity(originURL)); err != nil {
		return fmt.Errorf("record origin position url: %w", err)
	}
	return nil
}

// adoptOriginPositionURL records url only when no URL is recorded yet: the
// position a repository already has is taken to be its current origin's.
// Service.SetOrigin calls it, so a repository opened with its stored origin is
// tagged before any later change of URL can reach a Sync. Never overwrites.
//
// A failed write is logged, not returned: the next Sync adopts the URL it
// syncs with when nothing is recorded.
func (rh *repoHandler) adoptOriginPositionURL(originURL string) {
	if _, err := rh.db.ExecContext(context.Background(),
		`INSERT OR IGNORE INTO meta(key, value) VALUES (?, ?)`, originPositionURLKey, originIdentity(originURL)); err != nil {
		log.Warn().Err(err).Msg("SetOrigin: tagging the origin position failed; the next Sync tags it")
	}
}

// originPositionStale reports whether the recorded position belongs to an
// origin other than originURL. With nothing recorded it adopts originURL and
// reports false.
func (rh *repoHandler) originPositionStale(originURL string) (bool, error) {
	recorded, err := rh.readOriginPositionURL()
	if err != nil {
		return false, err
	}
	if recorded == "" {
		return false, rh.recordOriginPositionURL(originURL)
	}
	return recorded != originIdentity(originURL), nil
}

// forgetWatermarks deletes every refs/knomit/agent-base/* ref. Takes
// rh.lockBranch(agentBranch) when one is given: the reconcile writes that
// branch's watermark under it.
func (rh *repoHandler) forgetWatermarks(agentBranch string) (int, error) {
	if agentBranch != "" {
		defer rh.lockBranch(agentBranch)()
	}
	return rh.removeRefsWithPrefix("refs/knomit/agent-base/")
}

// forgetTrackingRefs deletes every refs/remotes/origin/* ref (the consensus
// branch, the agent branch and, on a fleet master, the members' branches).
// The caller holds rh.configMu (the fetch writes these refs under it).
func (rh *repoHandler) forgetTrackingRefs() (int, error) {
	return rh.removeRefsWithPrefix("refs/remotes/origin/")
}

func (rh *repoHandler) removeRefsWithPrefix(prefix string) (int, error) {
	iter, err := rh.gits.IterReferences()
	if err != nil {
		return 0, fmt.Errorf("list refs: %w", err)
	}
	var names []plumbing.ReferenceName
	if err := iter.ForEach(func(ref *plumbing.Reference) error {
		if strings.HasPrefix(ref.Name().String(), prefix) {
			names = append(names, ref.Name())
		}
		return nil
	}); err != nil {
		return 0, fmt.Errorf("list refs: %w", err)
	}
	for _, n := range names {
		if err := rh.gits.RemoveReference(n); err != nil {
			return 0, fmt.Errorf("remove %s: %w", n, err)
		}
	}
	return len(names), nil
}

// logForgotOriginPosition reports a forgotten position once per Sync.
func logForgotOriginPosition(originURL string, watermarks, tracking int) {
	log.Info().Str("origin", originIdentity(originURL)).Int("watermarks", watermarks).Int("tracking_refs", tracking).
		Msg("Sync: the origin URL changed; forgot the previous origin's position, this sync is a first sync")
}
