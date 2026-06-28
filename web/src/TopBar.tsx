import { useState, useRef } from 'react';
import type { Dispatch, CSSProperties, MouseEvent as ReactMouseEvent } from 'react';
import { createPortal } from 'react-dom';
import type { AppState, Action } from './state';
import type { RepoInfo } from './api';
import { useDismiss } from './hooks';
import { BookIcon, GitBranchIcon, ChevronDownIcon, GearIcon } from './icons';

interface Props {
  state: AppState;
  repos: RepoInfo[];
  dispatch: Dispatch<Action>;
  onManageRepos: () => void;
  /** Live width of the Library panel; the title-bar identity zone matches it
   *  so the divider lines up with the splitter below. */
  leftWidth: number;
}

export function TopBar({ state, repos, dispatch, onManageRepos, leftWidth }: Props) {
  const [repoOpen, setRepoOpen] = useState(false);
  const repoBtnRef = useRef<HTMLButtonElement>(null);
  const repoMenuRef = useRef<HTMLDivElement>(null);
  const [repoPos, setRepoPos] = useState({ top: 0, left: 0, minWidth: 0 });

  useDismiss(repoOpen, () => setRepoOpen(false), [repoBtnRef, repoMenuRef]);

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

  // In the desktop app the native title bar is hidden, so the macOS traffic
  // lights float over the top-left of this header. Wails uses the CSS custom
  // property --wails-draggable (NOT -webkit-app-region, which WKWebView
  // ignores); it inherits, so interactive controls opt out with no-drag.
  const desktop = typeof window !== 'undefined' && (window as Window & { __KNOMIT_DESKTOP__?: boolean }).__KNOMIT_DESKTOP__;
  const noDrag = { '--wails-draggable': 'no-drag' } as CSSProperties;

  // Wails' CSS --wails-draggable mechanism gates the drag on the click target's
  // clientWidth/clientHeight, which are 0 for inline text and SVG — leaving the
  // logo and the repo/branch/commit labels as dead zones that can't grab the
  // window. So trigger the native window drag explicitly on mousedown anywhere
  // on the bar except interactive controls (tagged data-nodrag), making the
  // whole bar a reliable drag handle. This posts the same "wails:drag" message
  // over the same transport the Wails runtime uses; --wails-draggable below is
  // kept as a fallback.
  const startWindowDrag = (e: ReactMouseEvent) => {
    if (!desktop || e.button !== 0) return;
    if ((e.target as Element).closest('[data-nodrag]')) return;
    const w = window as unknown as {
      webkit?: { messageHandlers?: { external?: { postMessage(m: string): void } } };
      chrome?: { webview?: { postMessage(m: string): void } };
    };
    if (w.webkit?.messageHandlers?.external) w.webkit.messageHandlers.external.postMessage('wails:drag');
    else if (w.chrome?.webview) w.chrome.webview.postMessage('wails:drag');
  };

  // Two-tier title bar: a slim OS-chrome strip on top (where the macOS traffic
  // lights float, at any window size) sits above the app toolbar, so the
  // identity + repo context get their own row instead of sharing the line with
  // the native window buttons. The whole stack is one Wails drag region.
  const outerStyle = {
    background: '#111', flexShrink: 0,
    display: 'flex', flexDirection: 'column',
    ...(desktop ? { '--wails-draggable': 'drag' } : {}),
  } as CSSProperties;

  // Desktop-only: clears the traffic lights. Inherits the toolbar background
  // and has no bottom border, so the strip and toolbar read as one continuous
  // bar with the lights floating above the content. The browser/cloud build
  // has no native window controls, so it renders just the toolbar row below.
  const stripStyle: CSSProperties = {
    height: 28, flexShrink: 0,
  };

  // Toolbar row. The left zone matches the Library width so the identity sits
  // over the list column; the right zone spans the content pane and holds the
  // repo/branch/commit context + the gear.
  const rowStyle: CSSProperties = {
    height: 40, borderBottom: '1px solid #1c1c1c',
    display: 'flex', flexShrink: 0,
  };

  const leftZoneStyle: CSSProperties = {
    width: leftWidth, flexShrink: 0,
    display: 'flex', alignItems: 'center', gap: 6,
    paddingLeft: 14, paddingRight: 12,
    overflow: 'hidden',
  };

  const rightZoneStyle: CSSProperties = {
    flex: 1, minWidth: 0,
    display: 'flex', alignItems: 'center', gap: 10,
    padding: '0 14px',
  };

  return (
    <div style={outerStyle} onMouseDown={startWindowDrag}>
      {/* ── OS-chrome strip: native traffic lights float here ── */}
      {desktop && <div style={stripStyle} />}

      {/* ── Toolbar row ── */}
      <div style={rowStyle}>
      {/* ── Left zone: knomit identity ── */}
      <div style={leftZoneStyle}>
        <span style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
          <svg width="16" height="16" viewBox="0 0 80 80" style={{ display: 'block', flexShrink: 0 }}>
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
          <span style={{ color: '#7c9', fontWeight: 'bold', fontSize: 15, whiteSpace: 'nowrap' }}>knomit</span>
        </span>
      </div>

      {/* ── Right zone: repo · branch · commit ········ gear ── */}
      <div style={rightZoneStyle}>
        <span style={{ display: 'flex', alignItems: 'center', gap: 5, color: '#7c9', fontSize: 12 }}>
          <BookIcon color="currentColor" size={13} />
          {repos.length > 1 ? (
            <button
              data-testid="toknomitr-repo-select"
              data-nodrag
              ref={repoBtnRef}
              onClick={toggleRepoMenu}
              aria-haspopup="listbox"
              aria-expanded={repoOpen}
              style={{
                background: '#1a1a2a', color: '#7c9', border: '1px solid #333',
                borderRadius: 3, fontSize: 12, padding: '1px 4px 1px 6px', cursor: 'pointer',
                display: 'inline-flex', alignItems: 'center', gap: 4, fontFamily: 'inherit',
                lineHeight: 1.5, ...noDrag,
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
          <span style={{ display: 'flex', alignItems: 'center', gap: 5, color: '#8af', fontSize: 12, minWidth: 0 }}>
            <GitBranchIcon color="currentColor" size={13} />
            <span data-testid="toknomitr-branch" style={{ whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>{state.branch}</span>
          </span>
        )}
        {/* line-height 1 collapses the monospace block to its glyph extent so
            the digit caps align with the surrounding sans-serif text. */}
        {/* Commit chip — borderless icon + hash in the mode color (amber = past,
            green = now), so live and history read as the same shape. */}
        {state.asOf.mode === 'history' ? (
          <span data-testid="toknomitr-commit" style={{ display: 'inline-flex', alignItems: 'center', gap: 4, fontFamily: 'var(--k-font-mono)', fontSize: 11, lineHeight: 1, flexShrink: 0, color: '#e5a23c' }}>
            <span aria-hidden="true">⏱</span>{state.asOf.commit.slice(0, 7)}
          </span>
        ) : (
          state.headCommit && (
            <span data-testid="toknomitr-commit" style={{ display: 'inline-flex', alignItems: 'center', gap: 5, fontFamily: 'var(--k-font-mono)', fontSize: 11, lineHeight: 1, flexShrink: 0, color: '#7c9' }}>
              <span aria-hidden="true" style={{ width: 6, height: 6, borderRadius: '50%', background: '#7c9', boxShadow: '0 0 6px #7c9' }} />
              {state.headCommit.slice(0, 7)}
            </span>
          )
        )}
        <div style={{ flex: 1 }} />
        <button
          data-testid="toknomitr-manage-btn"
          data-nodrag
          onClick={onManageRepos}
          title="Manage repositories"
          style={{ background: 'none', border: 'none', color: state.remoteError ? '#f44336' : '#666', cursor: 'pointer', padding: 4, display: 'flex', alignItems: 'center', position: 'relative', flexShrink: 0, ...noDrag }}
          onMouseEnter={e => { if (!state.remoteError) e.currentTarget.style.color = '#aaa'; }}
          onMouseLeave={e => { e.currentTarget.style.color = state.remoteError ? '#f44336' : '#666'; }}
        >
          <GearIcon color="currentColor" size={15} />
          {state.remoteError && (
            <span style={{ position: 'absolute', top: 2, right: 2, width: 6, height: 6, borderRadius: '50%', background: '#f44336' }} />
          )}
        </button>
      </div>
      </div>

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
    </div>
  );
}
