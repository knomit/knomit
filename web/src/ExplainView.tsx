import { useState, useEffect } from 'react';
import { api } from './api';
import type { Fact } from './api';

interface ExplainEntry { path: string; commit: string | null; }
interface RefSummary { path: string; title: string; commit?: string; deleted?: boolean; }

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
  const [incoming, setIncoming] = useState<RefSummary[]>([]);
  const [outgoing, setOutgoing] = useState<RefSummary[]>([]);
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
    const explainPromise = current.commit === null
      ? api.explain(repo, branch, current.path)
      : Promise.resolve({ incoming: [], outgoing: [] });

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
          {incoming.map(r => (
            <Chip key={r.path} item={r} onClick={() => navigateTo({ path: r.path, commit: null })} />
          ))}
        </div>
      )}

      {/* Fact body */}
      <div style={{ flex: 1, overflowY: 'auto', padding: '16px 20px' }}>
        {loading && <div style={{ color: '#444', fontSize: 12 }}>Loading…</div>}
        {error && <div style={{ color: '#f66', fontSize: 12 }}>{error}</div>}
        {fact && <FactReadOnly fact={fact} />}
      </div>

      {/* Outgoing refs strip */}
      <div style={{ flexShrink: 0, borderTop: '1px solid #1a1a1a', padding: '6px 12px', background: '#0d0d0d', minHeight: 38, display: 'flex', flexWrap: 'wrap', gap: 6, alignItems: 'center' }}>
        <span style={{ fontSize: 9, color: '#3a3a3a', textTransform: 'uppercase', letterSpacing: '0.08em', marginRight: 4 }}>↗ References</span>
        {outgoing.length === 0 && !loading && <span style={{ fontSize: 11, color: '#2a2a2a' }}>none</span>}
        {outgoing.map(r => (
          <Chip
            key={r.path}
            item={r}
            deleted={r.deleted}
            onClick={() => navigateTo({ path: r.path, commit: r.deleted ? current.commit : null })}
          />
        ))}
      </div>
    </div>
  );
}

function Chip({ item, deleted, onClick }: { item: RefSummary; deleted?: boolean; onClick: () => void }) {
  return (
    <span
      onClick={onClick}
      title={deleted ? 'Target fact retracted.' : item.path}
      style={{
        display: 'inline-flex', flexDirection: 'column',
        padding: '3px 8px', borderRadius: 12,
        border: `1px solid ${deleted ? '#2a2a2a' : '#253565'}`,
        cursor: 'pointer', background: '#111',
        maxWidth: 200,
        opacity: deleted ? 0.45 : 1,
        textDecoration: deleted ? 'line-through' : 'none',
      }}
    >
      <span style={{ fontSize: 11, color: deleted ? '#555' : '#8af', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
        {item.title || item.path}
        {deleted && <span style={{ fontSize: 9, color: '#444', marginLeft: 4 }}>[deleted]</span>}
      </span>
      <span style={{ display: 'flex', alignItems: 'center', gap: 6, overflow: 'hidden' }}>
        <span style={{ fontSize: 9, color: '#333', fontFamily: 'monospace', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', flex: 1 }}>{item.path}</span>
        {item.commit && (
          <span style={{
            fontFamily: 'monospace', fontSize: 9, color: '#8af',
            background: '#1a1a2a', padding: '0 4px', borderRadius: 2,
            textDecoration: 'none',
            flexShrink: 0,
          }}>commit_at_{item.commit.slice(0, 7)}</span>
        )}
      </span>
    </span>
  );
}

function FactReadOnly({ fact }: { fact: Fact }) {
  return (
    <div style={{ maxWidth: 720 }}>
      <div style={{ fontSize: 10, color: '#444', fontFamily: 'monospace', marginBottom: 6 }}>{fact.path}</div>
      <div style={{ fontSize: 15, color: '#ddd', fontWeight: 500, marginBottom: 10 }}>{fact.title}</div>
      <div style={{ fontSize: 13, color: '#888', lineHeight: 1.7, whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{fact.body}</div>
      {(fact.domain?.length > 0 || fact.entities?.length > 0) && (
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 4, marginTop: 12 }}>
          {fact.domain?.map(d => <Tag key={d} label={d} />)}
          {fact.entities?.map(e => <Tag key={e} label={e} />)}
        </div>
      )}
    </div>
  );
}

function Tag({ label }: { label: string }) {
  return (
    <span style={{ fontSize: 10, color: '#555', background: '#141414', border: '1px solid #222', borderRadius: 3, padding: '1px 6px' }}>
      {label}
    </span>
  );
}
