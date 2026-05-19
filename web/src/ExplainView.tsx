import { useState, useEffect, useLayoutEffect, useRef } from 'react';
import { createPortal } from 'react-dom';
import { api } from './api';
import type { Fact, RefGroup, RefVersion } from './api';
import { relativeTimeEpoch, typeStyles } from './utils';
import { TypeIcon } from './icons';
import { FactBody } from './FactBody';
import { FactHistoryPanel } from './FactHistoryPanel';
import type { ExplainEntry } from './state';

interface Props {
  repo: string;
  branch: string;
  initialEntry: ExplainEntry;
  onClose: () => void;
}

const MIN_PANEL_WIDTH = 200;
const MAX_PANEL_WIDTH = 800;
const DEFAULT_PANEL_WIDTH = 380;

export function ExplainView({ repo, branch, initialEntry, onClose }: Props) {
  const [current, setCurrent] = useState<ExplainEntry>(initialEntry);
  const [backStack, setBackStack] = useState<ExplainEntry[]>([]);
  const [fact, setFact] = useState<Fact | null>(null);
  const [incoming, setIncoming] = useState<RefGroup[]>([]);
  const [outgoing, setOutgoing] = useState<RefGroup[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [panelWidth, setPanelWidth] = useState(DEFAULT_PANEL_WIDTH);

  const startResize = (e: React.MouseEvent) => {
    e.preventDefault();
    const startX = e.clientX;
    const startWidth = panelWidth;
    const onMove = (ev: MouseEvent) => {
      // Dragging left grows the panel (it's on the right edge of the layout).
      const next = startWidth + (startX - ev.clientX);
      setPanelWidth(Math.max(MIN_PANEL_WIDTH, Math.min(MAX_PANEL_WIDTH, next)));
    };
    const onUp = () => {
      document.removeEventListener('mousemove', onMove);
      document.removeEventListener('mouseup', onUp);
    };
    document.addEventListener('mousemove', onMove);
    document.addEventListener('mouseup', onUp);
  };

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);
    setFact(null);
    setIncoming([]);
    setOutgoing([]);

    const factPromise = api.fact(repo, branch, current.path, current.commit ?? undefined, { fallback: 'before' });
    const explainPromise = api.explain(repo, branch, current.path, current.commit ?? undefined);

    Promise.all([factPromise, explainPromise])
      .then(([f, e]) => {
        if (cancelled) return;
        setFact(f);
        setIncoming(e.incoming);
        setOutgoing(e.outgoing);
      })
      .catch(err => { if (!cancelled) setError(String(err)); })
      .finally(() => { if (!cancelled) setLoading(false); });

    return () => { cancelled = true; };
  }, [current, repo, branch]);

  const navigateTo = (entry: ExplainEntry) => {
    setBackStack(prev => [...prev, current]);
    setCurrent(entry);
  };

  const goBack = () => {
    if (backStack.length === 0) return;
    setCurrent(backStack[backStack.length - 1]);
    setBackStack(prev => prev.slice(0, -1));
  };

  // Escape closes the view. The history panel renders the close affordance
  // visually; this handler makes the key shortcut actually work.
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose();
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [onClose]);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%', background: '#0a0a0a' }}>
      {/* Header bar: EXPLAIN <path> @ <commit>, with back + close on the right. */}
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '0 12px', height: 32, background: '#0f0f0f', borderBottom: '1px solid #1a1a1a', flexShrink: 0 }}>
        <span style={{ fontSize: 10, color: '#444', textTransform: 'uppercase', letterSpacing: '0.08em', flexShrink: 0 }}>Explain</span>
        <span style={{ fontFamily: 'monospace', fontSize: 11, color: '#888', minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{current.path}</span>
        {fact?.commit_hash && (
          <>
            <span style={{ color: '#444', fontSize: 11, flexShrink: 0 }}>@</span>
            <span
              data-testid="explain-commit-chip"
              style={{
                color: '#7c9', background: '#1a2e1a', padding: '1px 6px', borderRadius: 3,
                fontFamily: 'monospace', fontSize: 11, flexShrink: 0,
              }}
            >{fact.commit_hash.slice(0, 7)}</span>
          </>
        )}
        <div style={{ flex: 1 }} />
        {backStack.length > 0 && (
          <button
            data-testid="explain-back"
            onClick={goBack}
            title="Back"
            style={{ background: 'none', border: 'none', color: '#888', fontSize: 14, padding: '2px 6px', cursor: 'pointer', lineHeight: 1 }}
            onMouseEnter={e => { (e.currentTarget as HTMLElement).style.color = '#ccc'; }}
            onMouseLeave={e => { (e.currentTarget as HTMLElement).style.color = '#888'; }}
          >←</button>
        )}
        <button
          data-testid="explain-close"
          onClick={onClose}
          title="Close (esc)"
          style={{ background: 'none', border: 'none', color: '#888', fontSize: 14, padding: '2px 6px', cursor: 'pointer', lineHeight: 1 }}
          onMouseEnter={e => { (e.currentTarget as HTMLElement).style.color = '#ccc'; }}
          onMouseLeave={e => { (e.currentTarget as HTMLElement).style.color = '#888'; }}
        >✕</button>
      </div>

      {/* Incoming refs strip */}
      <div style={{ flexShrink: 0, borderBottom: '1px solid #1a1a1a', background: '#0d0d0d' }}>
        <RailHeader direction="in" groups={incoming} testId="incoming-header" />
        <div style={{ padding: '6px 12px', minHeight: 38, display: 'flex', flexWrap: 'nowrap', gap: 6, alignItems: 'center', overflowX: 'auto', overflowY: 'hidden' }}>
          {incoming.length === 0 && !loading && <span style={{ fontSize: 11, color: '#2a2a2a' }}>none</span>}
          {incoming.map(g => (
            <Chip key={g.path} group={g} onClick={commit => navigateTo({ path: g.path, commit })} />
          ))}
        </div>
      </div>

      {/* Middle: fact body (left) + history panel (right) */}
      <div style={{ flex: 1, display: 'flex', minHeight: 0, overflow: 'hidden' }}>
        <div style={{ flex: 1, overflowY: 'auto', padding: '16px 20px', minWidth: 0 }}>
          {loading && <div style={{ color: '#444', fontSize: 12 }}>Loading…</div>}
          {error && <div style={{ color: '#f66', fontSize: 12 }}>{error}</div>}
          {fact && (
            <div style={{ maxWidth: 720, margin: '0 auto' }}>
              <div data-testid="fact-title" style={{ fontSize: 18, fontWeight: 600, color: '#eee', letterSpacing: '-0.3px', marginBottom: 14 }}>{fact.title || fact.path}</div>
              <FactBody
                fact={fact}
                dispatch={() => {}}
                readOnly={true}
                onRefClick={(refPath) => navigateTo({ path: refPath, commit: current.commit })}
              />
            </div>
          )}
        </div>
        <div style={{ width: panelWidth, flexShrink: 0, position: 'relative' }}>
          <div
            data-testid="history-resize-handle"
            onMouseDown={startResize}
            title="Drag to resize"
            style={{
              position: 'absolute', left: -2, top: 0, bottom: 0, width: 5,
              cursor: 'ew-resize', zIndex: 1,
            }}
          />
          <FactHistoryPanel
            repo={repo}
            branch={branch}
            factPath={current.path}
            currentCommit={fact?.commit_hash ?? current.commit ?? null}
            onNavigateToCommit={(commit) => navigateTo({ path: current.path, commit })}
            onFileClick={(path) => navigateTo({ path, commit: fact?.commit_hash ?? current.commit ?? null })}
          />
        </div>
      </div>

      {/* Outgoing refs strip */}
      <div style={{ flexShrink: 0, borderTop: '1px solid #1a1a1a', background: '#0d0d0d' }}>
        <RailHeader direction="out" groups={outgoing} testId="outgoing-header" />
        <div style={{ padding: '6px 12px', minHeight: 38, display: 'flex', flexWrap: 'nowrap', gap: 6, alignItems: 'center', overflowX: 'auto', overflowY: 'hidden' }}>
          {outgoing.length === 0 && !loading && <span style={{ fontSize: 11, color: '#2a2a2a' }}>none</span>}
          {outgoing.map(g => (
            <Chip
              key={g.path}
              group={g}
              onClick={commit => navigateTo({ path: g.path, commit })}
            />
          ))}
        </div>
      </div>
    </div>
  );
}

function RailHeader({ direction, groups, testId }: {
  direction: 'in' | 'out';
  groups: RefGroup[];
  testId: string;
}) {
  const total = groups.length;
  const retracted = groups.filter(g => g.deleted).length;

  // Per-type breakdown, ordered by descending count, ties broken by name.
  const counts = new Map<string, number>();
  for (const g of groups) {
    const t = g.type ?? g.versions[0]?.type;
    if (!t) continue;
    counts.set(t, (counts.get(t) ?? 0) + 1);
  }
  const breakdown = [...counts.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]));

  const arrow = direction === 'in' ? '↙' : '↗';
  const sectionLabel = direction === 'in' ? 'IN-EDGES · REFERENCED BY' : 'OUT-EDGES · REFERENCES';

  return (
    <div data-testid={testId} style={{
      display: 'flex', alignItems: 'center', gap: 10, padding: '6px 12px',
      background: '#0d0d0d',
      fontSize: 10, color: '#3a3a3a', textTransform: 'uppercase', letterSpacing: '0.08em',
      flexShrink: 0,
    }}>
      <span style={{ color: '#888' }}>{arrow} {sectionLabel} <span style={{ color: '#ccc' }}>{total}</span></span>
      <span style={{ display: 'flex', gap: 8, alignItems: 'center', flex: 1, overflow: 'hidden', flexWrap: 'wrap' }}>
        {breakdown.map(([t, n]) => {
          const ts = typeStyles[t];
          if (!ts) return null;
          return (
            <span key={t} style={{ color: ts.color, fontFamily: 'monospace', textTransform: 'lowercase' }}>
              {t} <span style={{ color: '#aaa' }}>{n}</span>
            </span>
          );
        })}
      </span>
      {retracted > 0 && (
        <span style={{ color: '#f88', fontFamily: 'monospace' }}>{retracted} retracted</span>
      )}
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

  // Anchor the dropdown to the chip's viewport position. We render via a
  // portal so the rail's overflow:auto doesn't clip the dropdown.
  useLayoutEffect(() => {
    if (!open || !chipRef.current) { setDropdownPos(null); return; }
    const r = chipRef.current.getBoundingClientRect();
    setDropdownPos({ top: r.bottom + 2, left: r.left });
  }, [open]);

  // Outside-click + Escape close the dropdown.
  useEffect(() => {
    if (!open) return;
    const onMouseDown = (e: MouseEvent) => {
      const target = e.target as Node | null;
      if (!target) return;
      if (dropdownRef.current?.contains(target)) return;
      if (chipRef.current?.contains(target)) return;
      setOpen(false);
    };
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setOpen(false);
    };
    document.addEventListener('mousedown', onMouseDown);
    window.addEventListener('keydown', onKeyDown);
    return () => {
      document.removeEventListener('mousedown', onMouseDown);
      window.removeEventListener('keydown', onKeyDown);
    };
  }, [open]);

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

  // Retracted chips render with the same colors and text weight as live chips
  // (keeps them readable), then a subtle ~10%-alpha diagonal hatch overlays
  // the background so the "retracted" status is visible without shouting.
  const hatch = 'repeating-linear-gradient(45deg, rgba(255,255,255,0.10) 0 1px, transparent 1px 5px)';
  return (
    <span
      ref={chipRef}
      data-testid="ref-chip"
      data-deleted={deleted ? 'true' : undefined}
      onClick={handleChipClick}
      title={deleted ? 'Target fact retracted.' : group.path}
      style={{
        display: 'inline-flex', flexDirection: 'column',
        padding: '4px 9px', borderRadius: 8,
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
        <span style={{ fontSize: 10, color: '#444', fontFamily: 'monospace', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', flex: 1 }}>{group.path}</span>
        {isMulti ? (
          <span style={{
            fontFamily: 'monospace', fontSize: 9, color: typeColor,
            background: '#1a1a2a', padding: '0 4px', borderRadius: 2,
            flexShrink: 0,
          }}>×{versionCount} ⌄</span>
        ) : (
          latest?.commit && (
            <span style={{
              fontFamily: 'monospace', fontSize: 9, color: typeColor,
              background: '#1a1a2a', padding: '0 4px', borderRadius: 2,
              textDecoration: 'none',
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
            textDecoration: 'none',
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
              <span style={{ fontFamily: 'monospace', fontSize: 10, color: typeColor }}>
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

