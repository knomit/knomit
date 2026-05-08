import { useState, useEffect, useRef } from 'react';
import { api } from './api';
import type { Fact, RefGroup, RefVersion } from './api';
import { relativeTimeEpoch } from './utils';
import { FactBody } from './FactBody';

interface ExplainEntry { path: string; commit: string | null; }

interface Props {
  repo: string;
  branch: string;
  initialEntry: ExplainEntry;
  onClose: () => void;
}

export function ExplainView({ repo, branch, initialEntry, onClose }: Props) {
  const [current, setCurrent] = useState<ExplainEntry>(initialEntry);
  const [backStack, setBackStack] = useState<ExplainEntry[]>([]);
  const [fact, setFact] = useState<Fact | null>(null);
  const [incoming, setIncoming] = useState<RefGroup[]>([]);
  const [outgoing, setOutgoing] = useState<RefGroup[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);
    setFact(null);
    setIncoming([]);
    setOutgoing([]);

    const factPromise = api.fact(repo, branch, current.path, current.commit ?? undefined);
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

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%', background: '#0a0a0a' }}>
      {/* Header bar */}
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '0 12px', height: 32, background: '#0f0f0f', borderBottom: '1px solid #1a1a1a', flexShrink: 0 }}>
        <span style={{ fontSize: 10, color: '#444', textTransform: 'uppercase', letterSpacing: '0.08em' }}>Explain</span>
        <span style={{ fontSize: 11, color: '#8af', fontFamily: 'monospace', flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{current.path}</span>
        {backStack.length > 0 && (
          <button onClick={goBack} style={{ background: 'none', border: '1px solid #2a2a2a', borderRadius: 3, color: '#888', fontSize: 11, padding: '2px 8px', cursor: 'pointer' }}>← Back</button>
        )}
        <button onClick={onClose} style={{ background: 'none', border: '1px solid #2a2a2a', borderRadius: 3, color: '#666', fontSize: 11, padding: '2px 8px', cursor: 'pointer' }}>✕ Close</button>
      </div>

      {/* Incoming refs strip */}
      {current.commit === null && (
        <div style={{ flexShrink: 0, borderBottom: '1px solid #1a1a1a', padding: '6px 12px', background: '#0d0d0d', minHeight: 38, display: 'flex', flexWrap: 'wrap', gap: 6, alignItems: 'center' }}>
          <span style={{ fontSize: 9, color: '#3a3a3a', textTransform: 'uppercase', letterSpacing: '0.08em', marginRight: 4 }}>↙ Referenced by</span>
          {incoming.length === 0 && !loading && <span style={{ fontSize: 11, color: '#2a2a2a' }}>none</span>}
          {incoming.map(g => (
            <Chip key={g.path} group={g} onClick={commit => navigateTo({ path: g.path, commit })} />
          ))}
        </div>
      )}

      {/* Fact body */}
      <div style={{ flex: 1, overflowY: 'auto', padding: '16px 20px' }}>
        {loading && <div style={{ color: '#444', fontSize: 12 }}>Loading…</div>}
        {error && <div style={{ color: '#f66', fontSize: 12 }}>{error}</div>}
        {fact && (
          <div style={{ maxWidth: 720 }}>
            <div style={{ fontSize: 10, color: '#444', fontFamily: 'monospace', marginBottom: 6 }}>{fact.path}</div>
            <div data-testid="fact-title" style={{ fontSize: 18, fontWeight: 600, color: '#eee', letterSpacing: '-0.3px', marginBottom: 14 }}>{fact.title || fact.path}</div>
            <FactBody fact={fact} navigate={() => {}} dispatch={() => {}} readOnly={true} />
          </div>
        )}
      </div>

      {/* Outgoing refs strip */}
      <div style={{ flexShrink: 0, borderTop: '1px solid #1a1a1a', padding: '6px 12px', background: '#0d0d0d', minHeight: 38, display: 'flex', flexWrap: 'wrap', gap: 6, alignItems: 'center' }}>
        <span style={{ fontSize: 9, color: '#3a3a3a', textTransform: 'uppercase', letterSpacing: '0.08em', marginRight: 4 }}>↗ References</span>
        {outgoing.length === 0 && !loading && <span style={{ fontSize: 11, color: '#2a2a2a' }}>none</span>}
        {outgoing.map(g => (
          <Chip
            key={g.path}
            group={g}
            onClick={commit => navigateTo({ path: g.path, commit: g.deleted ? commit : null })}
          />
        ))}
      </div>
    </div>
  );
}

function Chip({ group, onClick }: { group: RefGroup; onClick: (commit: string) => void }) {
  const [open, setOpen] = useState(false);
  const dropdownRef = useRef<HTMLDivElement | null>(null);
  const chipRef = useRef<HTMLSpanElement | null>(null);

  const versionCount = group.versions.length;
  const isMulti = versionCount > 1;
  const deleted = group.deleted ?? false;
  const latest = group.versions[0];

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

  return (
    <span
      ref={chipRef}
      onClick={handleChipClick}
      title={deleted ? 'Target fact retracted.' : group.path}
      style={{
        display: 'inline-flex', flexDirection: 'column',
        padding: '3px 8px', borderRadius: 12,
        border: `1px solid ${deleted ? '#2a2a2a' : '#253565'}`,
        cursor: 'pointer', background: '#111',
        maxWidth: 200,
        opacity: deleted ? 0.45 : 1,
        textDecoration: deleted ? 'line-through' : 'none',
        position: 'relative',
      }}
    >
      <span style={{ fontSize: 11, color: deleted ? '#555' : '#8af', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
        {group.title || group.path}
        {deleted && <span style={{ fontSize: 9, color: '#444', marginLeft: 4 }}>[deleted]</span>}
      </span>
      <span style={{ display: 'flex', alignItems: 'center', gap: 6, overflow: 'hidden' }}>
        <span style={{ fontSize: 9, color: '#333', fontFamily: 'monospace', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', flex: 1 }}>{group.path}</span>
        {isMulti ? (
          <span style={{
            fontFamily: 'monospace', fontSize: 9, color: '#8af',
            background: '#1a1a2a', padding: '0 4px', borderRadius: 2,
            flexShrink: 0,
          }}>×{versionCount} ⌄</span>
        ) : (
          latest?.commit && (
            <span style={{
              fontFamily: 'monospace', fontSize: 9, color: '#8af',
              background: '#1a1a2a', padding: '0 4px', borderRadius: 2,
              textDecoration: 'none',
              flexShrink: 0,
            }}>commit_at_{latest.commit.slice(0, 7)}</span>
          )
        )}
      </span>
      {open && isMulti && (
        <div
          ref={dropdownRef}
          onClick={e => e.stopPropagation()}
          style={{
            position: 'absolute',
            top: '100%',
            left: 0,
            marginTop: 2,
            minWidth: 180,
            maxHeight: 200,
            overflowY: 'auto',
            background: '#111',
            border: '1px solid #2a2a2a',
            borderRadius: 4,
            padding: '4px 0',
            zIndex: 50,
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
              <span style={{ fontSize: 10, color: idx === 0 ? '#8af' : '#444' }}>
                {idx === 0 ? '●' : '○'}
              </span>
              <span style={{ fontFamily: 'monospace', fontSize: 10, color: '#8af' }}>
                {v.commit.slice(0, 7)}
              </span>
              <span style={{ fontSize: 10, color: '#666', marginLeft: 'auto' }}>
                {relativeTimeEpoch(v.committed_at ?? 0)}
              </span>
            </div>
          ))}
        </div>
      )}
    </span>
  );
}

