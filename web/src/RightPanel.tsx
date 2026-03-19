import { useEffect, useState } from 'react';
import type { Dispatch } from 'react';
import ReactMarkdown from 'react-markdown';
import { api } from './api';
import type { Fact, HistoryEntry, Stats, CommitDetail, CommitFile, ActivityStats } from './api';
import type { AppState, Action } from './state';
import { relativeTime, opStyles } from './utils';

function commitDetailStyle(detail: CommitDetail): { color: string; bg: string; label: string } | null {
  if (detail.operation && opStyles[detail.operation]) return opStyles[detail.operation];
  return null;
}

function TagCloud({ label, entries, color, searchPrefix, onSearch, focusedValue }: {
  label: string;
  entries: [string, number][] | string[];
  color: string; // e.g. '119,204,153' or '136,170,255'
  searchPrefix: string; // e.g. 'domain:' or 'entity:'
  onSearch: (query: string) => void;
  focusedValue?: string;
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
  onSelect: (path: string, action: string) => void;
  focusIdx: number;       // 0 = trigger is focused (when right panel focused); -1 = not focused
  dropdownFocusIdx: number; // index within dropdown when open; -1 = none
  open: boolean;
  onToggle: () => void;
}

function FactSwitcher({ files, selectedPath, onSelect, focusIdx, dropdownFocusIdx, open, onToggle }: SwitcherProps) {
  const currentIdx = files.findIndex(f => f.path === selectedPath);
  const current = files[currentIdx] ?? files[0] ?? null;

  const actionStyle = (action: string): React.CSSProperties => ({
    fontSize: 9, padding: '1px 5px', borderRadius: 3, fontFamily: 'monospace', fontWeight: 600,
    color: action === 'added' ? '#7c9' : action === 'modified' ? '#8af' : '#f88',
    background: action === 'added' ? '#1a2e1a' : action === 'modified' ? '#1a1a2e' : '#2e1a1a',
  });

  const triggerFocused = focusIdx === 0;
  const hasMultiple = files.length > 1;

  return (
    <div style={{ margin: '10px 16px 12px' }}>
      <div
        onClick={onToggle}
        style={{
          display: 'flex', alignItems: 'center', gap: 8, padding: '7px 10px',
          background: triggerFocused ? '#222233' : '#161622',
          border: '1px solid transparent',
          borderRadius: open ? '6px 6px 0 0' : 6,
          cursor: hasMultiple ? 'pointer' : 'default',
          userSelect: 'none' as const,
        }}
      >
        {current && <span style={actionStyle(current.action)}>{current.action[0].toUpperCase()}</span>}
        <span style={{ flex: 1, fontSize: 12, color: '#ddd', fontFamily: 'monospace', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
          {current ? current.path.replace(/\.md$/, '') : '—'}
        </span>
        {hasMultiple && (
          <>
            <span style={{
              fontSize: 10, padding: '1px 7px', borderRadius: 8,
              color: '#ddd', background: '#3a3a4a', fontWeight: 600,
            }}>
              {currentIdx + 1}/{files.length} facts
            </span>
            <span style={{ fontSize: 11, color: '#888', flexShrink: 0 }}>{open ? '▴' : '▾'}</span>
          </>
        )}
      </div>

      {open && (
        <div style={{ background: '#1a1a2a', border: '1px solid #2a2a3a', borderTop: 'none', borderRadius: '0 0 6px 6px', maxHeight: 200, overflowY: 'auto' }}>
          {files.map((f, i) => {
            const isSelected = f.path === selectedPath;
            const isDdFocused = dropdownFocusIdx === i;
            return (
              <div
                key={f.path}
                onClick={() => { onSelect(f.path, f.action); onToggle(); }}
                style={{
                  display: 'flex', alignItems: 'center', gap: 8, padding: '7px 10px',
                  cursor: 'pointer',
                  background: isDdFocused ? '#2a2a3a' : isSelected ? '#222233' : 'transparent',
                  outline: isDdFocused ? '1px solid rgba(136,170,255,0.3)' : 'none',
                  outlineOffset: -1,
                }}
              >
                <span style={actionStyle(f.action)}>{f.action[0].toUpperCase()}</span>
                <span style={{ fontSize: 12, color: isSelected ? '#ddd' : '#999', fontFamily: 'monospace' }}>{f.path.replace(/\.md$/, '')}</span>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}

interface FocusInfo {
  target: FocusTarget | null;
}

function renderFact(fact: Fact, search: (q: string) => void, dispatch?: Dispatch<Action>, focusInfo?: FocusInfo, historyDate?: string, onFromCommitClick?: (commit: string) => void, onLocalRef?: (path: string) => void) {
  const ft = focusInfo?.target ?? null;
  return (
    <div style={{ padding: '24px 28px', overflowY: 'auto', height: '100%', boxSizing: 'border-box' }}>
      {/* Header */}
      <div style={{ marginBottom: 20 }}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start' }}>
          <div style={{ fontSize: 18, fontWeight: 600, color: '#eee', letterSpacing: '-0.3px', flex: 1, minWidth: 0 }}>
            {fact.title || fact.path}
          </div>
          {historyDate && (
            <span style={{ display: 'flex', alignItems: 'center', gap: 6, flexShrink: 0, marginTop: 4 }}>
              <span title={new Date(historyDate).toLocaleString()} style={{ color: '#666', fontSize: 11 }}>
                {relativeTime(historyDate)}
              </span>
              {fact.from_commit && onFromCommitClick && (
                <span
                  title={`Retracted from commit ${fact.from_commit}`}
                  onClick={() => onFromCommitClick(fact.from_commit!)}
                  style={{ color: '#f88', fontFamily: 'monospace', fontSize: 11, cursor: 'pointer', background: '#2e1a1a', padding: '1px 5px', borderRadius: 3 }}
                  onMouseEnter={e => { (e.currentTarget as HTMLElement).style.background = '#3e2020'; }}
                  onMouseLeave={e => { (e.currentTarget as HTMLElement).style.background = '#2e1a1a'; }}
                >
                  {fact.from_commit.slice(0, 7)}
                </span>
              )}
            </span>
          )}
          {dispatch && (
            <span style={{ display: 'flex', alignItems: 'center', gap: 6, flexShrink: 0, marginTop: 4 }}>
              {fact.commit_date && (
                <span title={new Date(fact.commit_date).toLocaleString()} style={{ color: '#555', fontSize: 11 }}>
                  {relativeTime(fact.commit_date)}
                </span>
              )}
              {fact.commit_hash && (
                <span
                  title={`Go to commit ${fact.commit_hash.slice(0, 7)} in history`}
                  onClick={() => {
                    dispatch({ type: 'FACT_HISTORY', factPath: fact.path, commit: fact.commit_hash! });
                  }}
                  style={{ color: '#7c9', fontFamily: 'monospace', fontSize: 11, cursor: 'pointer', background: '#1a2e1a', padding: '1px 5px', borderRadius: 3 }}
                  onMouseEnter={e => { (e.currentTarget as HTMLElement).style.background = '#1e3820'; }}
                  onMouseLeave={e => { (e.currentTarget as HTMLElement).style.background = '#1a2e1a'; }}
                >
                  {fact.commit_hash.slice(0, 7)}
                </span>
              )}
              {!historyDate && <span
                title="Find similar facts"
                onClick={() => dispatch({ type: 'SIMILAR_SEARCH', path: fact.path, text: fact.body || '' })}
                style={{
                  color: '#555', cursor: 'pointer', fontSize: 11, fontFamily: 'monospace',
                  transition: 'color 0.15s', flexShrink: 0,
                  outline: ft?.kind === 'similar' ? '2px solid rgba(136,170,255,0.55)' : 'none',
                  outlineOffset: 1, borderRadius: 3,
                }}
                onMouseEnter={e => { (e.currentTarget as HTMLElement).style.color = '#8af'; }}
                onMouseLeave={e => { (e.currentTarget as HTMLElement).style.color = '#555'; }}
              >≈</span>}
            </span>
          )}
        </div>
        {dispatch ? (
          <div
            onClick={() => { dispatch({ type: 'FACT_HISTORY', factPath: fact.path }); }}
            style={{ fontSize: 12, color: '#556', marginTop: 2, cursor: 'pointer' }}
            onMouseEnter={e => { (e.currentTarget as HTMLElement).style.color = '#8af'; }}
            onMouseLeave={e => { (e.currentTarget as HTMLElement).style.color = '#556'; }}
          >{fact.path}</div>
        ) : (
          <div style={{ fontSize: 12, color: '#555', marginTop: 2 }}>{fact.path}</div>
        )}
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


      <TagCloud label="Domains" entries={fact.domain || []} color="119,204,153" searchPrefix="domain:" onSearch={search}
        focusedValue={ft?.kind === 'domain' ? ft.value : undefined} />
      <TagCloud label="Entities" entries={fact.entities || []} color="136,170,255" searchPrefix="entity:" onSearch={search}
        focusedValue={ft?.kind === 'entity' ? ft.value : undefined} />

      {/* Refs */}
      {fact.refs?.length > 0 && (
        <div>
          <div style={{ fontSize: 10, textTransform: 'uppercase', letterSpacing: 1.5, color: '#555', marginBottom: 10 }}>References</div>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
            {fact.refs.map(ref => {
              if (ref.startsWith('http://') || ref.startsWith('https://')) {
                return (
                  <a key={ref} href={ref} target="_blank" rel="noopener noreferrer"
                    style={{
                      color: '#8af', fontSize: 12, textDecoration: 'none', transition: 'color 0.15s',
                      outline: ft?.kind === 'ref' && ft.value === ref ? '2px solid rgba(136,170,255,0.55)' : 'none',
                      outlineOffset: 2,
                    }}
                    onMouseEnter={e => { (e.currentTarget as HTMLElement).style.color = '#adf'; }}
                    onMouseLeave={e => { (e.currentTarget as HTMLElement).style.color = '#8af'; }}
                  >↗ {ref}</a>
                );
              }
              if (onLocalRef) {
                return (
                  <span key={ref} onClick={() => onLocalRef(ref)}
                    style={{ color: '#8af', fontSize: 12, fontFamily: 'monospace', cursor: 'pointer', transition: 'color 0.15s' }}
                    onMouseEnter={e => { (e.currentTarget as HTMLElement).style.color = '#adf'; }}
                    onMouseLeave={e => { (e.currentTarget as HTMLElement).style.color = '#8af'; }}
                  >→ {ref}</span>
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

function FactEditor({ fact, repo, onSaved }: { fact: Fact; repo: string; onSaved: (updated: Fact) => void }) {
  const [raw, setRaw] = useState(fact.body);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);

  const save = () => {
    setSaving(true);
    setSaveError(null);
    api.updateFact(repo, fact.path, raw)
      .then(updated => { setSaving(false); onSaved(updated); })
      .catch(e => { setSaving(false); setSaveError(String(e)); });
  };

  return (
    <div style={{ padding: '24px 28px', overflowY: 'auto', height: '100%', boxSizing: 'border-box', display: 'flex', flexDirection: 'column', gap: 16 }}>
      {/* Error banner */}
      <div style={{ background: '#2e1a1a', border: '1px solid rgba(255,80,80,0.3)', borderRadius: 6, padding: '10px 14px' }}>
        <div style={{ color: '#f88', fontSize: 11, textTransform: 'uppercase', letterSpacing: 1.2, marginBottom: 4 }}>Parse error</div>
        <div style={{ color: '#f44', fontSize: 12, fontFamily: 'monospace' }}>{fact.parse_error}</div>
      </div>

      {/* Path */}
      <div style={{ fontSize: 12, color: '#555' }}>{fact.path}</div>

      {/* Raw editor */}
      <textarea
        value={raw}
        onChange={e => setRaw(e.target.value)}
        spellCheck={false}
        style={{
          flex: 1, minHeight: 320, background: '#0d0d14', color: '#ccc', border: '1px solid #2a2a3a',
          borderRadius: 6, padding: '12px 14px', fontFamily: 'monospace', fontSize: 12,
          lineHeight: 1.6, resize: 'none', outline: 'none', boxSizing: 'border-box', width: '100%',
        }}
      />

      {/* Save button + feedback */}
      <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
        <button
          onClick={save}
          disabled={saving}
          style={{
            background: '#1a2e1a', border: '1px solid rgba(119,204,153,0.35)', color: '#7c9',
            padding: '6px 16px', borderRadius: 4, cursor: saving ? 'default' : 'pointer',
            fontSize: 13, opacity: saving ? 0.6 : 1,
          }}
        >{saving ? 'Saving…' : 'Save'}</button>
        {saveError && <span style={{ color: '#f88', fontSize: 12 }}>{saveError}</span>}
      </div>
    </div>
  );
}


export function RightPanel({ state, dispatch }: Props) {
  const [fact, setFact] = useState<Fact | null>(null);
  const [history, setHistory] = useState<HistoryEntry[]>([]);
  const [stats, setStats] = useState<Stats | null>(null);
  const [activity, setActivity] = useState<ActivityStats | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [commitDetail, setCommitDetail] = useState<CommitDetail | null>(null);
  const [commitSelectedFile, setCommitSelectedFile] = useState<string | null>(null);
  const [focusIdx, setFocusIdx] = useState(0);
  const [switcherOpen, setSwitcherOpen] = useState(false);
  const [dropdownFocusIdx, setDropdownFocusIdx] = useState(-1);

  const hasSwitcher = !!(state.historyCommit && commitDetail && commitDetail.files.length > 1);

  useEffect(() => {
    setError(null);
    if (state.historyCommit) {
      if (state.historyFocusPath) {
        // Local-ref navigation in history mode: load specific path at current commit
        setCommitDetail(null);
        setFocusIdx(0);
        setSwitcherOpen(false);
        setDropdownFocusIdx(-1);
        setCommitSelectedFile(state.historyFocusPath);
        api.fact(state.repo, state.historyFocusPath, state.historyCommit).then(setFact).catch(e => setError(String(e)));
      } else {
        // Time-travel: fetch commit detail and auto-load single file
        api.commitDetail(state.repo, state.historyCommit).then(detail => {
          setFocusIdx(0);
          setSwitcherOpen(false);
          setDropdownFocusIdx(-1);
          setCommitDetail(detail);
          // Auto-select first file (prefer non-deleted; fall back to first deleted)
          const firstFile = detail.files.find(f => f.action !== 'deleted') ?? detail.files[0];
          if (firstFile) {
            setCommitSelectedFile(firstFile.path);
            api.fact(state.repo, firstFile.path, state.historyCommit!).then(setFact).catch(e => setError(String(e)));
          }
        }).catch(() => setCommitDetail(null));
      }
    } else if (state.rightMode === 'fact' && state.selectedFact) {
      api.fact(state.repo, state.selectedFact, state.refCommit || undefined).then(setFact).catch(e => setError(String(e)));
    } else if (state.rightMode === 'history' && state.selectedFact) {
      api.history(state.repo, state.selectedFact).then(r => setHistory(r.entries || [])).catch(e => setError(String(e)));
    } else if (state.rightMode === 'summary') {
      const p = state.previewPath ?? state.currentPath;
      api.stats(state.repo, p).then(setStats).catch(() => setStats(null));
      api.activity(state.repo, p).then(setActivity).catch(() => setActivity(null));
    }
  }, [state.rightMode, state.selectedFact, state.currentPath, state.previewPath, state.headCommit, state.historyCommit, state.historyFocusPath]);

  // hasSimilar is true only in regular fact view (not in time-travel multi-file path)
  const hasSimilar = !!fact && !hasSwitcher;
  const focusTargets = buildFocusTargets(fact, hasSwitcher, hasSimilar);

  useEffect(() => {
    setFocusIdx(0);
    setSwitcherOpen(false);
    setDropdownFocusIdx(-1);
  }, [fact, commitDetail, state.rightMode]);

  useEffect(() => {
    if (!state.rightPanelFocused) return;
    const handler = (e: KeyboardEvent) => {
      if (e.key === 'ArrowLeft') {
        e.preventDefault();
        dispatch({ type: 'BLUR_RIGHT_PANEL' });
        return;
      }

      const moveInDropdown = (delta: 1 | -1) => {
        const total = commitDetail!.files.length;
        setDropdownFocusIdx(i => (i + delta + total) % total);
      };

      const cycleFact = (delta: 1 | -1) => {
        const all = commitDetail!.files;
        const cur = all.findIndex(f => f.path === commitSelectedFile);
        const next = (cur + delta + all.length) % all.length;
        const nextFile = all[next];
        setCommitSelectedFile(nextFile.path);
        api.fact(state.repo, nextFile.path, state.historyCommit!).then(setFact).catch(() => setFact(null));
        setFocusIdx(0);
      };

      if (e.key === 'ArrowDown' || e.key === 'j') {
        e.preventDefault();
        if (hasSwitcher && switcherOpen) { moveInDropdown(1); return; }
        if (hasSwitcher && focusTargets[focusIdx]?.kind === 'switcher') { cycleFact(1); return; }
        setFocusIdx(i => focusTargets.length > 0 ? (i + 1) % focusTargets.length : 0);
        return;
      }

      if (e.key === 'ArrowUp' || e.key === 'k') {
        e.preventDefault();
        if (hasSwitcher && switcherOpen) { moveInDropdown(-1); return; }
        if (hasSwitcher && focusTargets[focusIdx]?.kind === 'switcher') { cycleFact(-1); return; }
        setFocusIdx(i => focusTargets.length > 0 ? (i - 1 + focusTargets.length) % focusTargets.length : 0);
        return;
      }

      if (e.key === 'Enter') {
        e.preventDefault();
        const target = focusTargets[focusIdx];
        if (!target) return;
        if (target.kind === 'switcher') {
          if (switcherOpen && dropdownFocusIdx >= 0) {
            const f = commitDetail!.files[dropdownFocusIdx];
            if (f) {
              setCommitSelectedFile(f.path);
              api.fact(state.repo, f.path, state.historyCommit!).then(setFact).catch(() => setFact(null));
              setSwitcherOpen(false);
              setDropdownFocusIdx(-1);
              setFocusIdx(0);
            }
          } else {
            setSwitcherOpen(o => !o);
          }
          return;
        }
        if (target.kind === 'domain') { dispatch({ type: 'SEARCH', query: `domain:${target.value}` }); return; }
        if (target.kind === 'entity') { dispatch({ type: 'SEARCH', query: `entity:${target.value}` }); return; }
        if (target.kind === 'ref') { window.open(target.value, '_blank', 'noopener'); return; }
        if (target.kind === 'similar' && fact) { dispatch({ type: 'SIMILAR_SEARCH', path: fact.path, text: fact.body || '' }); return; }
      }
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [state.rightPanelFocused, focusIdx, focusTargets, switcherOpen, dropdownFocusIdx, hasSwitcher, commitDetail, commitSelectedFile, fact, state.repo, state.historyCommit, dispatch]);

  const focusInfo: FocusInfo = state.rightPanelFocused ? { target: focusTargets[focusIdx] ?? null } : { target: null };

  const search = (q: string) => dispatch({ type: 'SEARCH', query: q });

  const handleLocalRef = (path: string) => {
    if (state.leftMode === 'history') {
      dispatch({ type: 'HISTORY_OPEN_PATH', path });
    } else {
      dispatch({ type: 'OPEN_FACT', path, refCommit: fact?.commit_hash || undefined });
    }
  };

  if (error) return <div style={{ padding: 24, color: '#f44' }}>{error}</div>;

  // Time-travel: single file auto-loaded → show fact normally (no switcher)
  if (state.historyCommit && fact && !hasSwitcher) {
    if (fact.parse_error) return <FactEditor fact={fact} repo={state.repo} onSaved={setFact} />;
    const goToCommit = (commit: string) => {
      dispatch({ type: 'FACT_HISTORY', factPath: fact.path, commit });
    };
    return renderFact(fact, search, dispatch, focusInfo, commitDetail?.date, goToCommit, handleLocalRef);
  }

  // Time-travel: multiple files → show FactSwitcher + selected fact below
  if (state.historyCommit && commitDetail) {
    const added = commitDetail.files.filter(f => f.action === 'added').length;
    const modified = commitDetail.files.filter(f => f.action === 'modified').length;
    const deleted = commitDetail.files.filter(f => f.action === 'deleted').length;

    return (
      <div
        onClick={() => dispatch({ type: 'FOCUS_RIGHT_PANEL' })}
        style={{ display: 'flex', flexDirection: 'column', height: '100%', overflowY: 'auto', boxSizing: 'border-box' }}
      >
        {/* Commit header */}
        <div style={{ padding: '16px 20px 8px', borderBottom: '1px solid #222', display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap', flexShrink: 0 }}>
          <span style={{ color: '#7c9', fontFamily: 'monospace', fontSize: 12 }}>{commitDetail.commit.slice(0, 7)}</span>
          {(() => {
            const cs = commitDetailStyle(commitDetail);
            return cs ? <span style={{ color: cs.color, background: cs.bg, padding: '1px 6px', borderRadius: 3, fontSize: 10, fontFamily: 'monospace' }}>{cs.label}</span> : null;
          })()}
          {/* timestamp + A/M/D summary */}
          <span style={{ marginLeft: 'auto', display: 'flex', alignItems: 'center', gap: 6, flexShrink: 0 }}>
            <span title={new Date(commitDetail.date).toLocaleString()} style={{ color: '#666', fontSize: 11 }}>{relativeTime(commitDetail.date)}</span>
            {added > 0 && <span style={{ fontSize: 10, color: '#7c9', background: '#1a2e1a', padding: '1px 6px', borderRadius: 3, fontFamily: 'monospace' }}>{added} A</span>}
            {modified > 0 && <span style={{ fontSize: 10, color: '#8af', background: '#1a1a2e', padding: '1px 6px', borderRadius: 3, fontFamily: 'monospace' }}>{modified} M</span>}
            {deleted > 0 && <span style={{ fontSize: 10, color: '#f88', background: '#2e1a1a', padding: '1px 6px', borderRadius: 3, fontFamily: 'monospace' }}>{deleted} D</span>}
          </span>
        </div>
        <div style={{ color: '#888', fontSize: 12, padding: '6px 20px 0', flexShrink: 0 }}>{commitDetail.message}</div>

        <FactSwitcher
          files={commitDetail.files}
          selectedPath={commitSelectedFile}
          onSelect={(path, _action) => {
            setCommitSelectedFile(path);
            api.fact(state.repo, path, state.historyCommit!).then(setFact).catch(() => setFact(null));
            setFocusIdx(0);
          }}
          focusIdx={state.rightPanelFocused ? focusIdx : -1}
          dropdownFocusIdx={dropdownFocusIdx}
          open={switcherOpen}
          onToggle={() => setSwitcherOpen(o => !o)}
        />

        {fact && fact.parse_error && <FactEditor fact={fact} repo={state.repo} onSaved={setFact} />}
        {fact && !fact.parse_error && <div style={{ flex: 1 }}>{renderFact(fact, search, dispatch, focusInfo, commitDetail.date, undefined, handleLocalRef)}</div>}
        {!fact && <div style={{ padding: '16px 20px', color: '#666', fontSize: 13 }}>Loading…</div>}
      </div>
    );
  }

  if (state.rightMode === 'summary') {
    const domainEntries = stats ? Object.entries(stats.domains).sort((a, b) => b[1] - a[1]).slice(0, 10) : [];
    const entityEntries = stats ? Object.entries(stats.entities).sort((a, b) => b[1] - a[1]).slice(0, 10) : [];
    const domainCount = stats ? Object.keys(stats.domains).length : 0;
    const entityCount = stats ? Object.keys(stats.entities).length : 0;
    const totalCommits = activity ? String(activity.total) : '—';

    return (
      <div style={{ padding: '24px 28px', overflowY: 'auto', height: '100%', boxSizing: 'border-box' }}>
        {stats ? (
          <>
            {/* Summary line with last change time */}
            <div style={{ fontSize: 12, color: '#555', marginBottom: 20, display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
              <span>{stats.total} facts across {domainCount} domains</span>
              {activity?.last_commit && (
                <span title={new Date(activity.last_commit).toLocaleString()} style={{ color: '#555', fontSize: 11 }}>
                  {relativeTime(activity.last_commit)}
                </span>
              )}
            </div>

            {/* Stat cards */}
            <div style={{ display: 'flex', gap: 10, marginBottom: 28, flexWrap: 'wrap' }}>
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
              <div style={{ borderLeft: '3px solid #8af', padding: '10px 16px', background: '#1a1a2a', borderRadius: '0 6px 6px 0', minWidth: 90 }}>
                <div style={{ fontSize: 10, color: '#666', textTransform: 'uppercase', letterSpacing: 1 }}>Entities</div>
                <div style={{ fontSize: 22, fontWeight: 600, color: '#eee', marginTop: 2 }}>{entityCount}</div>
              </div>
              <div style={{ borderLeft: '3px solid #555', padding: '10px 16px', background: '#1a1a2a', borderRadius: '0 6px 6px 0', minWidth: 90 }}>
                <div style={{ fontSize: 10, color: '#666', textTransform: 'uppercase', letterSpacing: 1 }}>Commits</div>
                <div style={{ fontSize: 22, fontWeight: 600, color: '#eee', marginTop: 2 }}>{totalCommits}</div>
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

  if (fact.parse_error) return <FactEditor fact={fact} repo={state.repo} onSaved={setFact} />;
  return renderFact(fact, search, dispatch, focusInfo, undefined, undefined, handleLocalRef);
}
