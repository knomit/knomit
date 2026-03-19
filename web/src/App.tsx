import { useReducer, useEffect, useState, useRef } from 'react';
import { createPortal } from 'react-dom';
import { reducer, init } from './state';
import type { Action } from './state';
import { api } from './api';
import type { RepoInfo, DirChild } from './api';
import { TopBar } from './TopBar';
import { LeftPanel } from './LeftPanel';
import { RightPanel } from './RightPanel';
import { Console } from './Console';
import { OriginModal } from './OriginModal';
import './App.css';

function BreadcrumbPicker({ repo, currentPath, dispatch }: { repo: string; currentPath: string; dispatch: React.Dispatch<Action> }) {
  const [open, setOpen] = useState(false);
  const [children, setChildren] = useState<DirChild[]>([]);
  const [filter, setFilter] = useState('');
  const [highlightIdx, setHighlightIdx] = useState(0);
  const triggerRef = useRef<HTMLSpanElement>(null);
  const dropdownRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const [pos, setPos] = useState({ top: 0, left: 0 });

  useEffect(() => {
    if (!open) return;
    api.browse(repo, currentPath).then(r => {
      setChildren(r.children || []);
    }).catch(() => setChildren([]));
  }, [open, repo, currentPath]);

  // Focus input when dropdown opens
  useEffect(() => {
    if (open) setTimeout(() => inputRef.current?.focus(), 0);
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const handler = (e: MouseEvent) => {
      if (triggerRef.current?.contains(e.target as Node)) return;
      if (dropdownRef.current?.contains(e.target as Node)) return;
      setOpen(false);
    };
    document.addEventListener('mousedown', handler);
    return () => document.removeEventListener('mousedown', handler);
  }, [open]);

  const filtered = children.filter(c => {
    const name = c.name;
    return name.toLowerCase().includes(filter.toLowerCase());
  });

  // Sort: dirs first, then alphabetical
  filtered.sort((a, b) => {
    if (a.is_dir !== b.is_dir) return a.is_dir ? -1 : 1;
    return a.name.localeCompare(b.name);
  });

  const pick = (c: DirChild) => {
    const fullPath = currentPath ? `${currentPath}/${c.name}` : c.name;
    if (c.is_dir) {
      dispatch({ type: 'NAVIGATE', path: fullPath });
    } else {
      dispatch({ type: 'SELECT_FACT', path: fullPath });
    }
    close();
  };

  const close = () => { setOpen(false); setFilter(''); setHighlightIdx(0); };

  const handleOpen = () => {
    if (!open && triggerRef.current) {
      const rect = triggerRef.current.getBoundingClientRect();
      setPos({ top: rect.bottom + 4, left: rect.left });
    }
    if (open) close(); else setOpen(true);
  };

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'ArrowDown') { e.preventDefault(); setHighlightIdx(i => Math.min(i + 1, filtered.length - 1)); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); setHighlightIdx(i => Math.max(i - 1, 0)); }
    else if (e.key === 'Enter' && filtered.length > 0) { e.preventDefault(); pick(filtered[highlightIdx] || filtered[0]); }
    else if (e.key === 'Escape') { e.preventDefault(); close(); }
  };

  return (
    <>
      <span style={{ display: 'inline-flex', alignItems: 'center' }}>
        <span style={{ color: '#444', fontSize: 12, flexShrink: 0, margin: '0 2px' }}>/</span>
        <span
          ref={triggerRef}
          onClick={handleOpen}
          style={{
            color: '#555',
            fontSize: 12,
            cursor: 'pointer',
            padding: '1px 4px',
            borderRadius: 3,
            fontFamily: 'monospace',
          }}
          onMouseEnter={e => (e.currentTarget.style.color = '#eee')}
          onMouseLeave={e => (e.currentTarget.style.color = '#555')}
        >_</span>
      </span>
      {open && createPortal(
        <div ref={dropdownRef} style={{
          position: 'fixed',
          top: pos.top,
          left: pos.left,
          background: '#1a1a1a',
          border: '1px solid #333',
          borderRadius: 4,
          minWidth: 160,
          maxHeight: 240,
          display: 'flex',
          flexDirection: 'column',
          zIndex: 10000,
          boxShadow: '0 4px 12px rgba(0,0,0,0.5)',
        }}>
          <input
            ref={inputRef}
            value={filter}
            onChange={e => { setFilter(e.target.value); setHighlightIdx(0); }}
            onKeyDown={handleKeyDown}
            placeholder="type to filter…"
            style={{
              background: '#222',
              border: 'none',
              borderBottom: '1px solid #333',
              color: '#ccc',
              fontSize: 12,
              padding: '5px 8px',
              outline: 'none',
              fontFamily: 'monospace',
            }}
          />
          <div style={{ overflowY: 'auto', maxHeight: 200 }}>
            {filtered.length === 0 && (
              <div style={{ color: '#555', fontSize: 12, padding: '6px 10px' }}>no matches</div>
            )}
            {filtered.map((c, i) => (
              <div
                key={c.name}
                onClick={() => pick(c)}
                style={{
                  color: i === highlightIdx ? '#eee' : '#aaa',
                  background: i === highlightIdx ? '#2a2a3a' : 'transparent',
                  fontSize: 12,
                  padding: '4px 10px',
                  cursor: 'pointer',
                  whiteSpace: 'nowrap',
                  display: 'flex',
                  alignItems: 'center',
                  gap: 4,
                }}
                onMouseEnter={() => setHighlightIdx(i)}
              >
                <span style={{ color: c.is_dir ? '#6a9fb5' : '#888', fontSize: 10, width: 12, textAlign: 'center', flexShrink: 0 }}>
                  {c.is_dir ? '▸' : '·'}
                </span>
                {c.is_dir ? c.name + '/' : c.name}
              </div>
            ))}
          </div>
        </div>,
        document.body
      )}
    </>
  );
}

export default function App() {
  const [state, dispatch] = useReducer(reducer, init);
  const [repos, setRepos] = useState<RepoInfo[]>([]);
  const [showOrigin, setShowOrigin] = useState(false);

  // Fetch repos list on mount.
  useEffect(() => {
    api.repos().then(setRepos).catch(() => {});
  }, []);

  // Load status when repo changes (also fires on mount).
  useEffect(() => {
    api.status(state.repo).then(s =>
      dispatch({ type: 'SET_STATUS', head: s.head, branch: s.branch, embeddingsEnabled: s.embeddings_enabled, ontologyRoot: s.ontology_root })
    ).catch(() => {});
  }, [state.repo]);

  // SSE for task and status events — reconnects when repo changes.
  useEffect(() => {
    const es = new EventSource(`/api/v1/${state.repo}/events`);
    es.addEventListener('task', (e) => {
      const ev = JSON.parse(e.data);
      dispatch({ type: 'SET_TASK', op: ev.op, status: ev.status, message: ev.message || '' });
      const level = ev.status === 'error' ? 'error' as const : 'info' as const;
      const repo = ev.repo ? `${ev.repo}/` : '';
      dispatch({ type: 'CONSOLE_LOG', level, message: `[${repo}${ev.op}] ${ev.message || ev.status}` });
      // Refresh head when a task completes.
      if (ev.status === 'done' || ev.status === 'error') {
        api.status(state.repo).then(s => dispatch({ type: 'SET_HEAD', head: s.head })).catch(() => {});
      }
    });
    es.addEventListener('status', (e) => {
      const s = JSON.parse(e.data);
      if (s.head) dispatch({ type: 'SET_HEAD', head: s.head });
    });
    const handleRemoteEvent = (e: MessageEvent) => {
      const ev = JSON.parse(e.data);
      if (ev.error) {
        dispatch({ type: 'SET_REMOTE_ERROR', error: ev.error });
        dispatch({ type: 'CONSOLE_LOG', level: 'error', message: `[remote] ${ev.error}` });
      } else {
        dispatch({ type: 'SET_REMOTE_ERROR', error: '' });
      }
    };
    es.addEventListener('sync_ok', handleRemoteEvent);
    es.addEventListener('sync_error', handleRemoteEvent);
    es.addEventListener('push_ok', handleRemoteEvent);
    es.addEventListener('push_error', handleRemoteEvent);
    return () => es.close();
  }, [state.repo]);

  // Build breadcrumb segments from currentPath
  // When currentPath points to a fact (.md file), treat the parent as the dir breadcrumb
  const isFactPath = state.currentPath.endsWith('.md');
  const dirPath = isFactPath ? state.currentPath.split('/').slice(0, -1).join('/') : state.currentPath;
  const pathParts = dirPath.split('/').filter(Boolean);
  const breadcrumbs = pathParts.map((seg, i) => ({
    label: seg,
    path: pathParts.slice(0, i + 1).join('/'),
  }));
  const isSearchMode = !!(state.searchQuery || state.similarTo) || state.leftMode === 'recent';
  const showFact = (state.selectedFact && !isSearchMode) || isFactPath;
  const factLabel = state.selectedFact
    ? state.selectedFact.split('/').pop()
    : state.currentPath.split('/').pop();

  return (
    <div style={{ display: 'flex', flexDirection: 'column', height: '100vh', width: '100vw', background: '#141414', color: '#eee', fontFamily: 'system-ui, sans-serif', overflow: 'hidden' }}>
      <TopBar state={state} repos={repos} dispatch={dispatch} onSettingsClick={() => setShowOrigin(true)} />

      {/* Breadcrumb path bar + action buttons */}
      <div style={{ height: 30, background: '#111', borderBottom: '1px solid #222', display: 'flex', alignItems: 'center', padding: '0 8px', gap: 2, flexShrink: 0, overflow: 'hidden' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 2, flex: 1, overflow: 'hidden', minWidth: 0 }}>
          <span style={{ color: '#444', fontSize: 12, flexShrink: 0, marginRight: 2 }}>⟩</span>
          {breadcrumbs.map((crumb, i) => (
            <span key={crumb.path} style={{ display: 'flex', alignItems: 'center', gap: 2, minWidth: 0, overflow: 'hidden' }}>
              {i > 0 && <span style={{ color: '#444', fontSize: 12, flexShrink: 0 }}>/</span>}
              <span
                onClick={() => dispatch({ type: 'NAVIGATE', path: crumb.path })}
                style={{
                  color: i === breadcrumbs.length - 1 && !showFact ? '#ccc' : '#666',
                  fontSize: 12,
                  cursor: 'pointer',
                  whiteSpace: 'nowrap',
                  overflow: 'hidden',
                  textOverflow: 'ellipsis',
                  padding: '1px 4px',
                  borderRadius: 3,
                }}
                onMouseEnter={e => (e.currentTarget.style.color = '#eee')}
                onMouseLeave={e => (e.currentTarget.style.color = i === breadcrumbs.length - 1 && !showFact ? '#ccc' : '#666')}
              >
                {crumb.label}
              </span>
            </span>
          ))}
          {showFact ? (
            <span style={{ display: 'flex', alignItems: 'center', gap: 2, minWidth: 0, overflow: 'hidden' }}>
              <span style={{ color: '#444', fontSize: 12, flexShrink: 0 }}>/</span>
              <span style={{
                color: '#ccc',
                fontSize: 12,
                whiteSpace: 'nowrap',
                overflow: 'hidden',
                textOverflow: 'ellipsis',
                padding: '1px 4px',
                borderRadius: 3,
              }}>
                {factLabel}
              </span>
            </span>
          ) : (
            <BreadcrumbPicker repo={state.repo} currentPath={state.currentPath} dispatch={dispatch} />
          )}
        </div>
        <span
          title={state.leftMode === 'browse' ? 'Switch to history (h)' : state.leftMode === 'history' ? 'Switch to recent (r)' : 'Switch to browsing (esc)'}
          onClick={() => {
            if (state.leftMode === 'browse') dispatch({ type: 'ENTER_HISTORY' });
            else if (state.leftMode === 'history') dispatch({ type: 'ENTER_RECENT' });
            else dispatch({ type: 'EXIT_HISTORY' });
          }}
          style={{
            display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
            width: 22, height: 20, borderRadius: 3, cursor: 'pointer', flexShrink: 0, marginLeft: 8,
            color: '#888',
          }}
          onMouseEnter={e => (e.currentTarget.style.color = '#ccc')}
          onMouseLeave={e => (e.currentTarget.style.color = '#888')}
        >
          {state.leftMode === 'browse' ? (
            <svg width="12" height="12" viewBox="0 0 16 16" fill="currentColor">
              <path d="M1 2.5A1.5 1.5 0 0 1 2.5 1h3.879a1.5 1.5 0 0 1 1.06.44l1.122 1.12A1.5 1.5 0 0 0 9.62 3H13.5A1.5 1.5 0 0 1 15 4.5v8a1.5 1.5 0 0 1-1.5 1.5h-11A1.5 1.5 0 0 1 1 12.5v-10z"/>
            </svg>
          ) : state.leftMode === 'history' ? (
            <svg width="12" height="12" viewBox="0 0 16 16" fill="currentColor">
              <path d="M8 3.5a.5.5 0 0 0-1 0V8a.5.5 0 0 0 .252.434l3.5 2a.5.5 0 0 0 .496-.868L8 7.71V3.5z"/>
              <path d="M8 16A8 8 0 1 0 8 0a8 8 0 0 0 0 16zm7-8A7 7 0 1 1 1 8a7 7 0 0 1 14 0z"/>
            </svg>
          ) : (
            <svg width="12" height="12" viewBox="0 0 16 16" fill="currentColor">
              <path d="M2.5 15a.5.5 0 1 1 0-1h1v-1a4.5 4.5 0 0 1 2.557-4.06c.29-.139.443-.377.443-.59v-.7c0-.213-.154-.451-.443-.59A4.5 4.5 0 0 1 3.5 3V2h-1a.5.5 0 0 1 0-1h11a.5.5 0 0 1 0 1h-1v1a4.5 4.5 0 0 1-2.557 4.06c-.29.139-.443.377-.443.59v.7c0 .213.154.451.443.59A4.5 4.5 0 0 1 12.5 13v1h1a.5.5 0 0 1 0 1h-11z"/>
            </svg>
          )}
        </span>
      </div>

      <div style={{ display: 'flex', flex: 1, overflow: 'hidden', minHeight: 0 }}>
        <div style={{ width: '35%', minWidth: 180, maxWidth: '50%', borderRight: '1px solid #222', overflow: 'hidden', display: 'flex', flexDirection: 'column' }}>
          <LeftPanel state={state} dispatch={dispatch} />
        </div>
        <div style={{ flex: 1, overflow: 'hidden', minWidth: 0 }}>
          <RightPanel state={state} dispatch={dispatch} />
        </div>
      </div>
      <Console state={state} dispatch={dispatch} />
      {showOrigin && <OriginModal repo={state.repo} onClose={() => setShowOrigin(false)} />}
    </div>
  );
}
