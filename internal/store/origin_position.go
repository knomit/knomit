package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
// Service.SetOrigin), not the derived git URL: a credential change keeps the
// URL and keeps the position. The check is lazy, at Sync, so every path that
// changes the origin (PUT origin, the connect wizard, a swap, a detach and
// re-attach, the fleet's follow and move) is covered by one place, and a
// change undone before any sync ran (a failed attach's restore) clears
// nothing.
const originPositionURLKey = "origin_position_url"

// readOriginPositionURL returns the origin URL the position is recorded for,
// "" when none is recorded (a repository made before the tag existed).
func (rh *repoHandler) readOriginPositionURL() (string, error) {
	var url string
	err := rh.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, originPositionURLKey).Scan(&url)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read origin position url: %w", err)
	}
	return url, nil
}

// recordOriginPositionURL records url as the origin the position belongs to.
func (rh *repoHandler) recordOriginPositionURL(url string) error {
	if _, err := rh.db.Exec(
		`INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`, originPositionURLKey, url); err != nil {
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
func (rh *repoHandler) adoptOriginPositionURL(url string) {
	if _, err := rh.db.ExecContext(context.Background(),
		`INSERT OR IGNORE INTO meta(key, value) VALUES (?, ?)`, originPositionURLKey, url); err != nil {
		log.Warn().Err(err).Msg("SetOrigin: tagging the origin position failed; the next Sync tags it")
	}
}

// originPositionStale reports whether the recorded position belongs to an
// origin other than url. With nothing recorded it adopts url and reports
// false.
func (rh *repoHandler) originPositionStale(url string) (bool, error) {
	recorded, err := rh.readOriginPositionURL()
	if err != nil {
		return false, err
	}
	if recorded == "" {
		return false, rh.recordOriginPositionURL(url)
	}
	return recorded != url, nil
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
func logForgotOriginPosition(url string, watermarks, tracking int) {
	log.Info().Str("origin", url).Int("watermarks", watermarks).Int("tracking_refs", tracking).
		Msg("Sync: the origin URL changed; forgot the previous origin's position, this sync is a first sync")
}
