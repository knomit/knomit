import { memo } from 'react';
import type { RefGroup } from './api';
import { typeStyles } from './utils';
import { TypeIcon } from './icons';

// Diagonal hatch overlay marking a retracted/deleted edge target.
const RETRACTED_HATCH = 'repeating-linear-gradient(45deg, rgba(255,255,255,0.08) 0 1px, transparent 1px 6px)';

// One row of the connections list: a link to a fact that references this one,
// or that this one references.
//
// Memoized because a list of N rows re-rendered every row whenever its parent
// did (a fact open, a scrub, any App-level state change). `group` comes from
// the edge fetch result, so its identity only moves when the edges actually
// change; callers must pass a stable `onHop` or the memo is inert.
//
// The memo is pinned in EdgeRow.memo.test.tsx AGAINST THIS COMPONENT DIRECTLY,
// never through whatever renders it: a memoized parent never re-renders with
// stable props, and any prop that does get through unmounts the rows outright —
// so a test driving the parent re-renders the rows in every configuration,
// including with this memo deleted, and therefore asserts nothing. That is how
// the previous version of that test came to assert nothing.
export const EdgeRow = memo(function EdgeRow({ group, onHop }: {
  group: RefGroup;
  onHop: (group: RefGroup, commit: string) => void;
}) {
  // THE PIN IS THE EDGE'S target_commit: the version of the target the referrer
  // reasoned over, resolved at index time from the referrer's own commit. If B
  // referenced A at a point when A was at commit 1, this edge says commit 1 —
  // and keeps saying commit 1 after A is rewritten at commit 3. That is the
  // whole of kb/principles/philosophy/historical-not-current, and
  // kb/incidents/ui/ref-resolution/in-body-ref-target-commit names this exact
  // field as the value to hop on.
  //
  // This used to branch: a group carrying more than one entry rendered as a
  // bordered chip with a dropdown of every version. Wrong twice over. It offered
  // a CHOICE where the temporal rule says there is exactly one answer, and it
  // disagreed with the same fact's in-body refs, which already hop on this value
  // via refCommits. It also surfaced backend duplicates as a picker between two
  // identical commits (see .claude/harness/scratch/duplicate-derived-from-edges.md).
  const pinned = group.versions[0];
  const deleted = group.deleted ?? false;
  const groupType = group.type ?? pinned?.type;
  const typeColor = (groupType && typeStyles[groupType]?.color) || '#253565';
  const hatch = RETRACTED_HATCH;

  const handleClick = () => {
    if (pinned) onHop(group, pinned.commit);
  };

  return (
    <div
      onClick={handleClick}
      style={deleted ? rowDeleted : row}
      onMouseEnter={e => {
        (e.currentTarget as HTMLElement).style.background = deleted ? `${hatch}, #111` : '#111';
      }}
      onMouseLeave={e => {
        (e.currentTarget as HTMLElement).style.background = deleted ? `${hatch}, transparent` : 'transparent';
      }}
    >
      {groupType && (
        <span style={rowIcon}>
          <TypeIcon type={groupType} color={typeColor} size={11} />
        </span>
      )}
      <div style={rowBody}>
        <div style={deleted ? rowTitleDeleted : rowTitle}>
          {group.title || group.path}
        </div>
        <div style={rowMeta}>
          <span style={rowPath}>{group.path}</span>
          {pinned?.commit && (
            <span style={{ ...rowBadge, color: deleted ? '#f88' : typeColor }}>
              {deleted ? 'retracted' : pinned.commit.slice(0, 7)}
            </span>
          )}
        </div>
      </div>
    </div>
  );
});

// Row styles, hoisted to module scope. The rail renders one of these per edge,
// and every object here was previously re-allocated on each row on each render.
// Only the commit badge still spreads at render time — its color is the
// per-type hue, the one genuinely dynamic value in the row.
const row: React.CSSProperties = { display: 'flex', gap: 8, padding: '8px 12px', alignItems: 'flex-start', borderTop: '1px solid #1a1a1a', background: 'transparent', cursor: 'pointer', opacity: 1 };
const rowDeleted: React.CSSProperties = { ...row, background: `${RETRACTED_HATCH}, transparent`, opacity: 0.7 };
const rowIcon: React.CSSProperties = { marginTop: 2 };
const rowBody: React.CSSProperties = { minWidth: 0, flex: 1 };
const rowTitle: React.CSSProperties = { fontSize: 11.5, color: '#ddd', lineHeight: 1.3, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', textDecoration: 'none' };
const rowTitleDeleted: React.CSSProperties = { ...rowTitle, color: '#777', textDecoration: 'line-through' };
const rowMeta: React.CSSProperties = { marginTop: 3, display: 'flex', alignItems: 'center', gap: 6 };
const rowPath: React.CSSProperties = { fontFamily: 'var(--k-font-mono)', fontSize: 9, color: '#444', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', flex: 1 };
const rowBadge: React.CSSProperties = { fontSize: 9, fontFamily: 'var(--k-font-mono)', background: '#1a1a2a', padding: '0 4px', borderRadius: 2, flexShrink: 0 };
