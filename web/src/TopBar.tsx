import { useState, useRef, useEffect } from 'react';
import type { Dispatch } from 'react';
import { createPortal } from 'react-dom';
import type { AppState, Action } from './state';
import { isReadOnly, READ_ONLY_TITLE } from './state';
import type { RepoInfo } from './api';
import { api } from './api';
import { BookIcon, GitBranchIcon, WrenchIcon, GlobeIcon, MoreVerticalIcon, ChevronDownIcon } from './icons';

interface Props {
  state: AppState;
  repos: RepoInfo[];
  dispatch: Dispatch<Action>;
  onSettingsClick: () => void;
  onManageRepos: () => void;
}


export function TopBar({ state, repos, dispatch, onSettingsClick, onManageRepos }: Props) {
  const [menuOpen, setMenuOpen] = useState(false);
  const btnRef = useRef<HTMLButtonElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const [pos, setPos] = useState({ top: 0, right: 0 });

  const [repoOpen, setRepoOpen] = useState(false);
  const repoBtnRef = useRef<HTMLButtonElement>(null);
  const repoMenuRef = useRef<HTMLDivElement>(null);
  const [repoPos, setRepoPos] = useState({ top: 0, left: 0, minWidth: 0 });

  useEffect(() => {
    if (!menuOpen) return;
    const handler = (e: MouseEvent) => {
      if (btnRef.current?.contains(e.target as Node)) return;
      if (menuRef.current?.contains(e.target as Node)) return;
      setMenuOpen(false);
    };
    document.addEventListener('mousedown', handler);
    return () => document.removeEventListener('mousedown', handler);
  }, [menuOpen]);

  useEffect(() => {
    if (!repoOpen) return;
    const handler = (e: MouseEvent) => {
      if (repoBtnRef.current?.contains(e.target as Node)) return;
      if (repoMenuRef.current?.contains(e.target as Node)) return;
      setRepoOpen(false);
    };
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') setRepoOpen(false); };
    document.addEventListener('mousedown', handler);
    window.addEventListener('keydown', onKey);
    return () => {
      document.removeEventListener('mousedown', handler);
      window.removeEventListener('keydown', onKey);
    };
  }, [repoOpen]);

  const toggleMenu = () => {
    if (!menuOpen && btnRef.current) {
      const rect = btnRef.current.getBoundingClientRect();
      setPos({ top: rect.bottom + 4, right: window.innerWidth - rect.right });
    }
    setMenuOpen(o => !o);
  };

  const toggleRepoMenu = () => {
    if (!repoOpen && repoBtnRef.current) {
      const rect = repoBtnRef.current.getBoundingClientRect();
      setRepoPos({ top: rect.bottom + 4, left: rect.left, minWidth: rect.width });
    }
    setRepoOpen(o => !o);
  };

  const pickRepo = (name: string) => {
    setRepoOpen(false);
    if (name !== state.repo) dispatch({ type: 'SET_REPO', repo: name });
  };

  const rebuilding = state.tasks.rebuild?.status === 'running';
  const readOnly = isReadOnly(state);

  return (
    <div style={{ height: 40, background: '#111', borderBottom: '1px solid #1c1c1c', display: 'flex', alignItems: 'center', padding: '0 14px', gap: 10, flexShrink: 0 }}>
      <span style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
        <svg width="20" height="20" viewBox="0 0 80 80">
          <rect x="12" y="12" width="56" height="56" rx="10" transform="rotate(45 40 40)" fill="#7c9"/>
          <line x1="30" y1="24" x2="30" y2="56" stroke="#111" strokeWidth="4" strokeLinecap="round"/>
          <path d="M30 40 Q38 36 50 24" stroke="#111" strokeWidth="4" fill="none" strokeLinecap="round"/>
          <path d="M30 40 Q38 44 50 56" stroke="#111" strokeWidth="4" fill="none" strokeLinecap="round"/>
          <circle cx="30" cy="24" r="3.5" fill="#111"/>
          <circle cx="30" cy="56" r="3.5" fill="#111"/>
          <circle cx="50" cy="24" r="3.5" fill="#111"/>
          <circle cx="50" cy="56" r="3.5" fill="#111"/>
          <circle cx="30" cy="40" r="3.5" fill="#111"/>
        </svg>
        <span style={{ color: '#7c9', fontWeight: 'bold', fontSize: 15 }}>knomit</span>
      </span>

      <div style={{ flex: 1 }} />
      <span style={{ display: 'flex', alignItems: 'center', gap: 5, color: '#7c9', fontSize: 12 }}>
        <BookIcon color="currentColor" size={13} />
        {repos.length > 1 ? (
          <button
            data-testid="toknomitr-repo-select"
            ref={repoBtnRef}
            onClick={toggleRepoMenu}
            aria-haspopup="listbox"
            aria-expanded={repoOpen}
            style={{
              background: '#1a1a2a', color: '#7c9', border: '1px solid #333',
              borderRadius: 3, fontSize: 12, padding: '1px 4px 1px 6px', cursor: 'pointer',
              display: 'inline-flex', alignItems: 'center', gap: 4, fontFamily: 'inherit',
              lineHeight: 1.5,
            }}
          >
            <span>{state.repo}</span>
            <ChevronDownIcon color="currentColor" size={11} />
          </button>
        ) : (
          <span data-testid="toknomitr-repo-name">{state.repo}</span>
        )}
      </span>
      {state.branch && (
        <span style={{ display: 'flex', alignItems: 'center', gap: 5, color: '#8af', fontSize: 12 }}>
          <GitBranchIcon color="currentColor" size={13} />
          <span data-testid="toknomitr-branch">{state.branch}</span>
        </span>
      )}
      {state.headCommit && (
        // line-height 1 collapses the monospace block to its glyph extent
        // so the digit caps align visually with the surrounding sans-serif
        // text (baseline alignment alone leaves the digits a few px high
        // because monospace glyphs occupy their full cap-height while the
        // adjacent text is mostly lowercase x-height).
        <span data-testid="toknomitr-commit" style={{ display: 'inline-flex', alignItems: 'center', gap: 4, fontFamily: 'monospace', fontSize: 11, lineHeight: 1 }}>
          <span style={{ color: '#3a3a3a' }}>@</span>
          <span style={{ color: '#6a9080' }}>{state.headCommit.slice(0, 7)}</span>
        </span>
      )}
      <button
        data-testid="toknomitr-menu-btn"
        ref={btnRef}
        onClick={toggleMenu}
        title="Actions"
        style={{ background: 'none', border: 'none', color: state.remoteError ? '#f44336' : '#666', cursor: 'pointer', padding: 4, display: 'flex', alignItems: 'center', position: 'relative' }}
        onMouseEnter={e => { if (!state.remoteError) e.currentTarget.style.color = '#aaa'; }}
        onMouseLeave={e => { if (!state.remoteError) e.currentTarget.style.color = state.remoteError ? '#f44336' : '#666'; }}
      >
        <MoreVerticalIcon color="currentColor" size={14} />
        {state.remoteError && (
          <span style={{ position: 'absolute', top: 2, right: 2, width: 6, height: 6, borderRadius: '50%', background: '#f44336' }} />
        )}
      </button>
      {repoOpen && createPortal(
        <div ref={repoMenuRef} role="listbox" data-testid="toknomitr-repo-menu" style={{
          position: 'fixed',
          top: repoPos.top,
          left: repoPos.left,
          minWidth: Math.max(repoPos.minWidth, 140),
          background: '#1a1a1a',
          border: '1px solid #333',
          borderRadius: 4,
          zIndex: 10000,
          boxShadow: '0 4px 12px rgba(0,0,0,0.5)',
          padding: '4px 0',
          maxHeight: 300,
          overflowY: 'auto',
        }}>
          {repos.map(r => {
            const active = r.name === state.repo;
            return (
              <div
                key={r.name}
                role="option"
                aria-selected={active}
                data-testid={`toknomitr-repo-option-${r.name}`}
                onClick={() => pickRepo(r.name)}
                style={{
                  display: 'flex', alignItems: 'center', gap: 8,
                  padding: '6px 12px',
                  cursor: 'pointer',
                  color: active ? '#7c9' : '#aaa', fontSize: 12,
                }}
                onMouseEnter={e => { e.currentTarget.style.background = '#2a2a3a'; if (!active) e.currentTarget.style.color = '#eee'; }}
                onMouseLeave={e => { e.currentTarget.style.background = 'transparent'; e.currentTarget.style.color = active ? '#7c9' : '#aaa'; }}
              >
                <span style={{ width: 10, color: '#7c9' }}>{active ? '✓' : ''}</span>
                <span>{r.name}</span>
              </div>
            );
          })}
        </div>,
        document.body
      )}
      {menuOpen && createPortal(
        <div ref={menuRef} style={{
          position: 'fixed',
          top: pos.top,
          right: pos.right,
          background: '#1a1a1a',
          border: '1px solid #333',
          borderRadius: 4,
          minWidth: 140,
          zIndex: 10000,
          boxShadow: '0 4px 12px rgba(0,0,0,0.5)',
          padding: '4px 0',
        }}>
          <div
            data-testid="menu-manage"
            onClick={() => { setMenuOpen(false); onManageRepos(); }}
            style={{
              display: 'flex', alignItems: 'center', gap: 8,
              padding: '6px 12px',
              cursor: 'pointer',
              color: '#aaa', fontSize: 12,
            }}
            onMouseEnter={e => { e.currentTarget.style.background = '#2a2a3a'; e.currentTarget.style.color = '#eee'; }}
            onMouseLeave={e => { e.currentTarget.style.background = 'transparent'; e.currentTarget.style.color = '#aaa'; }}
          >
            Manage repos…
          </div>
          <div
            data-testid="menu-origin"
            title={readOnly ? READ_ONLY_TITLE : undefined}
            aria-disabled={readOnly}
            onClick={() => {
              if (readOnly) return;
              setMenuOpen(false);
              onSettingsClick();
            }}
            style={{
              display: 'flex', alignItems: 'center', gap: 8,
              padding: '6px 12px',
              cursor: readOnly ? 'not-allowed' : 'pointer',
              color: readOnly ? '#555' : '#aaa', fontSize: 12,
              opacity: readOnly ? 0.5 : 1,
            }}
            onMouseEnter={e => { if (!readOnly) { e.currentTarget.style.background = '#2a2a3a'; e.currentTarget.style.color = '#eee'; } }}
            onMouseLeave={e => { e.currentTarget.style.background = 'transparent'; e.currentTarget.style.color = readOnly ? '#555' : '#aaa'; }}
          >
            <GlobeIcon color="currentColor" size={13} /> Origin
          </div>
          <div
            data-testid="menu-rebuild"
            title={readOnly ? READ_ONLY_TITLE : undefined}
            aria-disabled={readOnly || rebuilding}
            onClick={() => {
              if (rebuilding || readOnly) return;
              api.rebuild(state.repo, state.branch).catch(err => {
                dispatch({ type: 'CONSOLE_LOG', level: 'error', message: `[rebuild] failed: ${String(err)}` });
              });
              setMenuOpen(false);
            }}
            style={{
              display: 'flex', alignItems: 'center', gap: 8,
              padding: '6px 12px',
              cursor: (rebuilding || readOnly) ? 'not-allowed' : 'pointer',
              color: (rebuilding || readOnly) ? '#555' : '#aaa', fontSize: 12,
              opacity: readOnly ? 0.5 : 1,
            }}
            onMouseEnter={e => { if (!rebuilding && !readOnly) { e.currentTarget.style.background = '#2a2a3a'; e.currentTarget.style.color = '#eee'; } }}
            onMouseLeave={e => { e.currentTarget.style.background = 'transparent'; e.currentTarget.style.color = (rebuilding || readOnly) ? '#555' : '#aaa'; }}
          >
            <WrenchIcon color="currentColor" size={13} /> Rebuild
          </div>
        </div>,
        document.body
      )}
    </div>
  );
}
