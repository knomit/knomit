import { memo } from 'react';
import type { CSSProperties, MouseEvent as ReactMouseEvent } from 'react';
import type { AppState } from './state';
import { isLensContext, remoteErrorText } from './state';
import type { RepoInfo } from './api';
import { BookIcon, GearIcon, ExitIcon, LayersIcon } from './icons';
import { noMouseFocus } from './utils';

interface Props {
  state: AppState;
  /** Every listed repo. The rail toggle's dot reads their remote health. */
  repos: RepoInfo[];
  /** Whether the target rail is open; the toggle shows it lit. */
  railOpen: boolean;
  /** Opens or closes the target rail (the same thing ⌘B does). */
  onToggleRail: () => void;
  /** Toggles the Manage mode. The SAME control both enters and leaves it, at the
   *  same anchor — a way out that appears somewhere else makes the reader hunt
   *  for a control they just clicked. */
  onManageRepos: () => void;
  /** True while the Manage mode owns the window. The gear then renders as a
   *  step-out button, and the browse chrome — the rail toggle and the target
   *  label — is omitted: Manage has its own rail, and the label would name a
   *  surface that is not on screen. */
  manageOpen?: boolean;
  /** True when Manage cannot be left — the zero-repository case, where there is
   *  no browse surface to return to. The toggle is then not rendered at all: a
   *  disabled exit is a control that answers "can I leave?" with noise. */
  manageLocked?: boolean;
  /** True while Manage is doing something that must not be interrupted — today,
   *  a connect commit swapping a repository's store. Unlike `manageLocked` the
   *  exit stays RENDERED and merely refuses: this state is temporary, and a
   *  control that vanished mid-flow would read as the window losing its way
   *  out rather than holding it. The tooltip carries the reason. */
  manageBusy?: boolean;
}

// The top bar is the first of the shell's three levels, and it never changes
// with what is below it: [rail toggle][logo] │ [target label] ······ [gear].
//
// The label is STATIC. Picking a repository or lens is the rail's job now, so
// the dropdown that used to hang off the name is gone; the branch picker and
// the filter moved to the Browse toolbar, because they act on the data being
// read and must not stay on screen over a page that reads none.
export const TopBar = memo(function TopBar({ state, railOpen, onToggleRail, onManageRepos, manageOpen = false, manageLocked = false, manageBusy = false }: Props) {
  const lensCtx = isLensContext(state);
  // The gear's red dot marks an unhealthy remote of EITHER kind on the CURRENT
  // repo — a rejected push is as much a reason to open the manager as an
  // unreachable origin. It moves to the Settings tab when the facets land.
  const remoteError = remoteErrorText(state);
  // Where leaving Manage puts you back: the surface you were reading, which is
  // the lens in a lens context and the repo otherwise. Named in the exit's
  // tooltip so the destination is one hover away without a label in the bar.
  const manageReturnTo = state.context.kind === 'lens' ? state.context.name : state.repo;
  // "Held": in Manage, and Manage says it is mid-something uninterruptible.
  // Only meaningful with manageOpen — busy is a state of the mode, not of a bar
  // that is not showing it.
  const held = manageOpen && manageBusy;

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

  // Toolbar row. Stretch, so the label's left rule (and, later, the facet
  // tabs' underline) runs the full height of the bar.
  const rowStyle: CSSProperties = {
    height: 40, borderBottom: '1px solid #1c1c1c',
    display: 'flex', alignItems: 'stretch', flexShrink: 0,
    padding: '0 12px 0 8px', fontSize: 12,
  };

  const leadStyle: CSSProperties = {
    display: 'flex', alignItems: 'center', gap: 8, paddingRight: 16, flexShrink: 0,
  };

  const label = lensCtx
    ? { name: state.context.kind === 'lens' ? state.context.name : '', color: LENS_ACCENT, icon: <LayersIcon color="currentColor" size={13} /> }
    : { name: state.repo, color: '#7c9', icon: <BookIcon color="currentColor" size={13} /> };

  return (
    <div style={outerStyle} onMouseDown={startWindowDrag}>
      {/* ── OS-chrome strip: native traffic lights float here ── */}
      {desktop && <div style={stripStyle} />}

      {/* ── Toolbar row ── */}
      <div data-testid="toknomitr-bar" style={rowStyle}>
        {/* ── Lead: rail toggle · knomit identity ──
            The toggle is withheld in Manage, which has its own rail: two rails
            in one window would be two answers to "where do I pick a repo". */}
        <div style={leadStyle}>
          {!manageOpen && (
            <button
              type="button"
              data-testid="rail-toggle"
              data-nodrag
              onClick={onToggleRail}
              onMouseDown={noMouseFocus}
              aria-pressed={railOpen}
              aria-label={railOpen ? 'Hide repositories and lenses' : 'Show repositories and lenses'}
              title={railOpen ? 'Hide repositories and lenses  (⌘B)' : 'Show repositories and lenses  (⌘B)'}
              style={{
                width: 26, height: 26, flexShrink: 0, position: 'relative',
                display: 'flex', alignItems: 'center', justifyContent: 'center',
                border: 'none', borderRadius: 4, padding: 0, cursor: 'pointer',
                color: railOpen ? '#dfe9e2' : '#777',
                background: railOpen ? '#1a231d' : 'transparent',
                ...noDrag,
              }}
            >
              <PanelIcon />
            </button>
          )}
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

        {/* ── Target label: what the page below is about ──
            A label, not a control: nothing here opens anything, so it stays
            part of the drag region. Withheld in Manage, where the page below
            is not about it. */}
        {!manageOpen && (
          <span data-testid="target-label" style={{
            display: 'flex', alignItems: 'center', gap: 6, minWidth: 0,
            padding: '0 14px', borderLeft: '1px solid #1e1e1e',
            fontSize: 13, fontWeight: 500, whiteSpace: 'nowrap',
            color: label.color,
          }}>
            {label.icon}
            <span style={{ overflow: 'hidden', textOverflow: 'ellipsis' }}>{label.name}</span>
          </span>
        )}

        <div style={{ flex: 1 }} />

        {/* One control, one anchor. In browse it is the gear that opens Manage;
            in Manage it is the step-out that leaves. Same handler, same pixel —
            an exit that appears on the other side of the window would make the
            reader hunt for the control they had just used. The tooltip carries
            the destination so the bar needs no label of its own. */}
        {!manageLocked && <button
          data-testid="toknomitr-manage-btn"
          data-nodrag
          onClick={onManageRepos}
          onMouseDown={noMouseFocus}
          aria-pressed={manageOpen}
          disabled={held}
          title={held ? 'Manage is finishing something — leave it running' : manageOpen ? `Leave Manage — back to ${manageReturnTo}  (Esc)` : 'Manage repositories'}
          aria-label={held ? 'Manage is finishing something — leave it running' : manageOpen ? `Leave Manage — back to ${manageReturnTo}` : 'Manage repositories'}
          // One control, one look: only the GLYPH changes between modes. A
          // tinted pill would make the way out read as a different kind of
          // thing from the way in, when it is the same button in the same pixel.
          style={{ background: 'none', border: 'none', color: held ? '#3d3d3d' : !manageOpen && remoteError ? '#f44336' : '#666', cursor: held ? 'default' : 'pointer', padding: 4, display: 'flex', alignItems: 'center', alignSelf: 'center', position: 'relative', flexShrink: 0, ...noDrag }}
          onMouseEnter={e => { if (!held && (manageOpen || !remoteError)) e.currentTarget.style.color = '#aaa'; }}
          onMouseLeave={e => { e.currentTarget.style.color = held ? '#3d3d3d' : !manageOpen && remoteError ? '#f44336' : '#666'; }}
        >
          {manageOpen ? <ExitIcon color="currentColor" size={15} /> : <GearIcon color="currentColor" size={15} />}
          {!manageOpen && remoteError && (
            <span style={{ position: 'absolute', top: 2, right: 2, width: 6, height: 6, borderRadius: '50%', background: '#f44336' }} />
          )}
        </button>}
      </div>
    </div>
  );
});

// The spec's lens accent, for the label.
const LENS_ACCENT = '#b9a6f0';

// PanelIcon is the rail toggle's glyph: a window with its left column ruled off.
const PanelIcon = () => (
  <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
    <rect x="3" y="4" width="18" height="16" rx="2" />
    <line x1="9" y1="4" x2="9" y2="20" />
  </svg>
);
