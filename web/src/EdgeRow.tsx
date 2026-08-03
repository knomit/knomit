import { memo, useState, useLayoutEffect, useRef } from 'react';
import { createPortal } from 'react-dom';
import type { RefGroup, RefVersion } from './api';
import { relativeTimeEpoch, typeStyles } from './utils';
import { useDismiss } from './hooks';
import { TypeIcon } from './icons';

// Diagonal hatch overlay marking a retracted/deleted edge target. Shared by the
// list rows and the multi-version chip so the "deleted" treatment is identical.
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
  // For single-version groups, clicking the row directly hops.
  // For multi-version groups, we use a Chip with dropdown.
  const latest = group.versions[0];
  const isMulti = group.versions.length > 1;
  const deleted = group.deleted ?? false;
  const groupType = group.type ?? latest?.type;
  const typeColor = (groupType && typeStyles[groupType]?.color) || '#253565';
  const hatch = RETRACTED_HATCH;

  const handleClick = () => {
    if (!isMulti && latest) {
      onHop(group, latest.commit);
    }
  };

  if (isMulti) {
    return (
      <div style={deleted ? rowMultiDeleted : rowMulti}>
        <Chip group={group} onClick={(commit) => onHop(group, commit)} />
      </div>
    );
  }

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
          {latest?.commit && (
            <span style={{ ...rowBadge, color: deleted ? '#f88' : typeColor }}>
              {deleted ? 'retracted' : latest.commit.slice(0, 7)}
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
const rowMulti: React.CSSProperties = { padding: '6px 12px', borderTop: '1px solid #1a1a1a', background: 'transparent' };
const rowMultiDeleted: React.CSSProperties = { ...rowMulti, background: `${RETRACTED_HATCH}, transparent` };
const row: React.CSSProperties = { display: 'flex', gap: 8, padding: '8px 12px', alignItems: 'flex-start', borderTop: '1px solid #1a1a1a', background: 'transparent', cursor: 'pointer', opacity: 1 };
const rowDeleted: React.CSSProperties = { ...row, background: `${RETRACTED_HATCH}, transparent`, opacity: 0.7 };
const rowIcon: React.CSSProperties = { marginTop: 2 };
const rowBody: React.CSSProperties = { minWidth: 0, flex: 1 };
const rowTitle: React.CSSProperties = { fontSize: 11.5, color: '#ddd', lineHeight: 1.3, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', textDecoration: 'none' };
const rowTitleDeleted: React.CSSProperties = { ...rowTitle, color: '#777', textDecoration: 'line-through' };
const rowMeta: React.CSSProperties = { marginTop: 3, display: 'flex', alignItems: 'center', gap: 6 };
const rowPath: React.CSSProperties = { fontFamily: 'var(--k-font-mono)', fontSize: 9, color: '#444', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', flex: 1 };
const rowBadge: React.CSSProperties = { fontSize: 9, fontFamily: 'var(--k-font-mono)', background: '#1a1a2a', padding: '0 4px', borderRadius: 2, flexShrink: 0 };

function Chip({ group, onClick }: { group: RefGroup; onClick: (commit: string) => void }) {
  const [open, setOpen] = useState(false);
  const [dropdownPos, setDropdownPos] = useState<{ top: number; left: number } | null>(null);
  const dropdownRef = useRef<HTMLDivElement | null>(null);
  const chipRef = useRef<HTMLSpanElement | null>(null);

  const versionCount = group.versions.length;
  const isMulti = versionCount > 1;
  const deleted = group.deleted ?? false;
  const latest = group.versions[0];
  const groupType = group.type ?? latest?.type;
  const typeColor = (groupType && typeStyles[groupType]?.color) || '#253565';

  useLayoutEffect(() => {
    if (!open || !chipRef.current) { setDropdownPos(null); return; }
    const r = chipRef.current.getBoundingClientRect();
    setDropdownPos({ top: r.bottom + 2, left: r.left });
  }, [open]);

  useDismiss(open, () => setOpen(false), [dropdownRef, chipRef]);

  const handleChipClick = () => {
    if (isMulti) {
      setOpen(o => !o);
      return;
    }
    if (latest) onClick(latest.commit);
  };

  const handleRowClick = (version: RefVersion) => {
    setOpen(false);
    onClick(version.commit);
  };

  const hatch = RETRACTED_HATCH;

  return (
    <span
      ref={chipRef}
      data-testid="ref-chip"
      data-deleted={deleted ? 'true' : undefined}
      onClick={handleChipClick}
      title={deleted ? 'Target fact retracted.' : group.path}
      style={{
        display: 'inline-flex',
        flexDirection: 'column',
        padding: '4px 9px',
        borderRadius: 8,
        border: `1px solid ${typeColor}`,
        cursor: 'pointer',
        background: deleted ? `${hatch}, #111` : '#111',
        // Fills its row. The 220px cap was proportionate in the 300px rail this
        // came from; in the drawer it left a bordered card floating in half the
        // width with the title truncated for no reason, reading as a different
        // kind of thing from the flat rows around it. The border stays — it is
        // what says "this target has several versions, pick one".
        width: '100%',
        boxSizing: 'border-box',
        flexShrink: 0,
        position: 'relative',
      }}
    >
      <span style={{ display: 'flex', alignItems: 'center', gap: 6, overflow: 'hidden' }}>
        {groupType && <TypeIcon type={groupType} color={typeColor} size={12} />}
        <span style={{ fontSize: 12, color: '#ddd', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
          {group.title || group.path}
        </span>
      </span>
      <span style={{ display: 'flex', alignItems: 'center', gap: 6, overflow: 'hidden', marginTop: 2 }}>
        <span style={{ fontSize: 10, color: '#444', fontFamily: 'var(--k-font-mono)', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', flex: 1 }}>{group.path}</span>
        {isMulti ? (
          <span style={{
            fontFamily: 'var(--k-font-mono)', fontSize: 9, color: typeColor,
            background: '#1a1a2a', padding: '0 4px', borderRadius: 2,
            flexShrink: 0,
          }}>×{versionCount} ⌄</span>
        ) : (
          latest?.commit && (
            <span style={{
              fontFamily: 'var(--k-font-mono)', fontSize: 9, color: typeColor,
              background: '#1a1a2a', padding: '0 4px', borderRadius: 2,
              flexShrink: 0,
            }}>{latest.commit.slice(0, 7)}</span>
          )
        )}
      </span>
      {open && isMulti && dropdownPos && createPortal(
        <div
          ref={dropdownRef}
          // Rendered into document.body, so it is NOT inside the drawer's DOM
          // subtree: moving the pointer into it fires the drawer's mouseleave.
          // The drawer checks for this attribute before hover-closing, or
          // choosing a version would dismiss the panel you chose it from.
          data-connections-portal=""
          onClick={e => e.stopPropagation()}
          style={{
            position: 'fixed',
            top: dropdownPos.top,
            left: dropdownPos.left,
            minWidth: 180,
            maxHeight: 200,
            overflowY: 'auto',
            background: '#111',
            border: '1px solid #2a2a2a',
            borderRadius: 4,
            padding: '4px 0',
            zIndex: 1000,
          }}
        >
          {group.versions.map((v, idx) => (
            <div
              key={`${v.commit}-${idx}`}
              onClick={() => handleRowClick(v)}
              onMouseEnter={e => (e.currentTarget.style.background = '#1a1a1a')}
              onMouseLeave={e => (e.currentTarget.style.background = 'transparent')}
              style={{
                display: 'flex',
                alignItems: 'center',
                gap: 8,
                padding: '6px 12px',
                cursor: 'pointer',
                background: 'transparent',
              }}
            >
              <span style={{ fontSize: 10, color: idx === 0 ? typeColor : '#444' }}>
                {idx === 0 ? '●' : '○'}
              </span>
              <span style={{ fontFamily: 'var(--k-font-mono)', fontSize: 10, color: typeColor }}>
                {v.commit.slice(0, 7)}
              </span>
              <span style={{ fontSize: 10, color: '#666', marginLeft: 'auto' }}>
                {relativeTimeEpoch(v.committed_at ?? 0)}
              </span>
            </div>
          ))}
        </div>,
        document.body
      )}
    </span>
  );
}
