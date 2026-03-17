import { useEffect, useState } from 'react';
import type { Dispatch } from 'react';
import ReactMarkdown from 'react-markdown';
import { api } from './api';
import type { Fact, HistoryEntry, Stats, CommitDetail, CommitFile } from './api';
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

type FocusTarget =
  | { kind: 'switcher' }
  | { kind: 'domain'; value: string }
  | { kind: 'entity'; value: string }
  | { kind: 'ref'; value: string }
  | { kind: 'similar' };

// hasSwitcher: true when the commit view has multiple viewable files.
// hasSimilar: true when the "Find similar" button will actually be rendered
//   (only in fact view where dispatch is passed to renderFact, NOT in the time-travel multi-file path).
function buildFocusTargets(f: Fact | null, hasSwitcher: boolean, hasSimilar: boolean): FocusTarget[] {
  const targets: FocusTarget[] = [];
  if (hasSwitcher) targets.push({ kind: 'switcher' });
  if (!f) return targets;
  const domainNames: string[] = (f.domain ?? []).map((d: string | [string, number]) => typeof d === 'string' ? d : d[0]);
  const entityNames: string[] = (f.entities ?? []).map((e: string | [string, number]) => typeof e === 'string' ? e : e[0]);
  for (const d of domainNames) targets.push({ kind: 'domain', value: d });
  for (const e of entityNames) targets.push({ kind: 'entity', value: e });
  for (const r of (f.refs ?? [])) {
    if (r.startsWith('http://') || r.startsWith('https://')) targets.push({ kind: 'ref', value: r });
  }
  if (hasSimilar) targets.push({ kind: 'similar' });
  return targets;
}

interface SwitcherProps {
  files: CommitFile[];
  selectedPath: string | null;
  onSelect: (path: string) => void;
  focusIdx: number;       // 0 = trigger is focused (when right panel focused); -1 = not focused
  dropdownFocusIdx: number; // index within dropdown when open; -1 = none
  open: boolean;
  onToggle: () => void;
}

function FactSwitcher({ files, selectedPath, onSelect, focusIdx, dropdownFocusIdx, open, onToggle }: SwitcherProps) {
  const viewable = files.filter(f => f.action !== 'deleted');
  const currentIdx = viewable.findIndex(f => f.path === selectedPath);
  const current = viewable[currentIdx] ?? viewable[0] ?? null;

  const actionStyle = (action: string): React.CSSProperties => ({
    fontSize: 9, padding: '1px 5px', borderRadius: 3, fontFamily: 'monospace', fontWeight: 600,
    color: action === 'added' ? '#7c9' : action === 'modified' ? '#8af' : '#f88',
    background: action === 'added' ? '#1a2e1a' : action === 'modified' ? '#1a1a2e' : '#2e1a1a',
    ...(action === 'deleted' ? { opacity: 0.5 } : {}),
  });

  const triggerFocused = focusIdx === 0;

  return (
    <div style={{ margin: '10px 16px 12px' }}>
      <div
        onClick={onToggle}
        style={{
          display: 'flex', alignItems: 'center', gap: 8, padding: '7px 10px',
          background: triggerFocused ? '#2a2a3a' : '#1a1a2a',
          border: '1px solid #2a2a3a',
          borderRadius: open ? '6px 6px 0 0' : 6,
          cursor: 'pointer', userSelect: 'none' as const,
        }}
      >
        {current && <span style={actionStyle(current.action)}>{current.action[0].toUpperCase()}</span>}
        <span style={{ flex: 1, fontSize: 12, color: '#ddd', fontFamily: 'monospace', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
          {current ? current.path.replace(/\.md$/, '') : '—'}
        </span>
        <span style={{ fontSize: 11, color: '#555', flexShrink: 0 }}>
          {currentIdx + 1} / {viewable.length}
        </span>
        <span style={{ fontSize: 11, color: '#555', flexShrink: 0 }}>{open ? '▴' : '▾'}</span>
      </div>

      {open && (
        <div style={{ background: '#1a1a2a', border: '1px solid #2a2a3a', borderTop: 'none', borderRadius: '0 0 6px 6px', overflow: 'hidden' }}>
          {files.map((f, i) => {
            const isDeleted = f.action === 'deleted';
            const isSelected = f.path === selectedPath;
            const isDdFocused = dropdownFocusIdx === i;
            return (
              <div
                key={f.path}
                onClick={() => { if (isDeleted) return; onSelect(f.path); onToggle(); }}
                style={{
                  display: 'flex', alignItems: 'center', gap: 8, padding: '7px 10px',
                  cursor: isDeleted ? 'default' : 'pointer',
                  opacity: isDeleted ? 0.4 : 1,
                  background: isDdFocused ? '#2a2a3a' : isSelected ? '#222233' : 'transparent',
                  outline: isDdFocused ? '1px solid rgba(136,170,255,0.3)' : 'none',
                  outlineOffset: -1,
                }}
              >
                <span style={actionStyle(f.action)}>{f.action[0].toUpperCase()}</span>
                <span style={{ fontSize: 12, color: '#ddd', fontFamily: 'monospace' }}>{f.path.replace(/\.md$/, '')}</span>
              </div>
            );
          })}
        </div>
      )}
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
  const [focusIdx, setFocusIdx] = useState(0);
  const [switcherOpen, setSwitcherOpen] = useState(false);
  const [dropdownFocusIdx, setDropdownFocusIdx] = useState(-1);

  const hasSwitcher = !!(state.historyCommit && commitDetail && commitDetail.files.filter(f => f.action !== 'deleted').length > 1);

  useEffect(() => {
    setError(null);
    if (state.historyCommit) {
      // Time-travel: fetch commit detail and auto-load single file
      api.commitDetail(state.repo, state.historyCommit).then(detail => {
        setFocusIdx(0);
        setSwitcherOpen(false);
        setDropdownFocusIdx(-1);
        setCommitDetail(detail);
        const viewableFiles = detail.files.filter(f => f.action !== 'deleted');
        if (viewableFiles.length === 1) {
          setCommitSelectedFile(viewableFiles[0].path);
          api.fact(state.repo, viewableFiles[0].path, state.historyCommit!).then(setFact).catch(e => setError(String(e)));
        } else {
          setFact(null);
          setCommitSelectedFile(null);
        }
      }).catch(() => setCommitDetail(null));
    } else if (state.rightMode === 'fact' && state.selectedFact) {
      api.fact(state.repo, state.selectedFact).then(setFact).catch(e => setError(String(e)));
    } else if (state.rightMode === 'history' && state.selectedFact) {
      api.history(state.repo, state.selectedFact).then(r => setHistory(r.entries || [])).catch(e => setError(String(e)));
    } else if (state.rightMode === 'summary') {
      api.stats(state.repo, state.previewPath ?? state.currentPath).then(setStats).catch(() => setStats(null));
    }
  }, [state.rightMode, state.selectedFact, state.currentPath, state.previewPath, state.headCommit, state.historyCommit]);

  const search = (q: string) => dispatch({ type: 'SEARCH', query: q });

  if (error) return <div style={{ padding: 24, color: '#f44' }}>{error}</div>;

  // Time-travel: single file auto-loaded → show fact normally (no switcher)
  if (state.historyCommit && fact && !hasSwitcher) {
    return renderFact(fact, search);
  }

  // Time-travel: multiple viewable files → show FactSwitcher + selected fact below
  if (state.historyCommit && commitDetail) {
    const viewable = commitDetail.files.filter(f => f.action !== 'deleted');

    return (
      <div
        onClick={() => dispatch({ type: 'FOCUS_RIGHT_PANEL' })}
        style={{ display: 'flex', flexDirection: 'column', height: '100%', overflowY: 'auto', boxSizing: 'border-box' }}
      >
        {/* Commit header */}
        <div style={{ padding: '16px 20px 8px', borderBottom: '1px solid #222', display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap', flexShrink: 0 }}>
          <span style={{ color: '#7c9', fontFamily: 'monospace', fontSize: 12 }}>{commitDetail.commit.slice(0, 7)}</span>
          <span style={{ color: '#666', fontSize: 11 }}>{relativeTime(commitDetail.date)}</span>
          {commitDetail.tags.map(tag => {
            const tc = tagColor(tag);
            return <span key={tag} style={{ color: tc.color, background: tc.bg, padding: '1px 6px', borderRadius: 3, fontSize: 10, fontFamily: 'monospace' }}>{tag}</span>;
          })}
        </div>
        <div style={{ color: '#888', fontSize: 12, padding: '6px 20px 0', flexShrink: 0 }}>{commitDetail.message}</div>

        {viewable.length > 1 && (
          <FactSwitcher
            files={commitDetail.files}
            selectedPath={commitSelectedFile}
            onSelect={path => {
              setCommitSelectedFile(path);
              api.fact(state.repo, path, state.historyCommit!).then(setFact).catch(() => setFact(null));
              setFocusIdx(0);
            }}
            focusIdx={state.rightPanelFocused ? focusIdx : -1}
            dropdownFocusIdx={dropdownFocusIdx}
            open={switcherOpen}
            onToggle={() => setSwitcherOpen(o => !o)}
          />
        )}

        {fact && <div style={{ flex: 1 }}>{renderFact(fact, search)}</div>}
        {!fact && viewable.length > 0 && (
          <div style={{ padding: '16px 20px', color: '#666', fontSize: 13 }}>Select a fact above.</div>
        )}
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
