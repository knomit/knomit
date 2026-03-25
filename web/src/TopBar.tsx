import { useState, useRef, useEffect } from 'react';
import type { Dispatch } from 'react';
import { createPortal } from 'react-dom';
import type { AppState, Action } from './state';
import type { RepoInfo } from './api';
import { api } from './api';

interface Props {
  state: AppState;
  repos: RepoInfo[];
  dispatch: Dispatch<Action>;
  onSettingsClick: () => void;
}

const BranchIcon = () => (
  <svg width="13" height="13" viewBox="0 0 16 16" fill="currentColor">
    <path d="M9.5 3.25a2.25 2.25 0 1 1 3 2.122V6A2.5 2.5 0 0 1 10 8.5H6a1 1 0 0 0-1 1v1.128a2.251 2.251 0 1 1-1.5 0V5.372a2.25 2.25 0 1 1 1.5 0v1.836A2.492 2.492 0 0 1 6 7h4a1 1 0 0 0 1-1v-.628A2.25 2.25 0 0 1 9.5 3.25Zm-6 0a.75.75 0 1 0 1.5 0 .75.75 0 0 0-1.5 0Zm8.25-.75a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5ZM4.25 12a.75.75 0 1 0 0 1.5.75.75 0 0 0 0-1.5Z"/>
  </svg>
);

const RepoIcon = () => (
  <svg width="13" height="13" viewBox="0 0 16 16" fill="currentColor">
    <path d="M2 2.5A2.5 2.5 0 0 1 4.5 0h8.75a.75.75 0 0 1 .75.75v12.5a.75.75 0 0 1-.75.75h-2.5a.75.75 0 0 1 0-1.5h1.75v-2h-8a1 1 0 0 0-.714 1.7.75.75 0 1 1-1.072 1.05A2.495 2.495 0 0 1 2 11.5Zm10.5-1h-8a1 1 0 0 0-1 1v6.708A2.486 2.486 0 0 1 4.5 9h8V1.5Z"/>
  </svg>
);

const HammerIcon = () => (
  <svg width="13" height="13" viewBox="0 0 16 16" fill="currentColor">
    <path d="M9.972 2.508a.5.5 0 0 0-.16-.556l-.178-.129a5.009 5.009 0 0 0-2.076-.783C6.215.862 4.504 1.229 2.84 3.133H1.786a.5.5 0 0 0-.354.147L.146 4.567a.5.5 0 0 0 0 .706l2.571 2.579a.5.5 0 0 0 .708 0l1.286-1.29a.5.5 0 0 0 .146-.353V5.57l8.387 8.873A.5.5 0 0 0 13.6 14.5l1.9-1.8a.5.5 0 0 0 .024-.73L9.972 2.508z"/>
  </svg>
);

const GlobeIcon = () => (
  <svg width="13" height="13" viewBox="0 0 16 16" fill="currentColor">
    <path d="M0 8a8 8 0 1 1 16 0A8 8 0 0 1 0 8zm7.5-6.923c-.67.204-1.335.82-1.887 1.855A7.97 7.97 0 0 0 5.145 4H7.5V1.077zM4.09 4a9.267 9.267 0 0 1 .64-1.539 6.7 6.7 0 0 1 .597-.933A7.025 7.025 0 0 0 2.255 4H4.09zm-.582 3.5c.03-.877.138-1.718.312-2.5H1.674a6.958 6.958 0 0 0-.656 2.5h2.49zM4.847 5a12.5 12.5 0 0 0-.338 2.5H7.5V5H4.847zM8.5 5v2.5h2.99a12.495 12.495 0 0 0-.337-2.5H8.5zM4.51 8.5a12.5 12.5 0 0 0 .337 2.5H7.5V8.5H4.51zm3.99 0V11h2.653c.187-.765.306-1.608.338-2.5H8.5zM5.145 12c.138.386.295.744.468 1.068.552 1.035 1.218 1.65 1.887 1.855V12H5.145zm.182 2.472a6.696 6.696 0 0 1-.597-.933A9.268 9.268 0 0 1 4.09 12H2.255a7.024 7.024 0 0 0 3.072 2.472zM3.82 11a13.652 13.652 0 0 1-.312-2.5h-2.49c.062.89.291 1.733.656 2.5H3.82zm6.853 3.472A7.024 7.024 0 0 0 13.745 12H11.91a9.27 9.27 0 0 1-.64 1.539 6.688 6.688 0 0 1-.597.933zM8.5 12v2.923c.67-.204 1.335-.82 1.887-1.855.173-.324.33-.682.468-1.068H8.5zm3.68-1h2.146c.365-.767.594-1.61.656-2.5h-2.49a13.65 13.65 0 0 1-.312 2.5zm2.802-3.5a6.959 6.959 0 0 0-.656-2.5H12.18c.174.782.282 1.623.312 2.5h2.49zM11.27 2.461c.247.464.462.98.64 1.539h1.835a7.024 7.024 0 0 0-3.072-2.472c.218.284.418.598.597.933zM10.855 4a7.966 7.966 0 0 0-.468-1.068C9.835 1.897 9.17 1.282 8.5 1.077V4h2.355z"/>
  </svg>
);

const MenuIcon = () => (
  <svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor">
    <path d="M9.5 13a1.5 1.5 0 1 1-3 0 1.5 1.5 0 0 1 3 0zm0-5a1.5 1.5 0 1 1-3 0 1.5 1.5 0 0 1 3 0zm0-5a1.5 1.5 0 1 1-3 0 1.5 1.5 0 0 1 3 0z"/>
  </svg>
);


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
        <RepoIcon />
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
          <BranchIcon />
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
        <MenuIcon />
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
            <GlobeIcon /> Origin
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
            <HammerIcon /> Rebuild
          </div>
        </div>,
        document.body
      )}
    </div>
  );
}
