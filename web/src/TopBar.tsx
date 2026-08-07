import { memo, useState, useRef } from 'react';
import type { Dispatch, CSSProperties, ReactNode, MouseEvent as ReactMouseEvent } from 'react';
import { createPortal } from 'react-dom';
import type { AppState, Action } from './state';
import { isLensContext, remoteErrorText } from './state';
import type { RepoInfo, Lens } from './api';
import { useDismiss } from './hooks';
import { BookIcon, GitBranchIcon, ChevronDownIcon, GearIcon, LayersIcon } from './icons';
import { LENS, repoHue, shortBranch } from './utils';
import { MountsPicker } from './MountsPicker';

interface Props {
  state: AppState;
  repos: RepoInfo[];
  /** All lenses (for the switcher's Lenses group). Defaults to [] when the
   *  caller hasn't loaded them yet, so the repo group still renders. */
  lenses?: Lens[];
  dispatch: Dispatch<Action>;
  onManageRepos: () => void;
  /** Live width of the Library panel; the title-bar identity zone matches it
   *  so the divider lines up with the splitter below. */
  leftWidth: number;
  /** The filter input, handed in rather than built here so the bar stays a
   *  layout. Omitted in history mode, where the trail breadcrumb takes over
   *  below and there is nothing to filter. */
  search?: ReactNode;
}

// The row is sorted by what you can act on: every element here opens something.
// The two that only reported — the commit, and the lens write target — moved to
// the StatusFooter, which is the readout rail. What is left is the same shape in
// both contexts: the switcher, then the scope picker, then search.
export const TopBar = memo(function TopBar({ state, repos, lenses = [], dispatch, onManageRepos, leftWidth, search }: Props) {
  const [menuOpen, setMenuOpen] = useState(false);
  const menuBtnRef = useRef<HTMLButtonElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const [menuPos, setMenuPos] = useState({ top: 0, left: 0, minWidth: 0 });

  const lensCtx = isLensContext(state);
  // The gear's red dot marks an unhealthy remote of EITHER kind — a rejected
  // push is as much a reason to open the manager as an unreachable origin.
  const remoteError = remoteErrorText(state);
  // The switcher trigger appears when there's more than one surface to pick:
  // multiple repos, any lens (even with a single repo), or a lens context.
  const showTrigger = repos.length > 1 || lenses.length > 0 || lensCtx;

  useDismiss(menuOpen, () => setMenuOpen(false), [menuBtnRef, menuRef]);

  const toggleMenu = () => {
    if (!menuOpen && menuBtnRef.current) {
      const rect = menuBtnRef.current.getBoundingClientRect();
      setMenuPos({ top: rect.bottom + 4, left: rect.left, minWidth: rect.width });
    }
    setMenuOpen(o => !o);
  };

  const pickRepo = (name: string) => {
    setMenuOpen(false);
    // No-op only when already in this repo context. In a lens context
    // (context.kind !== 'repo') a repo pick always switches surface, even if the
    // name matches the lens's write mount (state.repo).
    if (state.context.kind === 'repo' && name === state.repo) return;
    dispatch({ type: 'SET_REPO', repo: name });
  };

  const pickLens = (name: string) => {
    setMenuOpen(false);
    if (state.context.kind === 'lens' && name === state.context.name) return;
    dispatch({ type: 'SET_CONTEXT', context: { kind: 'lens', name } });
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

  // Dropdown group header: small uppercase label with an icon (Repositories /
  // Lenses). Verbatim spec from the design handoff (Part 2 §switcher).
  const groupHeaderStyle: CSSProperties = {
    fontSize: 10, letterSpacing: '.1em', textTransform: 'uppercase', color: '#555',
    padding: '5px 12px 3px', display: 'flex', alignItems: 'center', gap: 6,
  };

  return (
    <div style={outerStyle} onMouseDown={startWindowDrag}>
      {/* ── OS-chrome strip: native traffic lights float here ── */}
      {desktop && <div style={stripStyle} />}

      {/* ── Toolbar row ── */}
      <div data-testid="toknomitr-bar" style={rowStyle}>
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

      {/* ── Right zone: context · (branch|mounts) · commit ········ gear ── */}
      <div style={rightZoneStyle}>
        {lensCtx ? (
          /* ── LENS context: lens chip · N mounts · write-target pill · commit ── */
          <>
            <span style={{ display: 'flex', alignItems: 'center', gap: 5, color: LENS.accent, fontSize: 12 }}>
              <LayersIcon color="currentColor" size={13} />
              <button
                data-testid="toknomitr-lens-select"
                data-nodrag
                ref={menuBtnRef}
                onClick={toggleMenu}
                aria-haspopup="listbox"
                aria-expanded={menuOpen}
                style={{
                  background: LENS.bg, color: LENS.accent, border: `1px solid ${LENS.border}`,
                  borderRadius: 3, fontSize: 12, padding: '1px 4px 1px 6px', cursor: 'pointer',
                  display: 'inline-flex', alignItems: 'center', gap: 4, fontFamily: 'inherit',
                  lineHeight: 1.5, ...noDrag,
                }}
              >
                <span>{state.context.kind === 'lens' ? state.context.name : ''}</span>
                <ChevronDownIcon color="currentColor" size={11} />
              </button>
            </span>
            {/* The scope control. This slot used to render lens.reads.length —
                the TOTAL, always, whatever the reader had selected — while the
                actual picker sat in the left panel under a SOURCES label. Two
                places showed the same fact and the more prominent one was the
                one that could not be true, so the readout became the control
                and the left panel's block went away. */}
            {state.lens && (
              <MountsPicker lens={state.lens} selection={state.lensSources} dispatch={dispatch} />
            )}
          </>
        ) : (
          /* ── REPO context (unchanged): book chip · branch · commit ── */
          <>
            <span style={{ display: 'flex', alignItems: 'center', gap: 5, color: '#7c9', fontSize: 12 }}>
              <BookIcon color="currentColor" size={13} />
              {showTrigger ? (
                <button
                  data-testid="toknomitr-repo-select"
                  data-nodrag
                  ref={menuBtnRef}
                  onClick={toggleMenu}
                  aria-haspopup="listbox"
                  aria-expanded={menuOpen}
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
            {/* Trimmed to the machine name: identity.go builds these as
                agent/<host>-<fp8>, where the prefix is constant across every
                agent branch and the fingerprint only separates two agents on
                one host. 68px instead of 196px, and the title keeps the whole
                thing. The caret is here before the picker is: switching
                branches is coming, and adding the affordance with the layout
                means that day is a behaviour change, not a visual one. */}
            {state.branch && (
              <span
                data-testid="toknomitr-branch"
                title={state.branch}
                style={{ display: 'flex', alignItems: 'center', gap: 5, color: '#8af', fontSize: 12, minWidth: 0 }}
              >
                <GitBranchIcon color="currentColor" size={13} />
                <span style={{ whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>{shortBranch(state.branch)}</span>
                <ChevronDownIcon color="currentColor" size={11} />
              </span>
            )}
          </>
        )}
        {/* Search takes the remainder, so it is the element that absorbs a
            narrowing window rather than the context chips truncating. It sits
            here because it is global: it governs the fact list, and it used to
            render as a band over the RIGHT pane, which reads state.filters
            exactly zero times.

            Without it the spacer still has to be here to hold the gear right —
            history mode has no filter input, since the trail breadcrumb takes
            over below and there is nothing to type into. */}
        {search
          ? <div data-testid="toknomitr-search" data-nodrag style={{ flex: 1, minWidth: 0, ...noDrag }}>{search}</div>
          : <div style={{ flex: 1 }} />}
        <button
          data-testid="toknomitr-manage-btn"
          data-nodrag
          onClick={onManageRepos}
          title="Manage repositories"
          style={{ background: 'none', border: 'none', color: remoteError ? '#f44336' : '#666', cursor: 'pointer', padding: 4, display: 'flex', alignItems: 'center', position: 'relative', flexShrink: 0, ...noDrag }}
          onMouseEnter={e => { if (!remoteError) e.currentTarget.style.color = '#aaa'; }}
          onMouseLeave={e => { e.currentTarget.style.color = remoteError ? '#f44336' : '#666'; }}
        >
          <GearIcon color="currentColor" size={15} />
          {remoteError && (
            <span style={{ position: 'absolute', top: 2, right: 2, width: 6, height: 6, borderRadius: '50%', background: '#f44336' }} />
          )}
        </button>
      </div>
      </div>

      {menuOpen && createPortal(
        <div ref={menuRef} role="listbox" data-testid="toknomitr-repo-menu" style={{
          position: 'fixed',
          top: menuPos.top,
          left: menuPos.left,
          minWidth: Math.max(menuPos.minWidth, 200),
          background: '#1a1a1a',
          border: '1px solid #333',
          borderRadius: 6,
          zIndex: 10000,
          boxShadow: '0 4px 12px rgba(0,0,0,0.5)',
          padding: '5px 0',
          maxHeight: 340,
          overflowY: 'auto',
        }}>
          {/* ── Repositories group ── */}
          <div data-testid="toknomitr-group-repos" style={groupHeaderStyle}>
            <BookIcon color="#6a8" size={11} /> Repositories
          </div>
          {repos.map(r => {
            const active = state.context.kind === 'repo' && r.name === state.repo;
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
                <span aria-hidden="true" style={{ width: 7, height: 7, borderRadius: '50%', background: repoHue(r.name), flexShrink: 0 }} />
                <span>{r.name}</span>
              </div>
            );
          })}

          {/* ── Lenses group (omitted when there are no lenses) ── */}
          {lenses.length > 0 && (
            <>
              <div style={{ borderTop: '1px solid #2a2a2a', margin: '5px 0' }} />
              <div data-testid="toknomitr-group-lenses" style={groupHeaderStyle}>
                <LayersIcon color={LENS.accent} size={11} /> Lenses
              </div>
              {lenses.map(l => {
                const active = state.context.kind === 'lens' && l.name === state.context.name;
                return (
                  <div
                    key={l.name}
                    role="option"
                    aria-selected={active}
                    data-testid={`toknomitr-lens-option-${l.name}`}
                    onClick={() => pickLens(l.name)}
                    style={{
                      display: 'flex', alignItems: 'center', gap: 8,
                      padding: '6px 12px',
                      cursor: 'pointer',
                      background: active ? LENS.soft : 'transparent',
                    }}
                    onMouseEnter={e => { if (!active) e.currentTarget.style.background = '#26243a'; }}
                    onMouseLeave={e => { e.currentTarget.style.background = active ? LENS.soft : 'transparent'; }}
                  >
                    <span style={{ width: 10, color: LENS.accent }}>{active ? '✓' : ''}</span>
                    <LayersIcon color={LENS.accent} size={13} />
                    <span style={{ display: 'flex', flexDirection: 'column' }}>
                      <span style={{ fontSize: 12, color: active ? LENS.accent : '#ccc' }}>{l.name}</span>
                      <span style={{ fontSize: 10.5, color: '#888' }}>{l.reads.length} mounts · → {l.write}</span>
                    </span>
                  </div>
                );
              })}
            </>
          )}
        </div>,
        document.body
      )}
    </div>
  );
});
