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
      api.stats(state.previewPath ?? state.currentPath).then(setStats).catch(() => setStats(null));
    }
  }, [state.rightMode, state.selectedFact, state.currentPath, state.previewPath]);

  if (error) return <div style={{ padding: 24, color: '#f44' }}>{error}</div>;

  if (state.rightMode === 'summary') {
    const domainEntries = stats ? Object.entries(stats.domains).sort((a, b) => b[1] - a[1]).slice(0, 10) : [];
    const entityEntries = stats ? Object.entries(stats.entities).sort((a, b) => b[1] - a[1]).slice(0, 10) : [];
    const domainCount = stats ? Object.keys(stats.domains).length : 0;

    return (
      <div style={{ padding: '24px 28px', overflowY: 'auto', height: '100%', boxSizing: 'border-box' }}>
        {stats ? (
          <>
            {/* Summary line */}
            <div style={{ fontSize: 12, color: '#555', marginBottom: 20 }}>
              {stats.total} facts across {domainCount} domains
            </div>

            {/* Stat cards */}
            <div style={{ display: 'flex', gap: 10, marginBottom: 28 }}>
              <div style={{ borderLeft: '3px solid #7c9', padding: '10px 16px', background: '#1a1a2a', borderRadius: '0 6px 6px 0', minWidth: 90 }}>
                <div style={{ fontSize: 10, color: '#666', textTransform: 'uppercase', letterSpacing: 1 }}>Facts</div>
                <div style={{ fontSize: 22, fontWeight: 600, color: '#eee', marginTop: 2 }}>{stats.total}</div>
              </div>
              <div style={{ borderLeft: '3px solid #8af', padding: '10px 16px', background: '#1a1a2a', borderRadius: '0 6px 6px 0', minWidth: 90 }}>
                <div style={{ fontSize: 10, color: '#666', textTransform: 'uppercase', letterSpacing: 1 }}>Confidence</div>
                <div style={{ fontSize: 22, fontWeight: 600, color: '#eee', marginTop: 2 }}>{stats.avg_confidence.toFixed(2)}</div>
              </div>
              <div style={{ borderLeft: '3px solid #fa8', padding: '10px 16px', background: '#1a1a2a', borderRadius: '0 6px 6px 0', minWidth: 90 }}>
                <div style={{ fontSize: 10, color: '#666', textTransform: 'uppercase', letterSpacing: 1 }}>Domains</div>
                <div style={{ fontSize: 22, fontWeight: 600, color: '#eee', marginTop: 2 }}>{domainCount}</div>
              </div>
            </div>

            {/* Domains */}
            {domainEntries.length > 0 && (
              <div style={{ marginBottom: 22 }}>
                <div style={{ fontSize: 10, textTransform: 'uppercase', letterSpacing: 1.5, color: '#555', marginBottom: 10 }}>Domains</div>
                <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
                  {domainEntries.map(([d, n]) => {
                    const max = domainEntries[0][1];
                    const ratio = n / max;
                    return (
                      <span key={d} onClick={() => dispatch({ type: 'SEARCH', query: `domain:${d}` })}
                        style={{
                          display: 'inline-flex', alignItems: 'center', gap: 5, cursor: 'pointer',
                          padding: ratio >= 0.75 ? '5px 11px' : '4px 9px',
                          borderRadius: 6,
                          background: ratio >= 0.5 ? '#1a1a2a' : 'rgba(26,26,42,0.6)',
                          border: `1px solid rgba(119,204,153,${ratio >= 0.75 ? 0.3 : ratio >= 0.5 ? 0.2 : 0.1})`,
                          transition: 'border-color 0.15s, opacity 0.15s',
                        }}
                        onMouseEnter={e => { (e.currentTarget as HTMLElement).style.borderColor = 'rgba(119,204,153,0.5)'; }}
                        onMouseLeave={e => { (e.currentTarget as HTMLElement).style.borderColor = `rgba(119,204,153,${ratio >= 0.75 ? 0.3 : ratio >= 0.5 ? 0.2 : 0.1})`; }}
                      >
                        <span style={{
                          fontSize: ratio >= 0.5 ? 12 : 11,
                          fontWeight: ratio >= 0.75 ? 600 : 'normal',
                          color: ratio >= 0.5 ? '#7c9' : 'rgba(119,204,153,0.6)',
                        }}>{d}</span>
                        <span style={{
                          fontSize: 9, borderRadius: 10, padding: '1px 5px', fontWeight: 600,
                          color: ratio >= 0.5 ? '#111' : 'rgba(119,204,153,0.5)',
                          background: ratio >= 0.75 ? '#7c9' : ratio >= 0.5 ? 'rgba(119,204,153,0.8)' : 'rgba(119,204,153,0.15)',
                        }}>{n}</span>
                      </span>
                    );
                  })}
                </div>
              </div>
            )}

            {/* Entities */}
            {entityEntries.length > 0 && (
              <div>
                <div style={{ fontSize: 10, textTransform: 'uppercase', letterSpacing: 1.5, color: '#555', marginBottom: 10 }}>Entities</div>
                <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
                  {entityEntries.map(([e, n]) => {
                    const max = entityEntries[0][1];
                    const ratio = n / max;
                    return (
                      <span key={e} onClick={() => dispatch({ type: 'SEARCH', query: `entity:${e}` })}
                        style={{
                          display: 'inline-flex', alignItems: 'center', gap: 5, cursor: 'pointer',
                          padding: ratio >= 0.75 ? '5px 11px' : '4px 9px',
                          borderRadius: 6,
                          background: ratio >= 0.5 ? '#1a1a2a' : 'rgba(26,26,42,0.6)',
                          border: `1px solid rgba(136,170,255,${ratio >= 0.75 ? 0.3 : ratio >= 0.5 ? 0.2 : 0.1})`,
                          transition: 'border-color 0.15s, opacity 0.15s',
                        }}
                        onMouseEnter={e => { (e.currentTarget as HTMLElement).style.borderColor = 'rgba(136,170,255,0.5)'; }}
                        onMouseLeave={e => { (e.currentTarget as HTMLElement).style.borderColor = `rgba(136,170,255,${ratio >= 0.75 ? 0.3 : ratio >= 0.5 ? 0.2 : 0.1})`; }}
                      >
                        <span style={{
                          fontSize: ratio >= 0.5 ? 12 : 11,
                          fontWeight: ratio >= 0.75 ? 600 : 'normal',
                          color: ratio >= 0.5 ? '#8af' : 'rgba(136,170,255,0.6)',
                        }}>{e}</span>
                        <span style={{
                          fontSize: 9, borderRadius: 10, padding: '1px 5px', fontWeight: 600,
                          color: ratio >= 0.5 ? '#111' : 'rgba(136,170,255,0.5)',
                          background: ratio >= 0.75 ? '#8af' : ratio >= 0.5 ? 'rgba(136,170,255,0.8)' : 'rgba(136,170,255,0.15)',
                        }}>{n}</span>
                      </span>
                    );
                  })}
                </div>
              </div>
            )}
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
    <div style={{ padding: '24px 28px', overflowY: 'auto', height: '100%', boxSizing: 'border-box' }}>
      {/* Header */}
      <div style={{ marginBottom: 20 }}>
        <div style={{ fontSize: 18, fontWeight: 600, color: '#eee', letterSpacing: '-0.3px' }}>
          {fact.title || fact.path}
        </div>
        <div style={{ fontSize: 12, color: '#555', marginTop: 2 }}>{fact.path}</div>
      </div>

      {/* Stat cards */}
      <div style={{ display: 'flex', gap: 10, marginBottom: 28 }}>
        <div style={{ borderLeft: '3px solid #8af', padding: '10px 16px', background: '#1a1a2a', borderRadius: '0 6px 6px 0', minWidth: 90 }}>
          <div style={{ fontSize: 10, color: '#666', textTransform: 'uppercase', letterSpacing: 1 }}>Confidence</div>
          <div style={{ fontSize: 22, fontWeight: 600, color: '#eee', marginTop: 2 }}>{fact.confidence?.toFixed(2)}</div>
        </div>
        <div style={{ borderLeft: '3px solid #7c9', padding: '10px 16px', background: '#1a1a2a', borderRadius: '0 6px 6px 0', minWidth: 90 }}>
          <div style={{ fontSize: 10, color: '#666', textTransform: 'uppercase', letterSpacing: 1 }}>Sources</div>
          <div style={{ fontSize: 22, fontWeight: 600, color: '#eee', marginTop: 2 }}>{fact.sources}</div>
        </div>
      </div>

      {/* Body */}
      <div style={{ color: '#ccc', lineHeight: 1.7, fontSize: 14, marginBottom: 24 }}>
        <ReactMarkdown>{fact.body || ''}</ReactMarkdown>
      </div>

      {/* Domains */}
      {fact.domain?.length > 0 && (
        <div style={{ marginBottom: 22 }}>
          <div style={{ fontSize: 10, textTransform: 'uppercase', letterSpacing: 1.5, color: '#555', marginBottom: 10 }}>Domains</div>
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
            {fact.domain.map(d => (
              <span key={d} onClick={() => dispatch({ type: 'SEARCH', query: `domain:${d}` })}
                style={{
                  display: 'inline-flex', alignItems: 'center', gap: 5, cursor: 'pointer',
                  padding: '5px 11px', borderRadius: 6,
                  background: '#1a1a2a', border: '1px solid rgba(119,204,153,0.2)',
                  transition: 'border-color 0.15s',
                }}
                onMouseEnter={e => { (e.currentTarget as HTMLElement).style.borderColor = 'rgba(119,204,153,0.5)'; }}
                onMouseLeave={e => { (e.currentTarget as HTMLElement).style.borderColor = 'rgba(119,204,153,0.2)'; }}
              >
                <span style={{ fontSize: 12, color: '#7c9' }}>{d}</span>
              </span>
            ))}
          </div>
        </div>
      )}

      {/* Entities */}
      {fact.entities?.length > 0 && (
        <div style={{ marginBottom: 22 }}>
          <div style={{ fontSize: 10, textTransform: 'uppercase', letterSpacing: 1.5, color: '#555', marginBottom: 10 }}>Entities</div>
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
            {fact.entities.map(e => (
              <span key={e} onClick={() => dispatch({ type: 'SEARCH', query: `entity:${e}` })}
                style={{
                  display: 'inline-flex', alignItems: 'center', gap: 5, cursor: 'pointer',
                  padding: '5px 11px', borderRadius: 6,
                  background: '#1a1a2a', border: '1px solid rgba(136,170,255,0.2)',
                  transition: 'border-color 0.15s',
                }}
                onMouseEnter={e => { (e.currentTarget as HTMLElement).style.borderColor = 'rgba(136,170,255,0.5)'; }}
                onMouseLeave={e => { (e.currentTarget as HTMLElement).style.borderColor = 'rgba(136,170,255,0.2)'; }}
              >
                <span style={{ fontSize: 12, color: '#8af' }}>{e}</span>
              </span>
            ))}
          </div>
        </div>
      )}

      {/* Refs */}
      {fact.refs?.length > 0 && (
        <div>
          <div style={{ fontSize: 10, textTransform: 'uppercase', letterSpacing: 1.5, color: '#555', marginBottom: 10 }}>References</div>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
            {fact.refs.map(ref => {
              if (ref.startsWith('http://') || ref.startsWith('https://')) {
                return (
                  <a key={ref} href={ref} target="_blank" rel="noopener noreferrer"
                    style={{ color: '#8af', fontSize: 12, textDecoration: 'none', transition: 'color 0.15s' }}
                    onMouseEnter={e => { (e.currentTarget as HTMLElement).style.color = '#adf'; }}
                    onMouseLeave={e => { (e.currentTarget as HTMLElement).style.color = '#8af'; }}
                  >↗ {ref}</a>
                );
              }
              return <div key={ref} style={{ color: '#888', fontSize: 12, fontFamily: 'monospace' }}>{ref}</div>;
            })}
          </div>
        </div>
      )}
    </div>
  );
}
