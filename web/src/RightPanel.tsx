import { useEffect, useState } from 'react';
import type { Dispatch } from 'react';
import ReactMarkdown from 'react-markdown';
import { api } from './api';
import type { Fact, HistoryEntry, Stats } from './api';
import type { AppState, Action } from './state';

interface Props {
  state: AppState;
  dispatch: Dispatch<Action>;
}

export function RightPanel({ state, dispatch }: Props) {
  const [fact, setFact] = useState<Fact | null>(null);
  const [history, setHistory] = useState<HistoryEntry[]>([]);
  const [stats, setStats] = useState<Stats | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    setError(null);
    if (state.rightMode === 'fact' && state.selectedFact) {
      api.fact(state.selectedFact).then(setFact).catch(e => setError(String(e)));
    } else if (state.rightMode === 'history' && state.selectedFact) {
      api.history(state.selectedFact).then(r => setHistory(r.entries || [])).catch(e => setError(String(e)));
    } else if (state.rightMode === 'summary') {
      api.stats(state.currentPath).then(setStats).catch(() => setStats(null));
    }
  }, [state.rightMode, state.selectedFact, state.currentPath]);

  if (error) return <div style={{ padding: 24, color: '#f44' }}>{error}</div>;

  if (state.rightMode === 'summary') {
    return (
      <div style={{ padding: 24, overflowY: 'auto', height: '100%', boxSizing: 'border-box' }}>
        <h2 style={{ color: '#aaa', fontSize: 16, marginBottom: 16 }}>{state.currentPath}</h2>
        {stats ? (
          <>
            <div style={{ display: 'flex', gap: 16, flexWrap: 'wrap', marginBottom: 24 }}>
              {([['Total facts', stats.total], ['Avg confidence', stats.avg_confidence.toFixed(2)]] as [string, string | number][]).map(([label, value]) => (
                <div key={String(label)} style={{ background: '#1e1e2e', border: '1px solid #333', borderRadius: 8, padding: '12px 20px', minWidth: 120 }}>
                  <div style={{ color: '#666', fontSize: 11, marginBottom: 4 }}>{label}</div>
                  <div style={{ color: '#eee', fontSize: 22, fontWeight: 'bold' }}>{value}</div>
                </div>
              ))}
            </div>
            <div style={{ marginBottom: 16 }}>
              <div style={{ color: '#666', fontSize: 11, marginBottom: 8 }}>TOP DOMAINS</div>
              <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8 }}>
                {Object.entries(stats.domains).sort((a, b) => b[1] - a[1]).slice(0, 10).map(([d, n]) => (
                  <span key={d} onClick={() => dispatch({ type: 'SEARCH', query: d })}
                    style={{ background: '#2a2a3a', color: '#adf', padding: '4px 10px', borderRadius: 20, fontSize: 12, cursor: 'pointer' }}>
                    {d} ({n})
                  </span>
                ))}
              </div>
            </div>
          </>
        ) : <div style={{ color: '#666' }}>No facts indexed in this path.</div>}
      </div>
    );
  }

  if (state.rightMode === 'history') {
    return (
      <div style={{ padding: 24, overflowY: 'auto', height: '100%', boxSizing: 'border-box' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 20 }}>
          <button onClick={() => dispatch({ type: 'SHOW_FACT' })} style={{ background: '#333', color: '#eee', border: 'none', padding: '6px 12px', borderRadius: 4, cursor: 'pointer' }}>
            ← Back to Fact
          </button>
          <h2 style={{ color: '#aaa', fontSize: 15, margin: 0 }}>History: {state.selectedFact}</h2>
        </div>
        {history.map(e => (
          <div key={e.commit} style={{ borderBottom: '1px solid #222', padding: '10px 0', display: 'flex', gap: 12, alignItems: 'flex-start' }}>
            <code style={{ color: '#7c9', fontSize: 12, minWidth: 64 }}>{e.commit.slice(0, 7)}</code>
            <div>
              <div style={{ color: '#888', fontSize: 11 }}>{new Date(e.date).toLocaleDateString()}</div>
              <div style={{ color: '#ddd', fontSize: 13 }}>{e.message}</div>
            </div>
          </div>
        ))}
        {history.length === 0 && <div style={{ color: '#666' }}>No history available.</div>}
      </div>
    );
  }

  // Fact mode
  if (!fact) return <div style={{ padding: 24, color: '#666' }}>Select a fact to view.</div>;

  return (
    <div style={{ padding: 24, overflowY: 'auto', height: '100%', boxSizing: 'border-box' }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', marginBottom: 16 }}>
        <h1 style={{ color: '#eee', fontSize: 20, margin: 0, flex: 1 }}>{fact.title || fact.path}</h1>
        <button onClick={() => dispatch({ type: 'SHOW_HISTORY' })}
          style={{ background: '#333', color: '#aaa', border: 'none', padding: '4px 10px', borderRadius: 4, cursor: 'pointer', fontSize: 12, marginLeft: 12 }}>
          History
        </button>
      </div>

      {/* Metadata */}
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8, marginBottom: 16 }}>
        {fact.domain?.map(d => (
          <span key={d} onClick={() => dispatch({ type: 'SEARCH', query: d })}
            style={{ background: '#2a3a2a', color: '#8c8', padding: '3px 10px', borderRadius: 20, fontSize: 11, cursor: 'pointer' }}>
            {d}
          </span>
        ))}
        <span style={{ background: '#1a1a1a', color: '#888', padding: '3px 10px', borderRadius: 20, fontSize: 11 }}>
          confidence: {fact.confidence?.toFixed(2)}
        </span>
        <span style={{ background: '#1a1a1a', color: '#888', padding: '3px 10px', borderRadius: 20, fontSize: 11 }}>
          sources: {fact.sources}
        </span>
      </div>

      {/* Body */}
      <div style={{ color: '#ccc', lineHeight: 1.7, fontSize: 14, marginBottom: 20 }}>
        <ReactMarkdown>{fact.body || ''}</ReactMarkdown>
      </div>

      {/* Entities */}
      {fact.entities?.length > 0 && (
        <div style={{ marginBottom: 16 }}>
          <div style={{ color: '#555', fontSize: 11, marginBottom: 6 }}>ENTITIES</div>
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
            {fact.entities.map(e => (
              <span key={e} onClick={() => dispatch({ type: 'SEARCH', query: e })}
                style={{ background: '#1a2a3a', color: '#8af', padding: '3px 8px', borderRadius: 4, fontSize: 11, cursor: 'pointer' }}>
                {e}
              </span>
            ))}
          </div>
        </div>
      )}

      {/* Refs */}
      {fact.refs?.length > 0 && (
        <div>
          <div style={{ color: '#555', fontSize: 11, marginBottom: 6 }}>REFERENCES</div>
          {fact.refs.map(ref => {
            if (ref.startsWith('http://') || ref.startsWith('https://')) {
              return <div key={ref}><a href={ref} target="_blank" rel="noopener noreferrer" style={{ color: '#68f', fontSize: 12 }}>↗ {ref}</a></div>;
            }
            return <div key={ref} style={{ color: '#888', fontSize: 12, fontFamily: 'monospace' }}>{ref}</div>;
          })}
        </div>
      )}
    </div>
  );
}
