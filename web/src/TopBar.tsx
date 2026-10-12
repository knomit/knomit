import { memo } from 'react';
import type { CSSProperties, MouseEvent as ReactMouseEvent } from 'react';
import type { AppState } from './state';
import { isLensContext } from './state';
import type { RepoInfo } from './api';
import { BookIcon, GearIcon, LayersIcon } from './icons';
import { noMouseFocus } from './utils';
import { otherRepoFailing } from './facets';
import type { Facet, ServerPage, TargetLabel } from './facets';

interface Props {
  state: AppState;
  /** Every listed repo. The rail toggle's dot reads their remote health. */
  repos: RepoInfo[];
  /** The displayed target's label (facets.ts targetLabelFor). Absent, the
   *  label is the browse context's. */
  label?: TargetLabel;
  /** Whether the target rail is open; the toggle shows it lit. */
  railOpen: boolean;
  /** Opens or closes the target rail (the same thing ⌘B does). */
  onToggleRail: () => void;
  /** The current target's facets, from facetsFor — the only tabs drawn. A
   *  facet the target lacks is omitted, never disabled. */
  facets?: Facet[];
  /** The facet on screen; its tab is the active one. */
  facet?: Facet;
  onFacet?: (f: Facet) => void;
  /** The Settings tab's red dot: the current repo's remote is failing. The
   *  caller withholds it on a read-only server. */
  settingsDot?: boolean;
  /** A connect commit is in flight: the tabs and the gear refuse, because
   *  each would unmount the page the commit is reporting to. */
  locked?: boolean;
  /** The gear: opens the server pages, and from them returns to the target
   *  and facet that were on screen. The SAME control both ways, at the same
   *  anchor — a way out that appears somewhere else makes the reader hunt for
   *  a control they just clicked. */
  onGear: () => void;
  /** The server pages, while they are on screen. The target crumb is then
   *  gone, the tabs are the server's, and the gear is lit. */
  server?: ServerBar | null;
}

/** What the bar shows over the server pages. */
export interface ServerBar {
  /** The page on screen; its tab is the active one. */
  page: ServerPage;
  /** serverPagesFor's list, the only tabs drawn. */
  pages: ServerPage[];
  onPage: (p: ServerPage) => void;
  /** Requests waiting, badged on Authorizations when above zero. */
  authCount: number | null;
}

// The top bar is the first of the shell's three levels, and it never changes
// with what is below it: [rail toggle][mark knomit] / target │ tabs ······ [gear], the breadcrumb
// reading the way a repo header does. Over the server pages it is knomit │ tabs.
//
// The label is STATIC. Picking a repository or lens is the rail's job now, so
// the dropdown that used to hang off the name is gone; the branch picker and
// the filter moved to the Browse toolbar, because they act on the data being
// read and must not stay on screen over a page that reads none.
export const TopBar = memo(function TopBar({ state, repos, label: given, railOpen, onToggleRail, facets = [], facet = 'browse', onFacet, settingsDot = false, locked = false, onGear, server = null }: Props) {
  const lensCtx = isLensContext(state);
  const serverOpen = server !== null;
  // Held while a connect commit runs in the Settings facet: opening the server
  // pages would unmount the page it reports to. (The server pages host nothing
  // that locks, and the lock cannot be raised while they are open.)
  const held = locked;
  const heldReason = 'A remote is being connected — leave it running';
  // The toggle's dot: some repo OTHER than the current one has a failing
  // remote, so the rail has something to show you. The current repo is left
  // out because its failure already has the banner. Hidden on a read-only
  // server, where the Remote settings are hidden and the dot would point at
  // nothing.
  const othersFailing = !state.serverReadOnly && otherRepoFailing(repos, state.repo);

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
    height: 40, borderBottom: '1px solid #1e1e1e',
    display: 'flex', alignItems: 'stretch', flexShrink: 0,
    padding: '0 12px 0 0', fontSize: 12,
  };

  // The lead cell is the rail toggle and the word mark; the crumb follows it.
  const leadStyle: CSSProperties = {
    display: 'flex', alignItems: 'center', gap: 8, paddingLeft: 8, flexShrink: 0,
  };

  // The label names the DISPLAYED target, which is the browse context unless a
  // settings-only target is pinned or a draft is on screen. A draft is named
  // by what it will create, in the create flow's blue (a lens draft in the
  // lens accent).
  const target: TargetLabel = given ?? (lensCtx
    ? { kind: 'lens', name: state.context.kind === 'lens' ? state.context.name : '' }
    : { kind: 'repo', name: state.repo });
  const label = {
    name: target.name,
    color: target.kind === 'lens' ? LENS_ACCENT : target.draft ? '#8ab6d6' : '#7c9',
    icon: target.kind === 'lens' ? <LayersIcon color="currentColor" size={13} /> : <BookIcon color="currentColor" size={13} />,
  };
  // Where the gear returns to from the server pages: the target under them,
  // named in its tooltip so the destination is one hover away.
  const gearTitle = held ? heldReason : serverOpen ? `Back to ${target.name}` : 'Server: fleet, logs and authorizations';
  const tabStyle = (on: boolean, disabled: boolean): CSSProperties => ({
    display: 'flex', alignItems: 'center', gap: 6,
    padding: '0 11px', fontSize: 12, border: 'none', borderRadius: 0, background: 'none',
    color: on ? '#eee' : '#888',
    boxShadow: on ? 'inset 0 -2px 0 #7c9' : 'none',
    cursor: disabled ? 'default' : 'pointer',
    opacity: disabled && !on ? 0.5 : 1,
    ...noDrag,
  });
  // Hover lifts an inactive, enabled tab; the active one is already #eee.
  const tabHover = (on: boolean, disabled: boolean) => disabled || on ? {} : {
    onMouseEnter: (e: ReactMouseEvent<HTMLButtonElement>) => { e.currentTarget.style.color = '#ccc'; },
    onMouseLeave: (e: ReactMouseEvent<HTMLButtonElement>) => { e.currentTarget.style.color = '#888'; },
  };

  return (
    <div style={outerStyle} onMouseDown={startWindowDrag}>
      {/* ── OS-chrome strip: native traffic lights float here ── */}
      {desktop && <div style={stripStyle} />}

      {/* ── Toolbar row ── */}
      <div data-testid="toknomitr-bar" style={rowStyle}>
        {/* ── Lead: rail toggle · knomit identity ── */}
        <div data-testid="bar-lead" style={leadStyle}>
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
            {othersFailing && (
              <span data-testid="rail-toggle-dot" aria-hidden="true" style={{
                position: 'absolute', top: 3, right: 3, width: 6, height: 6,
                borderRadius: '50%', background: '#f44336', boxShadow: '0 0 0 2px #111',
              }} />
            )}
          </button>
          <span data-testid="bar-brand" style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
            <svg aria-hidden="true" width="16" height="16" viewBox="0 0 80 80" style={{ display: 'block', flexShrink: 0 }}>
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

        {/* ── Breadcrumb: knomit / target ──
            Labels, not controls: nothing here opens anything, so they stay
            part of the drag region. The target is what the page below shows;
            over the server pages the word mark stands alone. */}
        {!serverOpen && (
          <>
            <span data-testid="crumb-sep" aria-hidden="true" style={{ display: 'flex', alignItems: 'center', margin: '0 8px 0 12px', color: '#555', fontSize: 13, flexShrink: 0 }}>/</span>
            <span data-testid="target-label" style={{
              display: 'flex', alignItems: 'center', gap: 6, minWidth: 0,
              fontSize: 13, fontWeight: 600, whiteSpace: 'nowrap',
              color: label.color,
            }}>
              {label.icon}
              <span style={{ overflow: 'hidden', textOverflow: 'ellipsis' }}>{label.name}</span>
              {/* A target with only Settings says why: archived, its store
                  missing, or still being created. */}
              {target.chip && (
                <span data-testid="target-label-chip" data-tone={target.chip.tone} style={{ ...chipBase, ...CHIP_TONE[target.chip.tone] }}>
                  {target.chip.text}
                </span>
              )}
            </span>
          </>
        )}
        {(server ? server.pages.length > 0 : facets.length > 0) && (
          <span data-testid="bar-divider" aria-hidden="true" style={{ alignSelf: 'center', width: 1, height: 18, margin: '0 14px', background: '#1e1e1e', flexShrink: 0 }} />
        )}

        {/* ── Facet tabs: the pages of the target named on the left ──
            Exactly facetsFor's list; a tab the target lacks is not drawn. The
            active tab's underline sits on the bar's bottom edge. */}
        {server && (
          <div role="tablist" aria-label="Server pages" data-testid="server-tabs" style={{ display: 'flex', alignItems: 'stretch' }}>
            {server.pages.map(p => {
              const on = p === server.page;
              return (
                <button
                  key={p}
                  type="button"
                  role="tab"
                  data-testid={`server-tab-${p}`}
                  data-nodrag
                  aria-selected={on}
                  onMouseDown={noMouseFocus}
                  onClick={() => server.onPage(p)}
                  style={tabStyle(on, false)}
                  {...tabHover(on, false)}
                >
                  <span>{SERVER_LABEL[p]}</span>
                  {p === 'auth' && server.authCount !== null && server.authCount > 0 && (
                    <span data-testid="server-tab-auth-badge" aria-label={`${server.authCount} waiting`} style={tabBadge}>{server.authCount}</span>
                  )}
                </button>
              );
            })}
          </div>
        )}
        {!serverOpen && facets.length > 0 && (
          <div role="tablist" aria-label="Facets" data-testid="facet-tabs" style={{ display: 'flex', alignItems: 'stretch' }}>
            {facets.map(f => {
              const on = f === facet;
              return (
                <button
                  key={f}
                  type="button"
                  role="tab"
                  data-testid={`facet-tab-${f}`}
                  data-nodrag
                  aria-selected={on}
                  disabled={locked}
                  title={locked ? 'A remote is being connected — leave it running' : undefined}
                  onMouseDown={noMouseFocus}
                  onClick={() => onFacet?.(f)}
                  style={tabStyle(on, locked)}
                  {...tabHover(on, locked)}
                >
                  <span>{FACET_LABEL[f]}</span>
                  {f === 'settings' && settingsDot && (
                    <span data-testid="facet-tab-settings-dot" aria-label="the remote is failing" style={{
                      width: 6, height: 6, borderRadius: '50%', background: '#f44336', flexShrink: 0,
                    }} />
                  )}
                </button>
              );
            })}
          </div>
        )}

        <div style={{ flex: 1 }} />

        {/* One control, one anchor: the gear opens the server pages, and the
            same button in the same pixel takes you back from them. While they
            show it is LIT, with the rail toggle's treatment; the lit state is
            the indicator, so the glyph never changes. No red dot: the gear is
            server-only, and a failing remote is a fact about a target — its
            dot is on the Settings tab. */}
        <button
          data-testid="server-toggle"
          data-nodrag
          onClick={onGear}
          onMouseDown={noMouseFocus}
          aria-pressed={serverOpen}
          disabled={held}
          title={gearTitle}
          aria-label={gearTitle}
          style={{
            width: 26, height: 26, flexShrink: 0, alignSelf: 'center',
            display: 'flex', alignItems: 'center', justifyContent: 'center',
            border: 'none', borderRadius: 4, padding: 0,
            color: held ? '#3d3d3d' : serverOpen ? '#dfe9e2' : '#666',
            background: serverOpen ? '#1a231d' : 'transparent',
            cursor: held ? 'default' : 'pointer',
            ...noDrag,
          }}
          onMouseEnter={e => { if (!held && !serverOpen) e.currentTarget.style.color = '#aaa'; }}
          onMouseLeave={e => { e.currentTarget.style.color = held ? '#3d3d3d' : serverOpen ? '#dfe9e2' : '#666'; }}
        >
          <GearIcon color="currentColor" size={15} />
        </button>
      </div>
    </div>
  );
});

const FACET_LABEL: Record<Facet, string> = { browse: 'Browse', activity: 'Activity', settings: 'Settings' };
const SERVER_LABEL: Record<ServerPage, string> = { fleet: 'Fleet', logs: 'Logs', auth: 'Authorizations' };

// The Authorizations tab's waiting count: the green badge it carried in Manage.
const tabBadge: CSSProperties = {
  fontSize: 10, padding: '0 5px', borderRadius: 8, lineHeight: 1.6,
  background: '#1d2a22', color: '#4ade80', fontVariantNumeric: 'tabular-nums',
};

// The spec's lens accent, for the label.
const LENS_ACCENT = '#b9a6f0';

// The label's state chip: the rail's chip geometry, in the spec's tones —
// archive warm, error red, creating blue.
const chipBase: CSSProperties = {
  display: 'inline-flex', alignItems: 'center', marginLeft: 4,
  fontSize: 10, lineHeight: 1.7, padding: '0 6px', borderRadius: 3, fontWeight: 400,
  fontFamily: 'var(--k-font-mono)', whiteSpace: 'nowrap', flexShrink: 0,
};
const CHIP_TONE: Record<NonNullable<TargetLabel['chip']>['tone'], CSSProperties> = {
  archived: { color: '#c4b29a', background: '#221d16', border: '1px solid #453a2a' },
  bad: { color: '#e8a0a0', background: '#1f1414', border: '1px solid #4a2626' },
  creating: { color: '#8ab6d6', background: '#131d26', border: '1px solid #244056' },
};

// PanelIcon is the rail toggle's glyph: a window with its left column ruled off.
const PanelIcon = () => (
  <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
    <rect x="3" y="4" width="18" height="16" rx="2" />
    <line x1="9" y1="4" x2="9" y2="20" />
  </svg>
);
