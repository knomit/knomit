import { useEffect, useState, useRef } from 'react';
import type { Dispatch } from 'react';
import { api } from './api';
import type { Fact } from './api';
import type { Action, AppState } from './state';
import { unifiedDiff } from './diff';

interface Props {
  state: AppState & { factPath: string };
  dispatch: Dispatch<Action>;
}

export function FactDiffView({ state, dispatch }: Props) {
  const [fromFact, setFromFact] = useState<Fact | null>(null);
  const [toFact, setToFact] = useState<Fact | null>(null);
  const [error, setError] = useState<string | null>(null);
  const ctrlRef = useRef<AbortController | null>(null);

  // Diff endpoints expect both `from` and `to`. Outside diff mode we don't
  // fetch — the caller (RightPanel) only renders this view in diff mode.
  const asOf = state.asOf;
  const inDiff = asOf.mode === 'diff';
  const from = asOf.mode === 'diff' ? asOf.from : '';
  const to   = asOf.mode === 'diff' ? asOf.to   : '';

  useEffect(() => {
    if (!inDiff) return;
    ctrlRef.current?.abort();
    const ctrl = new AbortController();
    ctrlRef.current = ctrl;
    setError(null);
    api.factDiff(state.repo, state.branch, state.factPath, from, to, ctrl.signal)
      .then(({ from: f, to: t }) => {
        if (ctrl.signal.aborted) return;
        setFromFact(f);
        setToFact(t);
      })
      .catch(e => {
        if (ctrl.signal.aborted) return;
        if (e instanceof DOMException && e.name === 'AbortError') return;
        setError(String(e));
      });
    return () => ctrl.abort();
  }, [inDiff, state.repo, state.branch, state.factPath, from, to]);

  if (!inDiff) return null;
  if (error) return <div style={{ padding: 24, color: '#f44' }}>{error}</div>;

  const exitDiff = () => dispatch({ type: 'SET_AS_OF', asOf: { mode: 'scrubbed', commit: to } });

  return (
    <div style={{ padding: '24px 28px', overflowY: 'auto', height: '100%', boxSizing: 'border-box' }}>
      {/* Header: two commit chips + exit button */}
      <div style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 16 }}>
        <span style={{ fontSize: 10, color: '#666', textTransform: 'uppercase', letterSpacing: 1.2 }}>diff</span>
        <span style={{ color: '#8af', fontFamily: 'monospace', fontSize: 11, background: '#1a1a2a',
                       padding: '2px 7px', borderRadius: 3 }}>{from.slice(0, 7)}</span>
        <span style={{ color: '#666' }}>→</span>
        <span style={{ color: '#8af', fontFamily: 'monospace', fontSize: 11, background: '#1a1a2a',
                       padding: '2px 7px', borderRadius: 3 }}>{to.slice(0, 7)}</span>
        <div style={{ flex: 1 }} />
        <button onClick={exitDiff} style={{
          background: '#1a1a1a', border: '1px solid #2a2a33', color: '#a0a0a8',
          padding: '4px 10px', borderRadius: 3, fontSize: 11, cursor: 'pointer',
        }}>Exit diff</button>
      </div>

      {/* Empty-side chip */}
      {!fromFact && toFact && (
        <div style={{ color: '#7c9', fontSize: 11, fontFamily: 'monospace', marginBottom: 12 }}>
          not yet created at {from.slice(0, 7)}
        </div>
      )}
      {fromFact && !toFact && (
        <div style={{ color: '#f88', fontSize: 11, fontFamily: 'monospace', marginBottom: 12 }}>
          retracted at {to.slice(0, 7)}
        </div>
      )}

      {/* Metadata diff (side-by-side) */}
      {(fromFact || toFact) && (
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12, marginBottom: 18 }}>
          <MetaPanel fact={fromFact} side="from" />
          <MetaPanel fact={toFact}   side="to" />
        </div>
      )}

      {/* Body diff */}
      <div style={{
        fontFamily: 'monospace', fontSize: 12.5, lineHeight: 1.55,
        background: '#08080a', border: '1px solid #2a2a33', borderRadius: 6,
        padding: '12px 16px', whiteSpace: 'pre-wrap',
      }}>
        {unifiedDiff(fromFact?.body ?? '', toFact?.body ?? '').map((line, i) => {
          const color = line.kind === 'add' ? '#7c9' : line.kind === 'del' ? '#f88' : '#a0a0a8';
          const bg    = line.kind === 'add' ? 'rgba(124,153,153,0.06)'
                       : line.kind === 'del' ? 'rgba(255,136,136,0.06)' : 'transparent';
          const prefix = line.kind === 'add' ? '+' : line.kind === 'del' ? '−' : ' ';
          return (
            <div key={i} style={{ background: bg, color, padding: '0 4px' }}>
              {prefix} {line.text}
            </div>
          );
        })}
      </div>
    </div>
  );
}

function MetaPanel({ fact, side }: { fact: Fact | null; side: 'from' | 'to' }) {
  if (!fact) {
    return (
      <div style={{ background: '#08080a', border: '1px solid #2a2a33', borderRadius: 6,
                    padding: '10px 12px', color: '#5a5a65', fontSize: 11, fontStyle: 'italic' }}>
        {side === 'from' ? 'not yet created' : 'retracted'}
      </div>
    );
  }
  return (
    <div style={{ background: '#111', border: '1px solid #1f1f26', borderRadius: 6, padding: '10px 12px' }}>
      <div style={{ fontSize: 9, color: '#5a5a65', textTransform: 'uppercase', letterSpacing: 1.2, marginBottom: 6 }}>
        {side === 'from' ? 'before' : 'after'}
      </div>
      <div style={{ fontSize: 13, color: '#e6e6ea', marginBottom: 4 }}>{fact.title}</div>
      <div style={{ fontSize: 11, color: '#a0a0a8' }}>type: {fact.type ?? '—'}</div>
      <div style={{ fontSize: 11, color: '#a0a0a8' }}>domains: {fact.domain.join(', ') || '—'}</div>
      <div style={{ fontSize: 11, color: '#a0a0a8' }}>entities: {fact.entities.join(', ') || '—'}</div>
    </div>
  );
}
