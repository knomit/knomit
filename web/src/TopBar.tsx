import type { Dispatch } from 'react';
import type { AppState, Action } from './state';
import type { RepoInfo } from './api';

interface Props {
  state: AppState;
  repos: RepoInfo[];
  dispatch: Dispatch<Action>;
  onSettingsClick: () => void;
}

const GearIcon = () => (
  <svg width="13" height="13" viewBox="0 0 16 16" fill="currentColor">
    <path d="M9.405 1.05c-.413-1.4-2.397-1.4-2.81 0l-.1.34a1.464 1.464 0 0 1-2.105.872l-.31-.17c-1.283-.698-2.686.705-1.987 1.987l.169.311c.446.82.023 1.841-.872 2.105l-.34.1c-1.4.413-1.4 2.397 0 2.81l.34.1a1.464 1.464 0 0 1 .872 2.105l-.17.31c-.698 1.283.705 2.686 1.987 1.987l.311-.169a1.464 1.464 0 0 1 2.105.872l.1.34c.413 1.4 2.397 1.4 2.81 0l.1-.34a1.464 1.464 0 0 1 2.105-.872l.31.17c1.283.698 2.686-.705 1.987-1.987l-.169-.311a1.464 1.464 0 0 1 .872-2.105l.34-.1c1.4-.413 1.4-2.397 0-2.81l-.34-.1a1.464 1.464 0 0 1-.872-2.105l.17-.31c.698-1.283-.705-2.686-1.987-1.987l-.311.169a1.464 1.464 0 0 1-2.105-.872l-.1-.34zM8 10.93a2.929 2.929 0 1 1 0-5.86 2.929 2.929 0 0 1 0 5.858z"/>
  </svg>
);

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

export function TopBar({ state, repos, dispatch, onSettingsClick }: Props) {
  return (
    <div style={{ height: 40, background: '#111', borderBottom: '1px solid #333', display: 'flex', alignItems: 'center', padding: '0 16px', gap: 12, flexShrink: 0 }}>
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
          <span>{state.repo}</span>
        )}
      </span>
      {state.branch && (
        <span style={{ display: 'flex', alignItems: 'center', gap: 5, color: '#8af', fontSize: 12 }}>
          <BranchIcon />
          <span>{state.branch}</span>
        </span>
      )}
      {state.headCommit && (
        <span style={{ display: 'inline-flex', alignItems: 'center', fontSize: 11, fontFamily: 'monospace', color: '#666', background: '#1a1a2a', padding: '0 6px', borderRadius: 3, border: '1px solid #333', lineHeight: '18px' }}>
          {state.headCommit.slice(0, 7)}
        </span>
      )}
      <button
        onClick={onSettingsClick}
        title={state.remoteError ? `Remote error: ${state.remoteError}` : 'Origin settings'}
        style={{ background: 'none', border: 'none', color: state.remoteError ? '#f44336' : '#666', cursor: 'pointer', padding: 4, display: 'flex', alignItems: 'center', position: 'relative' }}
        onMouseEnter={e => { if (!state.remoteError) e.currentTarget.style.color = '#aaa'; }}
        onMouseLeave={e => { if (!state.remoteError) e.currentTarget.style.color = '#666'; }}
      >
        <GearIcon />
        {state.remoteError && (
          <span style={{ position: 'absolute', top: 2, right: 2, width: 6, height: 6, borderRadius: '50%', background: '#f44336' }} />
        )}
      </button>
    </div>
  );
}
