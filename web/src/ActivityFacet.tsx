import { useState } from 'react';
import type { CSSProperties } from 'react';
import { FacetToolbar } from './FacetToolbar';
import { SessionsTable } from './SessionsTable';
import { useTargetSessions, readShowHidden, writeShowHidden } from './useTargetSessions';
import type { SessionTarget } from './useTargetSessions';

// ActivityFacet is a target's Activity page: the MCP client sessions bound to
// THIS repo or lens, filtered server-side by `?binding=repo:<uid>|lens:<uid>`.
// Sessions bound to nothing are listed nowhere any more, which the spec
// accepts.
//
// The toolbar carries what used to be the card caption: "Sessions", the
// counts, the truncation note, and Show hidden. The table fills the body below
// it and scrolls inside, with the policy line as its footer.
//
// One sessions stream is open while this is mounted (the hook's), and none
// once it unmounts.
export function ActivityFacet({ target }: { target: SessionTarget }) {
  const [showHidden, setShowHidden] = useState(readShowHidden);
  const { rows, truncated, policy, error, now } = useTargetSessions(target, showHidden);
  const live = rows.filter(r => r.state === 'live').length;

  return (
    <div data-testid="activity-facet" style={body}>
      <FacetToolbar testId="activity-toolbar">
        <span style={title}>Sessions</span>
        <span data-testid="activity-counts" style={counts}>{live} live · {rows.length} shown</span>
        {/* A count from a bounded page: when the server cut the list, the
            numbers above are of the page, and this says so. Amber, not red:
            the page is correct, just partial. */}
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
      </FacetToolbar>
      <SessionsTable rows={rows} policy={policy} error={error} now={now} target={target} padded />
    </div>
  );
}

// flex '1 1 0' + minHeight 0: the facet body is bounded by the column it sits
// in, so the table scrolls inside it and the footer stays on screen.
const body: CSSProperties = { flex: '1 1 0', minHeight: 0, display: 'flex', flexDirection: 'column', overflow: 'hidden' };
const title: CSSProperties = { color: '#ddd', fontWeight: 500, fontSize: 12 };
const counts: CSSProperties = { fontFamily: 'var(--k-font-mono)', fontSize: 11, color: '#888' };
const truncatedNote: CSSProperties = { color: '#facc15', fontSize: 11 };
// The gap is the only thing separating the box from its words (App.css resets
// margins), the same rule ManageSessions' label follows.
const showHiddenLabel: CSSProperties = {
  marginLeft: 'auto', display: 'flex', alignItems: 'center', gap: 6, color: '#aaa', fontSize: 12,
};
