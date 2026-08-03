import { memo, useState, useLayoutEffect, useRef, useCallback } from 'react';
import { createPortal } from 'react-dom';
import type { RefGroup, RefVersion } from './api';
import { relativeTimeEpoch, typeStyles } from './utils';
import { useDismiss } from './hooks';
import { TypeIcon, ChevronDownIcon } from './icons';

// Diagonal hatch overlay marking a retracted/deleted edge target. Shared by the
// list rows and the multi-version chip so the "deleted" treatment is identical.
const RETRACTED_HATCH = 'repeating-linear-gradient(45deg, rgba(255,255,255,0.08) 0 1px, transparent 1px 6px)';

interface Props {
  /** Edges for the open fact, fetched once by App (useFactEdges). */
  incoming: RefGroup[];
  outgoing: RefGroup[];
  loading: boolean;
  error: string | null;
  onHop: (path: string, pinnedCommit: string) => void;
}

/**
 * The connections rail.
 *
 * It no longer fetches. App owns the open fact's edges (useFactEdges) because
 * RightPanel needs the same data for in-body ref pins, and the two were issuing
 * identical api.explain calls — same fact, same anchor, same fallback. Sharing
 * one fetch also removed the callback this component used to report "I am
 * empty" back to App: whoever owns the layout now owns the counts directly.
 */
export const EdgesRail = memo(function EdgesRail({ incoming, outgoing, loading, error, onHop }: Props) {
  // Stable identity: EdgeRow is memoized, and an inline arrow here would be a
  // fresh prop on every render, making that memo inert. onHop itself is stable
  // (App passes tt.hopEdge, useCallback'd on [repo, branch, dispatch]).
  const handleHop = useCallback((group: RefGroup, commit: string) => {
    onHop(group.path, commit);
  }, [onHop]);

  return (
    <div style={{
      width: 300,
      // border-box so the 1px left border fits INSIDE the 300px EDGES_RAIL_SLOT
      // the rail is mounted in, rather than overflowing it by a pixel.
      boxSizing: 'border-box',
      flexShrink: 0,
      borderLeft: '1px solid #1f1f26',
      background: '#0a0a0a',
      display: 'flex',
      flexDirection: 'column',
      height: '100%',
      overflow: 'hidden',
    }}>
      {/* Header */}
      <div style={{
        padding: '9px 12px',
        borderBottom: '1px solid #1a1a1a',
        fontSize: 10,
        color: '#555',
        fontFamily: 'var(--k-font-mono)',
        letterSpacing: '0.08em',
        textTransform: 'uppercase',
        flexShrink: 0,
      }}>
        Connections
      </div>

      <div style={{ flex: 1, overflowY: 'auto', minHeight: 0 }}>
        {loading && <div style={{ color: '#444', fontSize: 12, padding: '8px 12px' }}>Loading…</div>}
        {error && <div style={{ color: '#f66', fontSize: 12, padding: '8px 12px' }}>{error}</div>}

        {/* IN group */}
        <EdgeGroup
          dir="in"
          groups={incoming}
          onHop={handleHop}
        />

        {/* OUT group */}
        <EdgeGroup
          dir="out"
          groups={outgoing}
          onHop={handleHop}
        />
      </div>
    </div>
  );
});

function EdgeGroup({ dir, groups, onHop }: {
  dir: 'in' | 'out';
  groups: RefGroup[];
  onHop: (group: RefGroup, commit: string) => void;
}) {
  const [open, setOpen] = useState(true);
  const accent = dir === 'in' ? '#8af' : '#fa8';
  const arrow = dir === 'in' ? '↙' : '↗';
  const label = dir === 'in' ? 'IN · referenced by' : 'OUT · references';
  const liveCount = groups.filter(g => !g.deleted).length;
  const retractedCount = groups.filter(g => g.deleted).length;

  return (
    <div style={{ borderBottom: '1px solid #1a1a1a' }}>
      <div
        onClick={() => setOpen(o => !o)}
        style={{
          cursor: 'pointer',
          display: 'flex',
          alignItems: 'center',
          gap: 8,
          padding: '9px 12px',
          background: '#0d0d0d',
        }}
      >
        <span style={{ color: accent, fontFamily: 'var(--k-font-mono)', fontSize: 10, letterSpacing: '0.06em', textTransform: 'uppercase' }}>
          {arrow} {label}
        </span>
        <span style={{ fontSize: 13, fontWeight: 600, color: accent }}>{liveCount}</span>
        {retractedCount > 0 && (
          <span style={{ fontSize: 9, color: '#f88', fontFamily: 'var(--k-font-mono)' }}>{retractedCount} retracted</span>
        )}
        <span style={{ marginLeft: 'auto', color: '#555', transform: open ? 'none' : 'rotate(-90deg)', transition: 'transform .2s', display: 'flex' }}>
          <ChevronDownIcon color="#555" size={12} />
        </span>
      </div>

      {open && (
        <div>
          {groups.length === 0 && (
            <div style={{ padding: '8px 12px', fontSize: 11, color: '#333' }}>none</div>
          )}
          {groups.map(g => (
            <EdgeRow key={g.path} group={g} onHop={onHop} />
          ))}
        </div>
      )}
    </div>
  );
}

// Memoized: a rail of N edges re-rendered every row whenever the parent
// re-rendered (a fact open, a scrub, any App-level state change). `group` comes
// from the fetch result array, so its identity only moves when the edges
// actually change; `onHop` is stabilized by handleHop above.
//
// Exported for EdgesRail.memo.test.tsx. The memo cannot be pinned THROUGH the
// rail: EdgesRail is itself memoized, so a re-render with stable props never
// reaches the rows, and every prop that does get through either changes
// handleHop or refires the fetch (which clears `incoming` and unmounts the rows
// outright). A test driving the rail therefore re-renders the rows in every
// configuration — including with this memo deleted — which is exactly how the
// previous version of that file came to assert nothing. The memo is pinned
// against EdgeRow directly instead.
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
        maxWidth: 220,
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
