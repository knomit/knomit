import { useCallback, useEffect, useRef, useState } from 'react';
import type { Dispatch, ReactNode } from 'react';
import { useAsync } from './hooks';
import ReactMarkdown from 'react-markdown';
import { api } from './api';
import type { Fact, Stats, ActivityStats, CommitDetail } from './api';
import type { AppState, Action } from './state';
import { currentPath } from './state';
import { relativeTime, typeStyles, defaultTypeStyle, opStyles, defaultOpStyle } from './utils';
import { TypeIcon, EpisodeIcon, RetractIcon, ExplainIcon } from './icons';
import type { NavRequest } from './useNavigationManager';

function StatBox({ label, value, color }: { label: string; value: ReactNode; color: string }) {
  return (
    <div style={{ borderLeft: `3px solid ${color}`, padding: '10px 16px', background: '#1a1a2a', borderRadius: '0 6px 6px 0', minWidth: 90 }}>
      <div style={{ fontSize: 10, color: '#666', textTransform: 'uppercase', letterSpacing: 1 }}>{label}</div>
      <div style={{ fontSize: 22, fontWeight: 600, color: '#eee', marginTop: 2 }}>{value}</div>
    </div>
  );
}

function TagCloud({ label, entries, color, onTagClick, focusedValue }: {
  label: string;
  entries: [string, number][] | string[];
  color: string;
  onTagClick: (value: string) => void;
  focusedValue?: string;
}) {
  if (entries.length === 0) return null;

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
            <span key={name} data-testid="tag-item" data-value={name}
              onClick={() => onTagClick(name)}
              style={{
                display: 'inline-flex', alignItems: 'center', gap: 5, cursor: 'pointer',
                padding: weighted && ratio >= 0.75 ? '5px 11px' : weighted ? '4px 9px' : '5px 11px',
                borderRadius: 6,
                background: weighted && ratio < 0.5 ? 'rgba(26,26,42,0.6)' : '#1a1a2a',
                border: `1px solid ${accent}${weighted ? (ratio >= 0.75 ? 0.3 : ratio >= 0.5 ? 0.2 : 0.1) : 0.2})`,
                transition: 'border-color 0.15s, opacity 0.15s',
                outline: name === focusedValue ? `2px solid rgba(${color},0.55)` : 'none',
                outlineOffset: 1,
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

function renderFact(fact: Fact, navigate: (req: NavRequest) => void, dispatch: Dispatch<Action>, onRetract?: () => void, onExplain?: () => void) {
  return (
    <div style={{ padding: '24px 28px', overflowY: 'auto', height: '100%', boxSizing: 'border-box' }}>
      <div style={{ marginBottom: 20 }}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start' }}>
          <div data-testid="fact-title" style={{ fontSize: 18, fontWeight: 600, color: '#eee', letterSpacing: '-0.3px', flex: 1, minWidth: 0 }}>
            {fact.title || fact.path}
          </div>
          <span style={{ display: 'flex', alignItems: 'center', gap: 6, flexShrink: 0, marginTop: 4 }}>
            {fact.commit_date && (
              <span title={new Date(fact.commit_date).toLocaleString()} style={{ color: '#555', fontSize: 11 }}>
                {relativeTime(fact.commit_date)}
              </span>
            )}
            {fact.commit_hash && (
              <span
                title={`Committed at ${fact.commit_hash.slice(0, 7)}`}
                style={{ color: '#7c9', fontFamily: 'monospace', fontSize: 11, background: '#1a2e1a', padding: '1px 5px', borderRadius: 3 }}
              >
                {fact.commit_hash.slice(0, 7)}
              </span>
            )}
            {onExplain && (
              <button
                title="Explain"
                onClick={onExplain}
                style={{ background: 'none', border: 'none', padding: 2, color: '#8af', cursor: 'pointer', display: 'flex', alignItems: 'center', opacity: 0.6 }}
                onMouseEnter={e => { (e.currentTarget as HTMLElement).style.opacity = '1'; }}
                onMouseLeave={e => { (e.currentTarget as HTMLElement).style.opacity = '0.6'; }}
              ><ExplainIcon color="currentColor" size={15} /></button>
            )}
            {onRetract && (
              <button
                data-testid="retract-btn"
                title="Retract fact"
                onClick={onRetract}
                style={{
                  background: 'none', border: 'none', padding: 2,
                  color: '#f66', cursor: 'pointer', display: 'flex', alignItems: 'center', opacity: 0.6,
                }}
                onMouseEnter={e => { (e.currentTarget as HTMLElement).style.opacity = '1'; }}
                onMouseLeave={e => { (e.currentTarget as HTMLElement).style.opacity = '0.6'; }}
              ><RetractIcon color="#f66" size={15} /></button>
            )}
          </span>
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginTop: 4 }}>
          {fact.type && (() => {
            const ts = typeStyles[fact.type] || defaultTypeStyle;
            return (
              <span data-testid="fact-type-badge" style={{
                color: ts.color, background: ts.bg, fontSize: 10, padding: '2px 8px',
                borderRadius: 3, fontFamily: 'monospace', letterSpacing: 0.5,
                border: fact.type === 'hypothesis' ? `1px dashed ${ts.color}` : 'none',
                display: 'inline-flex', alignItems: 'center', gap: 4,
              }}><TypeIcon type={fact.type} color={ts.color} size={10} /> {ts.label}</span>
            );
          })()}
          <span
            onClick={() => fact.commit_hash
              ? navigate({ view: 'history', historyCommit: fact.commit_hash, factPath: fact.path, factCommit: fact.commit_hash })
              : navigate({ view: 'history' })
            }
            style={{ fontSize: 12, color: '#555', cursor: 'pointer', fontFamily: 'monospace' }}
            onMouseEnter={e => { (e.currentTarget as HTMLElement).style.color = '#8af'; }}
            onMouseLeave={e => { (e.currentTarget as HTMLElement).style.color = '#555'; }}
          >{fact.path}</span>
        </div>
      </div>

      <div data-testid="fact-meta" style={{ display: 'flex', gap: 10, marginBottom: 28 }}>
        <StatBox label="Confidence" value={fact.confidence?.toFixed(2)} color="#8af" />
        <StatBox label="Sources" value={fact.sources} color="#7c9" />
      </div>

      <div data-testid="fact-body" style={{ color: '#ccc', lineHeight: 1.7, fontSize: 14, marginBottom: 8 }}>
        <ReactMarkdown>{fact.body || ''}</ReactMarkdown>
      </div>

      <TagCloud label="Domains" entries={fact.domain || []} color="119,204,153"
        onTagClick={d => dispatch({ type: 'ADD_FILTER', chip: { category: 'domain', value: d } })} />
      <TagCloud label="Entities" entries={fact.entities || []} color="136,170,255"
        onTagClick={e => dispatch({ type: 'ADD_FILTER', chip: { category: 'entity', value: e } })} />

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
                  >{'\u2197'} {ref}</a>
                );
              }
              // Local ref: open at the same commit as the current fact (time-travel).
              const commit = fact.commit_hash;
              return (
                <span key={ref}
                  onClick={() => commit
                    ? navigate({ view: 'history', historyCommit: commit, factPath: ref, factCommit: commit })
                    : navigate({ view: 'tree', factPath: ref })
                  }
                  style={{ color: '#8af', fontSize: 12, fontFamily: 'monospace', cursor: 'pointer', transition: 'color 0.15s' }}
                  onMouseEnter={e => { (e.currentTarget as HTMLElement).style.color = '#adf'; }}
                  onMouseLeave={e => { (e.currentTarget as HTMLElement).style.color = '#8af'; }}
                >{'\u2192'} {ref}</span>
              );
            })}
          </div>
        </div>
      )}
    </div>
  );
}

function FactEditor({ fact, repo, branch, onSaved }: { fact: Fact; repo: string; branch: string; onSaved: (updated: Fact) => void }) {
  const [raw, setRaw] = useState(fact.body);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);

  const save = () => {
    setSaving(true);
    setSaveError(null);
    api.updateFact(repo, branch, fact.path, raw)
      .then(updated => { setSaving(false); onSaved(updated); })
      .catch(e => { setSaving(false); setSaveError(String(e)); });
  };

  return (
    <div style={{ padding: '24px 28px', overflowY: 'auto', height: '100%', boxSizing: 'border-box', display: 'flex', flexDirection: 'column', gap: 16 }}>
      <div style={{ background: '#2e1a1a', border: '1px solid rgba(255,80,80,0.3)', borderRadius: 6, padding: '10px 14px' }}>
        <div style={{ color: '#f88', fontSize: 11, textTransform: 'uppercase', letterSpacing: 1.2, marginBottom: 4 }}>Parse error</div>
        <div style={{ color: '#f44', fontSize: 12, fontFamily: 'monospace' }}>{fact.parse_error}</div>
      </div>
      <div style={{ fontSize: 12, color: '#555' }}>{fact.path}</div>
      <textarea
        data-testid="fact-editor"
        value={raw}
        onChange={e => setRaw(e.target.value)}
        spellCheck={false}
        style={{
          flex: 1, minHeight: 320, background: '#0d0d14', color: '#ccc', border: '1px solid #2a2a3a',
          borderRadius: 6, padding: '12px 14px', fontFamily: 'monospace', fontSize: 12,
          lineHeight: 1.6, resize: 'none', outline: 'none', boxSizing: 'border-box', width: '100%',
        }}
      />
      <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
        <button
          data-testid="fact-save-btn"
          onClick={save}
          disabled={saving}
          style={{
            background: '#1a2e1a', border: '1px solid rgba(119,204,153,0.35)', color: '#7c9',
            padding: '6px 16px', borderRadius: 4, cursor: saving ? 'default' : 'pointer',
            fontSize: 13, opacity: saving ? 0.6 : 1,
          }}
        >{saving ? 'Saving\u2026' : 'Save'}</button>
        {saveError && <span style={{ color: '#f88', fontSize: 12 }}>{saveError}</span>}
      </div>
    </div>
  );
}


// ─── Commit Panel (history mode) ─────────────────────────────────────────────

const ROW_HEIGHT = 26;
const DEFAULT_LIST_HEIGHT = 3 * ROW_HEIGHT;
const MIN_LIST_HEIGHT = ROW_HEIGHT;
const MAX_LIST_HEIGHT = 12 * ROW_HEIGHT;

function CommitPanel({ historyCommit, repo, branch, selectedFact, navigate, rightPanelFocused, dispatch }: {
  historyCommit: string;
  repo: string;
  branch: string;
  selectedFact: string | null;
  navigate: (req: NavRequest) => void;
  rightPanelFocused: boolean;
  dispatch: Dispatch<Action>;
}) {
  const [detail, setDetail] = useState<CommitDetail | null>(null);
  const listRef = useRef<HTMLDivElement>(null);
  const itemRefs = useRef<(HTMLDivElement | null)[]>([]);
  const [listHeight, setListHeight] = useState(DEFAULT_LIST_HEIGHT);
  const draggingRef = useRef(false);

  useAsync((stale) => {
    api.commitDetail(repo, branch, historyCommit)
      .then(d => { if (!stale()) setDetail(d); })
      .catch(() => { if (!stale()) setDetail(null); });
  }, [historyCommit, repo]);

  const files = detail?.files || [];
  const hasOverflow = files.length * ROW_HEIGHT > listHeight;

  // Scroll selected file into view
  const activeIdx = files.findIndex(f => f.path === selectedFact);
  useEffect(() => {
    if (activeIdx >= 0) {
      itemRefs.current[activeIdx]?.scrollIntoView({ block: 'nearest' });
    }
  }, [activeIdx]);

  // Auto-select first file when no fact is open or the current fact isn't in this commit.
  useEffect(() => {
    if (!detail) return;
    if (selectedFact && detail.files?.some(f => f.path === selectedFact)) return;
    const first = detail.files?.[0];
    if (first) dispatch({ type: 'AMEND_NAV', historyCommit, factPath: first.path, factCommit: historyCommit });
  }, [detail, selectedFact, historyCommit, dispatch]);

  useEffect(() => { setListHeight(DEFAULT_LIST_HEIGHT); }, [historyCommit]);

  // Keyboard navigation within commit files
  useEffect(() => {
    if (!rightPanelFocused) return;
    const handler = (e: KeyboardEvent) => {
      if (files.length === 0) return;
      if ((e.key === 'ArrowDown' || e.key === 'j' || e.key === 'ArrowUp' || e.key === 'k') && files.length > 1) {
        e.preventDefault();
        const currentIdx = files.findIndex(f => f.path === selectedFact);
        const delta = (e.key === 'ArrowDown' || e.key === 'j') ? 1 : -1;
        const nextIdx = Math.max(0, Math.min(currentIdx + delta, files.length - 1));
        if (nextIdx !== currentIdx) {
          navigate({ view: 'history', historyCommit, factPath: files[nextIdx].path, factCommit: historyCommit });
        }
      }
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [rightPanelFocused, detail, selectedFact, historyCommit, navigate]);

  // Drag to resize
  const startDrag = (e: React.MouseEvent) => {
    e.preventDefault();
    draggingRef.current = true;
    const startY = e.clientY;
    const startH = listHeight;
    const onMove = (ev: MouseEvent) => {
      if (!draggingRef.current) return;
      const delta = ev.clientY - startY;
      setListHeight(Math.max(MIN_LIST_HEIGHT, Math.min(MAX_LIST_HEIGHT, startH + delta)));
    };
    const onUp = () => {
      draggingRef.current = false;
      document.removeEventListener('mousemove', onMove);
      document.removeEventListener('mouseup', onUp);
    };
    document.addEventListener('mousemove', onMove);
    document.addEventListener('mouseup', onUp);
  };

  if (!detail) return null;

  // Episode tag
  const op = detail.operation || '';
  const os = op && opStyles[op] ? opStyles[op] : defaultOpStyle;

  return (
    <div style={{ flexShrink: 0, background: '#141414' }}>
      <div style={{ display: 'flex', borderBottom: '1px solid #2a2a2a' }}>
        <div style={{ width: 4, background: os.color, flexShrink: 0 }} />
        <div style={{ flex: 1, padding: '8px 14px' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 3 }}>
            <EpisodeIcon op={op} color={os.color} size={14} />
            <span style={{ fontSize: 11, color: os.color, fontWeight: 600, textTransform: 'uppercase', letterSpacing: 0.5 }}>{os.label || op}</span>
            <span style={{ fontSize: 10, color: '#555', fontFamily: 'monospace', marginLeft: 'auto' }}>{detail.commit.slice(0, 7)} · {relativeTime(detail.date)}</span>
          </div>
          <div style={{ fontSize: 12, color: '#bbb', lineHeight: 1.4 }}>{detail.message}</div>
        </div>
      </div>

      <div style={{ display: 'flex' }}>
        <div
          ref={listRef}
          style={{
            height: Math.min(listHeight, files.length * ROW_HEIGHT),
            maxHeight: listHeight,
            overflowY: hasOverflow ? 'auto' : 'hidden',
            flex: 1,
          }}
        >
          {files.map((file, idx) => {
            const isActive = selectedFact === file.path;
            const opColor = file.action === 'added' ? '#7c9' : file.action === 'deleted' ? '#f88' : '#8af';
            const opIndicator = file.action === 'added' ? '+' : file.action === 'deleted' ? '\u2212' : '~';
            const displayName = file.title || file.path.split('/').pop()?.replace(/\.md$/, '') || file.path;
            return (
              <div
                key={file.path}
                ref={el => { itemRefs.current[idx] = el; }}
                data-testid="commit-file"
                data-path={file.path}
                onClick={() => {
                  navigate({ view: 'history', historyCommit, factPath: file.path, factCommit: historyCommit });
                  dispatch({ type: 'FOCUS_RIGHT_PANEL' });
                }}
                style={{
                  height: ROW_HEIGHT,
                  display: 'flex',
                  alignItems: 'center',
                  gap: 6,
                  padding: '0 14px',
                  fontSize: 11,
                  cursor: 'pointer',
                  color: isActive ? '#fff' : '#aaa',
                  background: isActive ? '#14141e' : 'transparent',
                  borderLeft: isActive ? `4px solid #8af` : `4px solid transparent`,
                }}
                onMouseEnter={e => { if (!isActive) e.currentTarget.style.background = '#222'; }}
                onMouseLeave={e => { if (!isActive) e.currentTarget.style.background = isActive ? '#22223a' : 'transparent'; }}
              >
                <span style={{ color: opColor, fontWeight: 'bold', fontFamily: 'monospace', width: 12, textAlign: 'center', flexShrink: 0 }}>{opIndicator}</span>
                <span title={displayName} style={{
                  fontWeight: isActive ? 500 : 400,
                  overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', flex: 1, minWidth: 0,
                }}>{displayName}</span>
              </div>
            );
          })}
        </div>

      </div>

      <div
        onMouseDown={startDrag}
        style={{
          height: 5,
          cursor: 'ns-resize',
          background: 'transparent',
          borderBottom: '1px solid #333',
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
        }}
      >
        <div style={{ width: 30, height: 2, borderRadius: 1, background: '#444' }} />
      </div>
    </div>
  );
}

// ─── Confirm Modal ───────────────────────────────────────────────────────────

function ConfirmModal({ message, onConfirm, onCancel }: {
  message: string;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  // Close on Escape
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onCancel();
      if (e.key === 'Enter') onConfirm();
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [onConfirm, onCancel]);

  return (
    <div
      onClick={onCancel}
      style={{
        position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.6)',
        display: 'flex', alignItems: 'center', justifyContent: 'center', zIndex: 1000,
      }}
    >
      <div
        onClick={e => e.stopPropagation()}
        style={{
          background: '#1a1a2a', border: '1px solid #333', borderRadius: 8,
          padding: '24px 28px', maxWidth: 400, width: '90%', boxShadow: '0 8px 32px rgba(0,0,0,0.6)',
        }}
      >
        <div style={{ fontSize: 13, color: '#ccc', lineHeight: 1.6, marginBottom: 20 }}>{message}</div>
        <div style={{ display: 'flex', gap: 10, justifyContent: 'flex-end' }}>
          <button
            onClick={onCancel}
            style={{
              background: 'none', border: '1px solid #333', borderRadius: 4,
              color: '#888', cursor: 'pointer', padding: '6px 16px', fontSize: 12,
            }}
          >Cancel</button>
          <button
            data-testid="retract-confirm-btn"
            onClick={onConfirm}
            style={{
              background: '#2e1a1a', border: '1px solid rgba(255,80,80,0.4)', borderRadius: 4,
              color: '#f66', cursor: 'pointer', padding: '6px 16px', fontSize: 12,
            }}
          >Retract</button>
        </div>
      </div>
    </div>
  );
}

// ─── Main RightPanel ─────────────────────────────────────────────────────────

export function RightPanel({ state, dispatch, navigate, onExplain }: {
  state: AppState;
  dispatch: Dispatch<Action>;
  navigate: (req: NavRequest) => void;
  onExplain?: (path: string, commit: string | null) => void;
}) {
  const [fact, setFact] = useState<Fact | null>(null);
  const [stats, setStats] = useState<Stats | null>(null);
  const [activity, setActivity] = useState<ActivityStats | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [retracting, setRetracting] = useState(false);
  const [confirmRetract, setConfirmRetract] = useState(false);
  const path = currentPath(state);

  const factPath = state.factPath;
  const factCommit = state.factCommit;
  const historyCommit = state.historyCommit;

  useAsync((stale) => {
    if (!factPath) { setFact(null); setError(null); return; }
    setError(null);
    api.fact(state.repo, state.branch, factPath, factCommit ?? undefined)
      .then(f => {
        if (stale()) return;
        setFact(f);
        if (f.commit_hash) dispatch({ type: 'FACT_LOADED', commit: f.commit_hash });
      })
      .catch(e => { if (!stale()) setError(String(e)); });
  }, [factPath, factCommit, state.repo]);

  useAsync((stale) => {
    if (factPath || state.view === 'history') return;
    Promise.all([
      api.stats(state.repo, state.branch, path).catch(() => null),
      api.activity(state.repo, state.branch, path).catch(() => null),
    ]).then(([s, a]) => {
      if (stale()) return;
      setStats(s);
      setActivity(a);
    });
  }, [factPath, state.repo, path, state.headCommit]);

  const doRetract = useCallback(() => {
    if (!fact || retracting) return;
    setConfirmRetract(false);
    setRetracting(true);
    api.retractFact(state.repo, state.branch, fact.path)
      .then(() => {
        setRetracting(false);
        // Clear the fact without touching headCommit. The git observer will
        // sync the index and then broadcast a status event with the new commit
        // hash, which triggers SET_HEAD in App.tsx. Only then will headCommit
        // change, ensuring the search/chrono re-fire against a fresh index.
        dispatch({ type: 'AMEND_NAV', historyCommit: null, factPath: null, factCommit: null });
      })
      .catch(e => { setRetracting(false); setError(String(e)); });
  }, [fact, retracting, state.repo, dispatch]);

  // Keyboard: ArrowLeft blurs right panel; j/k navigation is handled inside CommitPanel
  useEffect(() => {
    if (!state.rightPanelFocused) return;
    const handler = (e: KeyboardEvent) => {
      if (e.key === 'ArrowLeft') {
        e.preventDefault();
        dispatch({ type: 'BLUR_RIGHT_PANEL' });
      }
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [state.rightPanelFocused, dispatch]);

  if (error) return <div style={{ padding: 24, color: '#f44' }}>{error}</div>;

  const commitPanel = state.view === 'history' && historyCommit
    ? <CommitPanel historyCommit={historyCommit} repo={state.repo} branch={state.branch} selectedFact={factPath} navigate={navigate} rightPanelFocused={state.rightPanelFocused} dispatch={dispatch} />
    : null;

  // Summary view: no fact selected
  if (!factPath) {
    const domainEntries = stats ? Object.entries(stats.domains).sort((a, b) => b[1] - a[1]).slice(0, 10) : [];
    const entityEntries = stats ? Object.entries(stats.entities).sort((a, b) => b[1] - a[1]).slice(0, 10) : [];
    const domainCount = stats ? Object.keys(stats.domains).length : 0;
    const entityCount = stats ? Object.keys(stats.entities).length : 0;
    const totalCommits = activity ? String(activity.total) : '\u2014';

    return (
      <div style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
        {commitPanel}
        <div data-testid="stats-view" style={{ flex: 1, padding: '24px 28px', overflowY: 'auto', boxSizing: 'border-box' }}>
          {stats ? (
            <>
              <div style={{ fontSize: 12, color: '#555', marginBottom: 20, display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                <span>{stats.total} facts across {domainCount} domains</span>
                {activity?.last_commit && (
                  <span title={new Date(activity.last_commit).toLocaleString()} style={{ color: '#555', fontSize: 11 }}>
                    {relativeTime(activity.last_commit)}
                  </span>
                )}
              </div>
              <div style={{ display: 'flex', gap: 10, marginBottom: 28, flexWrap: 'wrap' }}>
                <StatBox label="Facts"      value={stats.total}                       color="#7c9" />
                <StatBox label="Confidence" value={stats.avg_confidence.toFixed(2)}   color="#8af" />
                <StatBox label="Domains"    value={domainCount}                        color="#fa8" />
                <StatBox label="Entities"   value={entityCount}                        color="#8af" />
                <StatBox label="Commits"    value={totalCommits}                       color="#555" />
              </div>
              <TagCloud label="Domains" entries={domainEntries} color="119,204,153"
                onTagClick={d => dispatch({ type: 'ADD_FILTER', chip: { category: 'domain', value: d } })} />
              <TagCloud label="Entities" entries={entityEntries} color="136,170,255"
                onTagClick={e => dispatch({ type: 'ADD_FILTER', chip: { category: 'entity', value: e } })} />
            </>
          ) : <div style={{ color: '#666' }}>No facts indexed in this path.</div>}
        </div>
      </div>
    );
  }

  // Fact view (normal or time-travel)
  if (!fact) return <div style={{ padding: 24, color: '#666' }}>Loading...</div>;

  if (fact.parse_error) return <FactEditor fact={fact} repo={state.repo} branch={state.branch} onSaved={setFact} />;

  const canRetract = state.view !== 'history';

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100%', overflow: 'hidden' }}>
      {confirmRetract && (
        <ConfirmModal
          message={`Are you sure you want to retract "${fact.title || fact.path}"?`}
          onConfirm={doRetract}
          onCancel={() => setConfirmRetract(false)}
        />
      )}
      {commitPanel}
      <div style={{ flex: 1, overflow: 'auto' }}>
        {renderFact(fact, navigate, dispatch, canRetract ? () => setConfirmRetract(true) : undefined, canRetract ? () => onExplain?.(fact.path, null) : undefined)}
      </div>
    </div>
  );
}
