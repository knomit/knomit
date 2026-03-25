import { useState, useRef, useEffect } from 'react';
import type { Dispatch } from 'react';
import { createPortal } from 'react-dom';
import type { AppState, Action } from './state';
import type { RepoInfo } from './api';
import { api } from './api';
import { BookIcon, GitBranchIcon, WrenchIcon, GlobeIcon, MoreVerticalIcon } from './icons';

interface Props {
  state: AppState;
  repos: RepoInfo[];
  dispatch: Dispatch<Action>;
  onSettingsClick: () => void;
}


export function TopBar({ state, repos, dispatch, onSettingsClick }: Props) {
  const [menuOpen, setMenuOpen] = useState(false);
  const btnRef = useRef<HTMLButtonElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const [pos, setPos] = useState({ top: 0, right: 0 });

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

  const toggleMenu = () => {
    if (!menuOpen && btnRef.current) {
      const rect = btnRef.current.getBoundingClientRect();
      setPos({ top: rect.bottom + 4, right: window.innerWidth - rect.right });
    }
    setMenuOpen(o => !o);
  };

  const rebuilding = state.tasks.rebuild?.status === 'running';

  return (
    <div style={{ height: 40, background: '#111', borderBottom: '1px solid #333', display: 'flex', alignItems: 'center', padding: '0 16px', gap: 12, flexShrink: 0 }}>
      {/* Logo */}
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
          <select
            data-testid="toknomitr-repo-select"
            value={state.repo}
            onChange={e => dispatch({ type: 'SET_REPO', repo: e.target.value })}
            style={{
              background: '#1a1a2a', color: '#7c9', border: '1px solid #333',
              borderRadius: 3, fontSize: 12, padding: '1px 4px', cursor: 'pointer',
            }}
          >
            {repos.map(r => (
              <option key={r.name} value={r.name}>{r.name}</option>
            ))}
          </select>
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
        <span data-testid="toknomitr-commit" style={{ display: 'inline-flex', alignItems: 'center', fontSize: 11, fontFamily: 'monospace', color: '#666', background: '#1a1a2a', padding: '0 6px', borderRadius: 3, border: '1px solid #333', lineHeight: '18px' }}>
          {state.headCommit.slice(0, 7)}
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
            data-testid="menu-origin"
            onClick={() => { setMenuOpen(false); onSettingsClick(); }}
            style={{
              display: 'flex', alignItems: 'center', gap: 8,
              padding: '6px 12px', cursor: 'pointer', color: '#aaa', fontSize: 12,
            }}
            onMouseEnter={e => { e.currentTarget.style.background = '#2a2a3a'; e.currentTarget.style.color = '#eee'; }}
            onMouseLeave={e => { e.currentTarget.style.background = 'transparent'; e.currentTarget.style.color = '#aaa'; }}
          >
            <GlobeIcon color="currentColor" size={13} /> Origin
          </div>
          <div
            data-testid="menu-rebuild"
            onClick={() => {
              if (!rebuilding) {
                api.rebuild(state.repo);
                setMenuOpen(false);
              }
            }}
            style={{
              display: 'flex', alignItems: 'center', gap: 8,
              padding: '6px 12px', cursor: rebuilding ? 'default' : 'pointer',
              color: rebuilding ? '#555' : '#aaa', fontSize: 12,
            }}
            onMouseEnter={e => { if (!rebuilding) { e.currentTarget.style.background = '#2a2a3a'; e.currentTarget.style.color = '#eee'; } }}
            onMouseLeave={e => { e.currentTarget.style.background = 'transparent'; e.currentTarget.style.color = rebuilding ? '#555' : '#aaa'; }}
          >
            <WrenchIcon color="currentColor" size={13} /> Rebuild
          </div>
        </div>,
        document.body
      )}
    </div>
  );
}
