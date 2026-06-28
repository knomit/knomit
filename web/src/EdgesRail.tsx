import { useState, useEffect, useLayoutEffect, useRef } from 'react';
import { createPortal } from 'react-dom';
import { api } from './api';
import type { RefGroup, RefVersion } from './api';
import { relativeTimeEpoch, typeStyles } from './utils';
import { useDismiss } from './hooks';
import { TypeIcon, ChevronDownIcon } from './icons';

// Diagonal hatch overlay marking a retracted/deleted edge target. Shared by the
// list rows and the multi-version chip so the "deleted" treatment is identical.
const RETRACTED_HATCH = 'repeating-linear-gradient(45deg, rgba(255,255,255,0.08) 0 1px, transparent 1px 6px)';

interface Props {
  repo: string;
  branch: string;
  factPath: string;
  anchorCommit: string;
  history: boolean;
  onHop: (path: string, pinnedCommit: string) => void;
}

export function EdgesRail({ repo, branch, factPath, anchorCommit, history, onHop }: Props) {
  const [incoming, setIncoming] = useState<RefGroup[]>([]);
  const [outgoing, setOutgoing] = useState<RefGroup[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);
    setIncoming([]);
    setOutgoing([]);

    const opts = history ? { fallback: 'before' as const } : undefined;
    api.explain(repo, branch, factPath, anchorCommit, opts)
      .then(e => {
        if (cancelled) return;
        setIncoming(e.incoming);
        setOutgoing(e.outgoing);
      })
      .catch(err => { if (!cancelled) setError(String(err)); })
      .finally(() => { if (!cancelled) setLoading(false); });

    return () => { cancelled = true; };
  }, [repo, branch, factPath, anchorCommit, history]);

  const handleHop = (group: RefGroup, commit: string) => {
    onHop(group.path, commit);
  };

  return (
    <div style={{
      width: 300,
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
}

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

function EdgeRow({ group, onHop }: {
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
      <div style={{
        padding: '6px 12px',
        borderTop: '1px solid #1a1a1a',
        background: deleted ? `${hatch}, transparent` : 'transparent',
      }}>
        <Chip group={group} onClick={(commit) => onHop(group, commit)} />
      </div>
    );
  }

  return (
    <div
      onClick={handleClick}
      style={{
        display: 'flex',
        gap: 8,
        padding: '8px 12px',
        alignItems: 'flex-start',
        borderTop: '1px solid #1a1a1a',
        background: deleted ? `${hatch}, transparent` : 'transparent',
        cursor: 'pointer',
        opacity: deleted ? 0.7 : 1,
      }}
      onMouseEnter={e => {
        (e.currentTarget as HTMLElement).style.background = deleted ? `${hatch}, #111` : '#111';
      }}
      onMouseLeave={e => {
        (e.currentTarget as HTMLElement).style.background = deleted ? `${hatch}, transparent` : 'transparent';
      }}
    >
      {groupType && (
        <span style={{ marginTop: 2 }}>
          <TypeIcon type={groupType} color={typeColor} size={11} />
        </span>
      )}
      <div style={{ minWidth: 0, flex: 1 }}>
        <div style={{
          fontSize: 11.5,
          color: deleted ? '#777' : '#ddd',
          lineHeight: 1.3,
          overflow: 'hidden',
          textOverflow: 'ellipsis',
          whiteSpace: 'nowrap',
          textDecoration: deleted ? 'line-through' : 'none',
        }}>
          {group.title || group.path}
        </div>
        <div style={{ marginTop: 3, display: 'flex', alignItems: 'center', gap: 6 }}>
          <span style={{
            fontFamily: 'var(--k-font-mono)',
            fontSize: 9,
            color: '#444',
            overflow: 'hidden',
            textOverflow: 'ellipsis',
            whiteSpace: 'nowrap',
            flex: 1,
          }}>{group.path}</span>
          {latest?.commit && (
            <span style={{
              fontSize: 9,
              fontFamily: 'var(--k-font-mono)',
              color: deleted ? '#f88' : typeColor,
              background: '#1a1a2a',
              padding: '0 4px',
              borderRadius: 2,
              flexShrink: 0,
            }}>{deleted ? 'retracted' : latest.commit.slice(0, 7)}</span>
          )}
        </div>
      </div>
    </div>
  );
}

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
