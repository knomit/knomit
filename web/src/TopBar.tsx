import { memo, useState, useRef, useEffect } from 'react';
import type { Dispatch, CSSProperties, ReactNode, MouseEvent as ReactMouseEvent } from 'react';
import { createPortal } from 'react-dom';
import type { AppState, Action } from './state';
import { isLensContext, remoteErrorText } from './state';
import { api, repoAvailable, brokenLensMember } from './api';
import type { RepoInfo, Lens, ExperimentRow } from './api';
import { RepoStateChip } from './RepoStateChip';
import { RepoIndexChip } from './RepoIndexChip';
import { useRepoCreates, activeCreateByRepo, createFlag } from './useRepoCreates';
import { useDismiss } from './hooks';
import { BookIcon, GitBranchIcon, ChevronDownIcon, GearIcon, ExitIcon, LayersIcon } from './icons';
import { LENS, repoHue, shortBranch, noMouseFocus } from './utils';
import { MountsPicker } from './MountsPicker';

interface Props {
  state: AppState;
  repos: RepoInfo[];
  /** All lenses (for the switcher's Lenses group). Defaults to [] when the
   *  caller hasn't loaded them yet, so the repo group still renders. */
  lenses?: Lens[];
  dispatch: Dispatch<Action>;
  /** Toggles the Manage mode. The SAME control both enters and leaves it, at the
   *  same anchor — a way out that appears somewhere else makes the reader hunt
   *  for a control they just clicked. */
  onManageRepos: () => void;
  /** Switch the app to a branch of the current repo. Without it the branch
   *  chip stays the static label it was before the picker existed. */
  onEnterBranch?: (branch: string) => void;
  /** True while the Manage mode owns the window. The gear then renders as a
   *  step-out button, and the browse context (repo/branch chips, search) is
   *  omitted: those belong to the surface you are READING, and offering them
   *  here would switch a surface that is not currently on screen. */
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
export const TopBar = memo(function TopBar({ state, repos, lenses = [], dispatch, onManageRepos, onEnterBranch, manageOpen = false, manageLocked = false, manageBusy = false, leftWidth, search }: Props) {
  // The creates still working on listed repos, so the switcher flags them the
  // same way the manage rail does. The poller is already mounted here for
  // CreateIndicator below, so this reads a list that is being fetched anyway.
  const activeCreates = activeCreateByRepo(useRepoCreates());
  const [menuOpen, setMenuOpen] = useState(false);
  const menuBtnRef = useRef<HTMLButtonElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const [menuPos, setMenuPos] = useState({ top: 0, left: 0, minWidth: 0 });
  // The branch picker is a SECOND menu, not a second group inside the repo
  // one: it is anchored to a different chip and answers a different question
  // ("which branch of this repo", not "which knowledge base"), and folding
  // them would put two unrelated switches behind one caret.
  const [branchOpen, setBranchOpen] = useState(false);
  const branchBtnRef = useRef<HTMLButtonElement>(null);
  const branchMenuRef = useRef<HTMLDivElement>(null);
  const [branchPos, setBranchPos] = useState({ top: 0, left: 0, minWidth: 0 });
  const [branchRows, setBranchRows] = useState<ExperimentRow[]>([]);

  const lensCtx = isLensContext(state);
  // The gear's red dot marks an unhealthy remote of EITHER kind — a rejected
  // push is as much a reason to open the manager as an unreachable origin.
  const remoteError = remoteErrorText(state);
  // The switcher trigger appears when there's more than one surface to pick:
  // multiple repos, any lens (even with a single repo), or a lens context.
  const showTrigger = repos.length > 1 || lenses.length > 0 || lensCtx;
  // Where leaving Manage puts you back: the surface you were reading, which is
  // the lens in a lens context and the repo otherwise. Named in the exit's
  // tooltip so the destination is one hover away without a label in the bar.
  const manageReturnTo = state.context.kind === 'lens' ? state.context.name : state.repo;
  // "Held": in Manage, and Manage says it is mid-something uninterruptible.
  // Only meaningful with manageOpen — busy is a state of the mode, not of a bar
  // that is not showing it.
  const held = manageOpen && manageBusy;

  useDismiss(menuOpen, () => setMenuOpen(false), [menuBtnRef, menuRef]);
  useDismiss(branchOpen, () => setBranchOpen(false), [branchBtnRef, branchMenuRef]);

  // Fetched when the menu OPENS rather than held in app state: the list is
  // small, it is only ever read here, and a stale one would offer a branch
  // that a commit elsewhere has since deleted.
  useEffect(() => {
    if (!branchOpen || !state.repo) return;
    let alive = true;
    api.listExperiments(state.repo)
      .then(res => { if (alive) setBranchRows(res.experiments); })
      .catch(() => { if (alive) setBranchRows([]); });
    return () => { alive = false; };
  }, [branchOpen, state.repo]);

  const toggleBranchMenu = () => {
    if (!branchOpen && branchBtnRef.current) {
      const rect = branchBtnRef.current.getBoundingClientRect();
      setBranchPos({ top: rect.bottom + 6, left: rect.left, minWidth: rect.width });
    }
    setBranchOpen(o => !o);
  };

  const pickBranch = (branch: string) => {
    setBranchOpen(false);
    if (branch && branch !== state.branch) onEnterBranch?.(branch);
  };

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

      {/* ── Right zone: context · (branch|mounts) · commit ········ gear ──
          In Manage the context chips and the filter are gone: both act on the
          surface being READ, and this mode is not reading one. What is left is
          the spacer and the exit, so the control keeps the right edge it had. */}
      <div style={rightZoneStyle}>
        {manageOpen ? <div style={{ flex: 1 }} /> : lensCtx ? (
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
              /* Inside an experiment the chip turns GREEN and names the
                 experiment rather than the branch. Green already means "write
                 target" in this UI (the Agent-branch card, the write mount
                 tag), and inside an experiment that is exactly what this
                 branch is — so the marker is a change of state on the chip
                 the user already reads, not a second badge somewhere else. */
              <button
                ref={branchBtnRef}
                data-testid="toknomitr-branch"
                data-experiment={state.experiment ? state.experiment.name : undefined}
                onClick={onEnterBranch ? toggleBranchMenu : undefined}
                aria-haspopup={onEnterBranch ? 'listbox' : undefined}
                aria-expanded={onEnterBranch ? branchOpen : undefined}
                title={state.experiment
                  ? `Experiment ${state.experiment.name} — forked from ${state.experiment.parent}`
                  : state.branch}
                style={{
                  display: 'flex', alignItems: 'center', gap: 5, minWidth: 0,
                  fontSize: 12, fontFamily: 'inherit', lineHeight: 1.5,
                  color: state.experiment ? '#7c9' : '#8af',
                  background: state.experiment ? '#11201a' : 'transparent',
                  border: '1px solid ' + (state.experiment ? '#2a4a3a' : 'transparent'),
                  borderRadius: 3, padding: state.experiment ? '1px 6px' : '1px 2px',
                  cursor: onEnterBranch ? 'pointer' : 'default',
                  ...noDrag,
                }}
              >
                <GitBranchIcon color="currentColor" size={13} />
                {state.experiment && (
                  <span data-testid="toknomitr-experiment-marker" style={{
                    fontSize: 9.5, textTransform: 'uppercase', letterSpacing: 1,
                    border: '1px solid #2a4a3a', borderRadius: 2, padding: '0 3px',
                  }}>exp</span>
                )}
                <span style={{ whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>
                  {state.experiment ? state.experiment.name : shortBranch(state.branch)}
                </span>
                <ChevronDownIcon color="currentColor" size={11} />
              </button>
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
        {manageOpen || !search
          ? <div style={{ flex: 1 }} />
          : <div data-testid="toknomitr-search" data-nodrag style={{ flex: 1, minWidth: 0, ...noDrag }}>{search}</div>}
        {/* The global create light, immediately left of the gear it sends you
            to. A detached create is invisible from every view but the wizard
            that started it, and the wizard is the first thing a user closes —
            so a subscribe could run for minutes with nothing anywhere on
            screen saying so. It renders nothing when nothing is running. */}
        <div data-nodrag style={{ ...noDrag, display: 'flex', alignItems: 'center', flexShrink: 0 }}>

        </div>
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
          style={{ background: 'none', border: 'none', color: held ? '#3d3d3d' : !manageOpen && remoteError ? '#f44336' : '#666', cursor: held ? 'default' : 'pointer', padding: 4, display: 'flex', alignItems: 'center', position: 'relative', flexShrink: 0, ...noDrag }}
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

      {/* ── Branch picker ── the agent branch, then this repo's experiments,
          then the way to make one. Deliberately NOT a list of every branch:
          the consensus branch and other machines' agent branches are not
          writable, and offering them here would invite the user into a
          read-only view with no way to tell why from the menu. */}
      {branchOpen && !manageOpen && createPortal(
        <div ref={branchMenuRef} role="listbox" data-testid="toknomitr-branch-menu" style={{
          position: 'fixed', top: branchPos.top, left: branchPos.left,
          minWidth: Math.max(branchPos.minWidth, 240),
          background: '#1a1a1a', border: '1px solid #333', borderRadius: 6,
          zIndex: 10000, boxShadow: '0 4px 12px rgba(0,0,0,0.5)',
          padding: '5px 0', maxHeight: 340, overflowY: 'auto',
        }}>
          <div style={groupHeaderStyle}>
            <GitBranchIcon color="#6a8" size={11} /> Branch
          </div>
          {(() => {
            // The agent branch is wherever an experiment says it forked from;
            // outside one it is the branch we are on. Derived rather than
            // fetched: the row we already hold names its own parent, which is
            // the same value and one fewer request.
            const agent = state.experiment ? state.experiment.parent : state.branch;
            const onAgent = !state.experiment;
            return (
              <div
                role="option" aria-selected={onAgent}
                data-testid="toknomitr-branch-option-agent"
                onClick={() => pickBranch(agent)}
                style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '6px 12px', cursor: 'pointer', fontSize: 12, color: onAgent ? '#7c9' : '#aaa' }}
                onMouseEnter={e => { e.currentTarget.style.background = '#2a2a3a'; }}
                onMouseLeave={e => { e.currentTarget.style.background = 'transparent'; }}
              >
                <span style={{ width: 10, color: '#7c9' }}>{onAgent ? '✓' : ''}</span>
                <span style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{shortBranch(agent)}</span>
                <span style={{ fontSize: 10, color: '#6a7078' }}>agent branch</span>
              </div>
            );
          })()}

          {branchRows.length > 0 && (
            <div style={groupHeaderStyle}>
              <GitBranchIcon color="#6a8" size={11} /> Experiments
            </div>
          )}
          {branchRows.map(row => {
            const active = row.branch === state.branch;
            // An orphaned experiment is listed but NOT selectable: entering it
            // would land the user in a read-only view whose reason is not
            // visible from here. The repository page is where it can be
            // rolled back, which is its only remaining action.
            const usable = row.writable !== false;
            return (
              <div
                key={row.name} role="option" aria-selected={active}
                aria-disabled={!usable || undefined}
                data-testid={`toknomitr-branch-option-${row.name}`}
                title={usable ? row.description || undefined : `Forked from ${row.parent}, which is not this instance's agent branch`}
                onClick={usable ? () => pickBranch(row.branch) : undefined}
                style={{
                  display: 'flex', alignItems: 'center', gap: 8, padding: '6px 12px',
                  cursor: usable ? 'pointer' : 'default', fontSize: 12,
                  color: active ? '#7c9' : usable ? '#aaa' : '#6a6a6a',
                }}
                onMouseEnter={e => { if (usable) e.currentTarget.style.background = '#2a2a3a'; }}
                onMouseLeave={e => { if (usable) e.currentTarget.style.background = 'transparent'; }}
              >
                <span style={{ width: 10, color: '#7c9' }}>{active ? '✓' : ''}</span>
                <span style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{row.name}</span>
                {!usable && <span style={{ fontSize: 10, color: '#c9a227' }}>orphaned</span>}
              </div>
            );
          })}

          <div style={{ borderTop: '1px solid #262626', marginTop: 4, paddingTop: 4 }}>
            <div
              role="option" aria-selected={false}
              data-testid="toknomitr-branch-new"
              onClick={() => { setBranchOpen(false); onManageRepos(); }}
              style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '6px 12px', cursor: 'pointer', fontSize: 12, color: '#6ea8fe' }}
              onMouseEnter={e => { e.currentTarget.style.background = '#2a2a3a'; }}
              onMouseLeave={e => { e.currentTarget.style.background = 'transparent'; }}
            >
              <span style={{ width: 10 }} />
              <span>New experiment…</span>
            </div>
          </div>
        </div>,
        document.body,
      )}

      {menuOpen && !manageOpen && createPortal(
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
            // A registered repo with no live store is listed but NOT selectable:
            // there is nothing behind it to browse, and every request this pick
            // would fire answers 409. It stays visible — vanishing is exactly
            // the failure mode the server-side registry was built to end — and
            // the chip says which kind of broken it is. Manage is where it can
            // be acted on, so this row reports rather than navigates.
            const available = repoAvailable(r);
            return (
              <div
                key={r.name}
                role="option"
                aria-selected={active}
                aria-disabled={!available || undefined}
                data-testid={`toknomitr-repo-option-${r.name}`}
                data-repo-state={r.state ?? 'active'}
                title={available ? undefined : r.detail || `This repository has no store (${r.state}).`}
                onClick={available ? () => pickRepo(r.name) : undefined}
                style={{
                  display: 'flex', alignItems: 'center', gap: 8,
                  padding: '6px 12px',
                  cursor: available ? 'pointer' : 'default',
                  color: active ? '#7c9' : available ? '#aaa' : '#6a6a6a', fontSize: 12,
                }}
                onMouseEnter={e => { if (!available) return; e.currentTarget.style.background = '#2a2a3a'; if (!active) e.currentTarget.style.color = '#eee'; }}
                onMouseLeave={e => { if (!available) return; e.currentTarget.style.background = 'transparent'; e.currentTarget.style.color = active ? '#7c9' : '#aaa'; }}
              >
                <span style={{ width: 10, color: '#7c9' }}>{active ? '✓' : ''}</span>
                <span aria-hidden="true" style={{
                  width: 7, height: 7, borderRadius: '50%', background: repoHue(r.name),
                  flexShrink: 0, opacity: available ? 1 : 0.4,
                }} />
                <span>{r.name}</span>
                {/* Same precedence as the manage rail: while a create is still
                    working on this repo, that is the truth about it, and an
                    index chip beside it would report a detail of unsettled
                    work — at worst "indexing" on a repo being deleted. */}
                {activeCreates.get(r.name)
                  ? <span data-testid={`toknomitr-create-chip-${r.name}`} style={createChip}>
                      {createFlag(activeCreates.get(r.name)!.state)}
                    </span>
                  : <>
                      {!available && <RepoStateChip repo={r} />}
                      {available && <RepoIndexChip repo={r} />}
                    </>}
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
                // Same rule as the repo rows above, for the same reason. A lens
                // binds ALL its members or none, so one member without a live
                // store makes every read endpoint under the lens answer 503 —
                // while GET /lenses/{lens} itself, which sits outside the lens
                // middleware, still answers 200. Entering here would land the
                // user in a surface that fails on arrival, and the resolve
                // rescue cannot save them: the fetch SUCCEEDS.
                const broken = brokenLensMember(l, repos);
                const available = broken === null;
                return (
                  <div
                    key={l.name}
                    role="option"
                    aria-selected={active}
                    aria-disabled={!available || undefined}
                    data-testid={`toknomitr-lens-option-${l.name}`}
                    data-lens-available={available ? undefined : 'false'}
                    title={available ? undefined : `This lens cannot be read: its mount "${broken}" has no store.`}
                    onClick={available ? () => pickLens(l.name) : undefined}
                    style={{
                      display: 'flex', alignItems: 'center', gap: 8,
                      padding: '6px 12px',
                      cursor: available ? 'pointer' : 'default',
                      opacity: available ? 1 : 0.55,
                      background: active ? LENS.soft : 'transparent',
                    }}
                    onMouseEnter={e => { if (!available) return; if (!active) e.currentTarget.style.background = '#26243a'; }}
                    onMouseLeave={e => { if (!available) return; e.currentTarget.style.background = active ? LENS.soft : 'transparent'; }}
                  >
                    <span style={{ width: 10, color: LENS.accent }}>{active ? '✓' : ''}</span>
                    <LayersIcon color={LENS.accent} size={13} />
                    <span style={{ display: 'flex', flexDirection: 'column' }}>
                      <span style={{ fontSize: 12, color: active ? LENS.accent : available ? '#ccc' : '#7a7a7a' }}>{l.name}</span>
                      <span style={{ fontSize: 10.5, color: '#888' }}>
                        {available ? `${l.reads.length} mounts · → ${l.write.name}` : `unavailable · ${broken} has no store`}
                      </span>
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

// createChip flags a repo the switcher lists while a create is still working
// on it. Mirrors the manage rail's chip — one fact, one rendering.
const createChip: CSSProperties = {
  display: 'inline-flex', alignItems: 'center',
  fontSize: 9.5, lineHeight: 1.7, padding: '0 5px', borderRadius: 3,
  fontFamily: 'var(--k-font-mono)', whiteSpace: 'nowrap', flexShrink: 0,
  color: '#8ab6d6', background: '#131d26', border: '1px solid #244056',
};
