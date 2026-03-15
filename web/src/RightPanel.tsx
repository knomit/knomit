import { useEffect, useState } from 'react';
import type { Dispatch } from 'react';
import ReactMarkdown from 'react-markdown';
import { api } from './api';
import type { Fact, HistoryEntry, Stats, CommitDetail } from './api';
import type { AppState, Action } from './state';

interface Props {
  state: AppState;
  dispatch: Dispatch<Action>;
}

function relativeTime(dateStr: string): string {
  const diff = Date.now() - new Date(dateStr).getTime();
  const mins = Math.floor(diff / 60000);
  if (mins < 1) return 'just now';
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  if (days < 30) return `${days}d ago`;
  return new Date(dateStr).toLocaleDateString();
}

function tagColor(tag: string): { color: string; bg: string } {
  if (tag.startsWith('learn/')) return { color: '#7c9', bg: '#1a2e1a' };
  if (tag.startsWith('update/')) return { color: '#8af', bg: '#1a1a2e' };
  if (tag.startsWith('retract/')) return { color: '#f88', bg: '#2e1a1a' };
  if (tag.startsWith('synthesize/') || tag.startsWith('subsume/')) return { color: '#fa0', bg: '#2e2a1a' };
  return { color: '#888', bg: '#222' };
}

function TagCloud({ label, entries, color, searchPrefix, onSearch }: {
  label: string;
  entries: [string, number][] | string[];
  color: string; // e.g. '119,204,153' or '136,170,255'
  searchPrefix: string; // e.g. 'domain:' or 'entity:'
  onSearch: (query: string) => void;
}) {
  if (entries.length === 0) return null;

  // Normalize: if entries are strings (flat list), convert to [name, 1] pairs
  const items: [string, number][] = typeof entries[0] === 'string'
    ? (entries as string[]).map(s => [s, 1])
    : entries as [string, number][];
  const max = items[0][1];
  const weighted = items.some(([, n]) => n !== items[0][1]);

  return (
    <div style={{ marginBottom: 22 }}>
      <div style={{ fontSize: 10, textTransform: 'uppercase', letterSpacing: 1.5, color: '#555', marginBottom: 10 }}>{label}</div>
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
        {items.map(([name, n]) => {
          const ratio = max > 0 ? n / max : 1;
          const accent = `rgba(${color},`;
          return (
            <span key={name} onClick={() => onSearch(`${searchPrefix}${name}`)}
              style={{
                display: 'inline-flex', alignItems: 'center', gap: 5, cursor: 'pointer',
                padding: weighted && ratio >= 0.75 ? '5px 11px' : weighted ? '4px 9px' : '5px 11px',
                borderRadius: 6,
                background: weighted && ratio < 0.5 ? 'rgba(26,26,42,0.6)' : '#1a1a2a',
                border: `1px solid ${accent}${weighted ? (ratio >= 0.75 ? 0.3 : ratio >= 0.5 ? 0.2 : 0.1) : 0.2})`,
                transition: 'border-color 0.15s, opacity 0.15s',
              }}
              onMouseEnter={e => { (e.currentTarget as HTMLElement).style.borderColor = `${accent}0.5)`; }}
              onMouseLeave={e => { (e.currentTarget as HTMLElement).style.borderColor = `${accent}${weighted ? (ratio >= 0.75 ? 0.3 : ratio >= 0.5 ? 0.2 : 0.1) : 0.2})`; }}
            >
              <span style={{
                fontSize: weighted && ratio >= 0.5 ? 12 : weighted ? 11 : 12,
                fontWeight: weighted && ratio >= 0.75 ? 600 : 'normal',
                color: !weighted || ratio >= 0.5 ? `rgb(${color})` : `${accent}0.6)`,
              }}>{name}</span>
              {weighted && (
                <span style={{
                  fontSize: 9, borderRadius: 10, padding: '1px 5px', fontWeight: 600,
                  color: ratio >= 0.5 ? '#111' : `${accent}0.5)`,
                  background: ratio >= 0.75 ? `rgb(${color})` : ratio >= 0.5 ? `${accent}0.8)` : `${accent}0.15)`,
                }}>{n}</span>
              )}
            </span>
          );
        })}
      </div>
    </div>
  );
}

function renderFact(fact: Fact, search: (q: string) => void, dispatch?: Dispatch<Action>) {
  return (
    <div style={{ padding: '24px 28px', overflowY: 'auto', height: '100%', boxSizing: 'border-box' }}>
      {/* Header */}
      <div style={{ marginBottom: 20 }}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start' }}>
          <div style={{ fontSize: 18, fontWeight: 600, color: '#eee', letterSpacing: '-0.3px' }}>
            {fact.title || fact.path}
          </div>
          {dispatch && (
            <button
              title="Find similar facts"
              onClick={() => dispatch({ type: 'SIMILAR_SEARCH', path: fact.path, text: fact.body || '' })}
              style={{
                background: '#1a1a2a', border: '1px solid rgba(136,170,255,0.2)', color: '#8af',
                padding: '4px 8px', borderRadius: 4, cursor: 'pointer', fontSize: 14,
                transition: 'border-color 0.15s, color 0.15s', flexShrink: 0,
              }}
              onMouseEnter={e => { e.currentTarget.style.borderColor = 'rgba(136,170,255,0.5)'; e.currentTarget.style.color = '#adf'; }}
              onMouseLeave={e => { e.currentTarget.style.borderColor = 'rgba(136,170,255,0.2)'; e.currentTarget.style.color = '#8af'; }}
            >≈</button>
          )}
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
      <div style={{ color: '#ccc', lineHeight: 1.7, fontSize: 14, marginBottom: 8 }}>
        <ReactMarkdown>{fact.body || ''}</ReactMarkdown>
      </div>


      <TagCloud label="Domains" entries={fact.domain || []} color="119,204,153" searchPrefix="domain:" onSearch={search} />
      <TagCloud label="Entities" entries={fact.entities || []} color="136,170,255" searchPrefix="entity:" onSearch={search} />

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

export function RightPanel({ state, dispatch }: Props) {
  const [fact, setFact] = useState<Fact | null>(null);
  const [history, setHistory] = useState<HistoryEntry[]>([]);
  const [stats, setStats] = useState<Stats | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [commitDetail, setCommitDetail] = useState<CommitDetail | null>(null);
  const [commitSelectedFile, setCommitSelectedFile] = useState<string | null>(null);

  useEffect(() => {
    setError(null);
    if (state.historyCommit) {
      // Time-travel: fetch commit detail and auto-load single file
      api.commitDetail(state.historyCommit).then(detail => {
        setCommitDetail(detail);
        const viewableFiles = detail.files.filter(f => f.action !== 'deleted');
        if (viewableFiles.length === 1) {
          setCommitSelectedFile(viewableFiles[0].path);
          api.fact(viewableFiles[0].path, state.historyCommit!).then(setFact).catch(e => setError(String(e)));
        } else {
          setFact(null);
          setCommitSelectedFile(null);
        }
      }).catch(() => setCommitDetail(null));
    } else if (state.rightMode === 'fact' && state.selectedFact) {
      api.fact(state.selectedFact).then(setFact).catch(e => setError(String(e)));
    } else if (state.rightMode === 'history' && state.selectedFact) {
      api.history(state.selectedFact).then(r => setHistory(r.entries || [])).catch(e => setError(String(e)));
    } else if (state.rightMode === 'summary') {
      api.stats(state.previewPath ?? state.currentPath).then(setStats).catch(() => setStats(null));
    }
  }, [state.rightMode, state.selectedFact, state.currentPath, state.previewPath, state.headCommit, state.historyCommit]);

  const search = (q: string) => dispatch({ type: 'SEARCH', query: q });

  if (error) return <div style={{ padding: 24, color: '#f44' }}>{error}</div>;

  // Time-travel: single file auto-loaded → show fact normally
  if (state.historyCommit && fact) {
    return renderFact(fact, search);
  }

  // Time-travel: multiple files → show file list in the right panel
  if (state.historyCommit && commitDetail) {
    return (
      <div style={{ padding: '24px 28px', overflowY: 'auto', height: '100%', boxSizing: 'border-box' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 16 }}>
          <span style={{ color: '#7c9', fontFamily: 'monospace', fontSize: 12 }}>{commitDetail.commit.slice(0, 7)}</span>
          <span style={{ color: '#666', fontSize: 11 }}>{relativeTime(commitDetail.date)}</span>
          {commitDetail.tags.map(tag => {
            const tc = tagColor(tag);
            return <span key={tag} style={{ color: tc.color, background: tc.bg, padding: '1px 6px', borderRadius: 3, fontSize: 10, fontFamily: 'monospace' }}>{tag}</span>;
          })}
        </div>
        <div style={{ color: '#888', fontSize: 12, marginBottom: 12 }}>{commitDetail.message}</div>
        {(!commitDetail.files || commitDetail.files.length === 0) && (
          <div style={{ color: '#555', fontSize: 12, fontStyle: 'italic' }}>No file changes in this commit.</div>
        )}
        {(commitDetail.files || []).map(f => (
          <div key={f.path}
            onClick={() => {
              if (f.action === 'deleted') return;
              setCommitSelectedFile(f.path);
              api.fact(f.path, state.historyCommit!).then(setFact).catch(() => setFact(null));
            }}
            style={{
              padding: '8px 12px', cursor: f.action === 'deleted' ? 'default' : 'pointer',
              background: commitSelectedFile === f.path ? '#2a2a3a' : 'transparent',
              borderBottom: '1px solid #222', display: 'flex', alignItems: 'center', gap: 8,
              opacity: f.action === 'deleted' ? 0.5 : 1,
            }}>
            <span style={{
              fontSize: 9, padding: '1px 4px', borderRadius: 2, fontFamily: 'monospace',
              color: f.action === 'added' ? '#7c9' : f.action === 'modified' ? '#8af' : '#f88',
              background: f.action === 'added' ? '#1a2e1a' : f.action === 'modified' ? '#1a1a2e' : '#2e1a1a',
            }}>{f.action[0].toUpperCase()}</span>
            <span style={{ fontSize: 12, color: '#ddd', fontFamily: 'monospace' }}>{f.path.replace(/\.md$/, '')}</span>
          </div>
        ))}
      </div>
    );
  }

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

            <TagCloud label="Domains" entries={domainEntries} color="119,204,153" searchPrefix="domain:" onSearch={search} />
            <TagCloud label="Entities" entries={entityEntries} color="136,170,255" searchPrefix="entity:" onSearch={search} />
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

  return renderFact(fact, search, dispatch);
}
