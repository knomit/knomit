import { useState } from 'react';
import type { CSSProperties } from 'react';
import { card, cardLabel } from './manageStyles';
import { useTargetSessions, readShowHidden, writeShowHidden } from './useTargetSessions';
import { SessionsTable } from './SessionsTable';

// ManageSessions is Manage's Sessions tab: EVERY MCP client session the server
// has seen, in a card. It is a bridge for one PR — the Activity facet shows a
// target's own sessions and replaces it — and it is built on the same hook and
// table as Activity (target = null, unfiltered), so there are not two copies.

// Amber rather than red: the page is correct, just partial. Red would read as
// a failure and send someone looking for a broken request.
const truncatedNote: CSSProperties = { marginLeft: 8, color: '#facc15', fontSize: 11 };

export function ManageSessions({ onLiveCount }: {
  /** Reports the live count after every poll, so the Manage tab badge can
   *  ride this page's refresh instead of running a second loop.
   *
   *  `truncated` travels with it because the count is computed from a BOUNDED
   *  page: when the server cut the list, the number is a lower bound, not a
   *  total. A consumer that rendered it as exact would be the silent
   *  truncation this page's own note exists to prevent. */
  onLiveCount?: (n: number | null, truncated?: boolean) => void;
}) {
  // Read in the initializer so the FIRST fetch already carries it. Reading it
  // in an effect instead would render the unremembered list and correct it,
  // flashing the wrong rows.
  const [showHidden, setShowHidden] = useState(readShowHidden);
  const { rows, truncated, policy, error, now } = useTargetSessions(null, showHidden, onLiveCount);
  const live = rows.filter(r => r.state === 'live').length;

  return (
    <div data-testid="manage-sessions" style={page}>
      <div style={sessionsCard}>
        {/* The count and Show hidden ride the card's caption row. The tab strip
            already says "Sessions", so the heading that used to sit above this
            said it twice. Show hidden belongs HERE specifically — it acts on
            the block as a whole, which is the case
            kb/decisions/ui/manage-page/block-affordance-in-heading/b9176d5e.md
            was written for. */}
        <div style={labelRow}>
          MCP clients
          <span style={labelCount}>{live} live · {rows.length} shown</span>
          {/* TRUNCATION IS NOT AN ERROR, but it must not be silent either: the
              page is capped server-side, and a reader who cannot tell a cut
              list from a complete one will read "12 shown" as "12 exist". */}
          {truncated && (
            <span data-testid="session-truncated" style={truncatedNote}>
              showing the {rows.length} most recent of more
            </span>
          )}
          <label style={showHiddenLabel}>
            <input type="checkbox" aria-label="Show hidden" checked={showHidden}
                   onChange={e => { setShowHidden(e.target.checked); writeShowHidden(e.target.checked); }} />
            Show hidden
          </label>
        </div>
        <SessionsTable rows={rows} policy={policy} error={error} now={now} target={null} />
      </div>
    </div>
  );
}

// TABLE_FLOOR is the shortest the table area is allowed to get; CARD_FLOOR adds
// the card's own chrome so the card can always draw itself.
//
// Sized for the WORST case — chrome INCLUDING the error line — rather than made
// conditional on `error`. A conditional floor would resize the table at the
// moment an error appears, and that error arrives asynchronously from a failed
// poll, with no user action behind it: the layout would move under someone at
// the instant they started reading why it broke. The corpus settled this same
// tradeoff the same way in
// kb/decisions/ui/summary-panel/dashboard-quick-search/f3e24c49.md (motif
// layout-shifts-under-cursor), which rejected a placement that "pushes the
// whole dashboard down 34px, so the numbers being read move as the reader
// reaches for the box". The precedent is for the direction, not for the cost
// being free: this floor does pay a few unused pixels, and pays them only below
// ~280px of pane height, in a window nobody works in.
//
// The +150 is NOT just the measured chrome, and should not be trimmed to it.
// Measured at 1400px wide, this card's chrome is 73px (card 270 - table area
// 197), or roughly 97 with the error line present. The remaining ~53 is
// deliberate slack: the caption row wraps at narrow widths — the count and Show
// hidden drop below "MCP clients" — and the toolbar-equivalent grows with it,
// so the true chrome rises as the window narrows and a floor cut to 73 would
// stop containing the card exactly there. Logs' own addend (120 against a
// measured 116) is close to its chrome because its caption cannot wrap the same
// way; the two files look like one rule and are not.
const TABLE_FLOOR = 120;
const CARD_FLOOR = TABLE_FLOOR + 150;
// Fills the detail pane rather than sizing to the rows. detailCol is a
// stretched flex item, so its height is definite and this resolves against its
// content box, leaving the pane's own padding outside.
const page: React.CSSProperties = {
  height: '100%', minHeight: CARD_FLOOR, display: 'flex', flexDirection: 'column',
};
// minHeight:0 is load-bearing for the same reason it is on the Logs card: the
// flex default min-height:auto floors a card at its MIN-CONTENT height, and
// min-content propagates up from the content — the wrapper's own minHeight is a
// floor, not a ceiling, so it does not stop the table's full height travelling
// up. Left at auto the card grows to fit every row and the pane scrolls instead
// of the table. The small-window floor lives on the root instead, where it is
// not fighting flex:1 for the same axis.
//
// And the card must never be given an `overflow` of its own. That degradation
// works by letting the card overflow the root so detailCol scrolls it; a scroll
// container here would absorb exactly that overflow, the root would never
// outgrow the pane, and the floor would stop doing anything — besides nesting a
// second scrollbar inside tableWrap's. This is why the `overflowX: 'auto'` that
// used to sit on this card moved into tableWrap rather than staying put.
const sessionsCard: React.CSSProperties = {
  ...card, marginTop: 0, flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column',
};
// flexShrink:0 on all three pieces of chrome, so tableWrap is the only thing
// that gives when the pane is short.
const labelRow: React.CSSProperties = {
  ...cardLabel, display: 'flex', alignItems: 'center', gap: 10, flexShrink: 0,
};
// Not uppercase or letter-spaced: it reads as prose beside the caption, and at
// cardLabel's 10px the count would be unreadable tracked out.
const labelCount: React.CSSProperties = { textTransform: 'none', letterSpacing: 0 };
// The gap is load-bearing, not tidiness, and it is the ONLY thing separating
// the box from its words. This label used to be plain inline text, where the
// `{' '}` between them rendered as a real space. Making it a flex container to
// centre the box against the words took that space away — whitespace between
// flex children is not rendered — and the checkbox has no margin of its own to
// fall back on, because App.css resets `*` to margin:0. So the two collapsed
// together and, in the desktop WebKit view, the native control's box overlapped
// the "S". Removing this gap re-breaks it; there is no second mechanism.
const showHiddenLabel: React.CSSProperties = {
  marginLeft: 'auto', textTransform: 'none', letterSpacing: 0,
  display: 'flex', alignItems: 'center', gap: 6, color: '#aaa',
};
