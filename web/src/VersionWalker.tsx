import { useEffect, useState } from 'react';
import { api } from './api';
import { HistoryIcon } from './icons';
import { FACT_ACTION_REST } from './utils';

interface Entry {
  commit: string;
  date: string;
  message: string;
}

interface Props {
  repo: string;
  branch: string;
  factPath: string;
  onScrub: (commit: string) => void;
}

// A single always-available control that opens the fact's history: a JUMP
// POINT, nothing more (user, 2026-11-08). Clicking it enters history mode
// anchored at the fact's NEWEST version commit, which rotates the left rail to
// the TimelineNav version picker. There is no prev/next stepping — the
// timeline is the picker. Rendered for single-version facts too, so their
// history (and the other facts in the same commit) is reachable.
//
// It counts nothing. It used to read "v{N}", N being the length of the
// /commits list: that list includes merge commits, commits from side
// branches and deletes, and is capped at 50, so N was not the number of
// versions. The glyph alone is the button; aria-label names it.
//
// The glyph is HistoryIcon, an SVG at the materialize button's size (15) and
// stroke weight. It was the text arrow ↺ at 10px until the chat review of
// 2026-11-08, which read as a small hook beside the newspaper glyph.
//
// The fetch stays: it is how the newest version commit is found. The fact read
// does not carry it (a live read's as_of.commit is the branch tip, an anchored
// read's is the anchor), and the fetch also hides the button for a fact with
// no recorded versions.
/** The history glyph's size: the materialize (newspaper) glyph's, so the two match. */
const HISTORY_ICON_SIZE = 15;

export function VersionWalker({ repo, branch, factPath, onScrub }: Props) {
  const [entries, setEntries] = useState<Entry[]>([]);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    api.factCommits(repo, branch, factPath)
      .then(r => {
        if (!cancelled) {
          setEntries(r.entries || []);
          setLoading(false);
        }
      })
      .catch(() => { if (!cancelled) { setEntries([]); setLoading(false); } });
    return () => { cancelled = true; };
  }, [repo, branch, factPath]);

  if (loading) return null;
  // Nothing to anchor at for a fact with no version commits.
  if (entries.length === 0) return null;

  // Open history at the newest version commit. Scrubbing always enters history
  // mode — returning to live is the timeline's return-to-live control.
  const openHistory = () => onScrub(entries[0].commit);

  return (
    <button
      data-testid="version-walker"
      onClick={openHistory}
      title="Open history"
      aria-label="Open history"
      // Styled exactly as the Materialize button beside it (FactChrome): muted
      // at rest, the accent only on hover, through the shared .k-materialize
      // rule (App.css), and .k-bare's ring for keyboard focus only. It used to
      // be the accent (#7c9) at rest, an inline colour that also outranked any
      // :hover; and an inline `outline: none` that hid the keyboard ring too.
      className="k-bare k-materialize"
      style={{
        display: 'inline-flex', alignItems: 'center', gap: 4,
        // NO BORDER OF ITS OWN. This sits inside the fact header's control
        // strip, which draws the border and the dividers; a chip border here
        // would nest one inside the other. The strip's cells are separated by
        // hairlines, not by each child re-stating its own outline. No inline
        // border-radius either: it outranked .k-bare:focus-visible's 4px and
        // squared the keyboard ring that Materialize draws rounded.
        background: 'none', border: 'none',
        padding: 2, cursor: 'pointer',
        ['--k-materialize-rest' as string]: FACT_ACTION_REST, opacity: 0.75,
      }}
      onMouseEnter={e => { e.currentTarget.style.opacity = '1'; }}
      onMouseLeave={e => { e.currentTarget.style.opacity = '0.75'; }}
    >
      <HistoryIcon color="currentColor" size={HISTORY_ICON_SIZE} />
    </button>
  );
}
