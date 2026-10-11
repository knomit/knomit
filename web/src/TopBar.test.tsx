import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, within } from '@testing-library/react';
import { TopBar } from './TopBar';
import { init } from './state';
import type { AppState } from './state';
import type { RepoInfo, Lens } from './api';

// A realistic repo context: SET_CONTEXT sets repo and context.repo atomically,
// so the fixture pins both (production state can never diverge here).
const baseState: AppState = { ...init, repo: 'alpha', branch: 'agent/test', context: { kind: 'repo', repo: 'alpha' } };

const repos: RepoInfo[] = [
  { name: 'alpha' } as RepoInfo,
  { name: 'beta' } as RepoInfo,
  { name: 'gamma' } as RepoInfo,
];

const engLens: Lens = {
  name: 'eng',
  write: { uid: 'uid-core', name: 'core' },
  reads: [{ uid: 'uid-core', name: 'core' }, { uid: 'uid-docs', name: 'docs' }, { uid: 'uid-infra', name: 'infra' }],
};

// A lens-context state: browsing the "eng" lens (write mount core / agent/main).
const lensState: AppState = {
  ...init,
  repo: 'core',
  branch: 'agent/main',
  context: { kind: 'lens', name: 'eng' },
  lens: engLens,
};

type BarProps = React.ComponentProps<typeof TopBar>;
const bar = (over: Partial<BarProps> = {}) => (
  <TopBar state={baseState} repos={repos} railOpen={false} onToggleRail={vi.fn()} onGear={vi.fn()} {...over} />
);

// The top bar holds controls: everything on it opens something. The commit is
// the one element you can never act on, so it moved to the footer — see
// StatusFooter.test.tsx, where it is now shown unconditionally rather than only
// when a fact happened to be open.
describe('TopBar — the commit is not here any more', () => {
  it('renders no commit chip when anchored', () => {
    const state: AppState = {
      ...baseState,
      headCommit: 'head0001234',
      asOf: { mode: 'history', commit: 'sc123456abcd' },
    };
    render(bar({ state }));
    expect(screen.queryByTestId('toknomitr-commit')).toBeNull();
    expect(screen.getByTestId('toknomitr-bar').textContent).not.toContain('sc12345');
  });

  it('renders no commit chip when live', () => {
    const state: AppState = { ...baseState, headCommit: 'head0001234' };
    render(bar({ state }));
    expect(screen.queryByTestId('toknomitr-commit')).toBeNull();
    expect(screen.getByTestId('toknomitr-bar').textContent).not.toContain('head000');
  });

  it('shows no commit in any lens state — anchored, with a fact open, either way', () => {
    for (const state of [
      { ...lensState, factPath: 'kb/a/b.md', asOf: { mode: 'live' } } as AppState,
      { ...lensState, factPath: 'kb/a/b.md', asOf: { mode: 'history', commit: 'abc1234def5' } } as AppState,
      { ...lensState, factPath: null, asOf: { mode: 'history', commit: 'abc1234def5' } } as AppState,
    ]) {
      const { unmount } = render(bar({ state }));
      expect(screen.queryByTestId('toknomitr-commit')).toBeNull();
      unmount();
    }
  });
});

// The target label is a static label: picking a repo or lens moved to the
// rail, so the dropdown that used to hang off it is gone, not hidden.
describe('TopBar — the target label', () => {
  it('names the repo in green, as a label and not a menu trigger', () => {
    render(bar());
    const label = screen.getByTestId('target-label');
    expect(label.textContent).toBe('alpha');
    expect(label).toHaveStyle({ color: '#7c9' });
    expect(label.tagName).not.toBe('BUTTON');
    expect(label.querySelector('button')).toBeNull();
    // No popup anywhere on the bar: the switcher menu is deleted.
    expect(screen.getByTestId('toknomitr-bar').querySelector('[aria-haspopup]')).toBeNull();
    expect(screen.queryByTestId('toknomitr-repo-select')).toBeNull();
  });

  it('names the lens in the lens accent', () => {
    render(bar({ state: lensState }));
    const label = screen.getByTestId('target-label');
    expect(label.textContent).toBe('eng');
    expect(label).toHaveStyle({ color: '#b9a6f0' });
    expect(screen.queryByTestId('toknomitr-lens-select')).toBeNull();
  });

  it('is ruled off from the logo: 1px #1e1e1e left border, padding 0 14px, 13px/500', () => {
    render(bar());
    expect(screen.getByTestId('target-label')).toHaveStyle({
      borderLeft: '1px solid #1e1e1e',
      padding: '0px 14px',
      fontSize: '13px',
      fontWeight: '500',
    });
  });

  it('holds neither the branch picker nor the filter: both moved to the Browse toolbar', () => {
    render(bar());
    // The label is the positive control: the zone they used to sit in renders.
    expect(screen.getByTestId('target-label')).toBeInTheDocument();
    expect(screen.queryByTestId('toknomitr-branch')).toBeNull();
    expect(screen.queryByTestId('mounts-picker')).toBeNull();
    expect(screen.queryByTestId('toknomitr-search')).toBeNull();
  });

  it('names the server, by its host, over the server pages (control: the target outside them)', () => {
    const { unmount } = render(bar());
    expect(screen.getByTestId('target-label')).toHaveTextContent('alpha');
    expect(screen.queryByTestId('server-host')).toBeNull();
    unmount();
    render(bar({ server: { page: 'fleet', pages: ['fleet', 'logs', 'auth'], onPage: vi.fn(), authCount: null } }));
    const label = screen.getByTestId('target-label');
    expect(label).toHaveTextContent(/^Server/);
    expect(label).not.toHaveTextContent('alpha');
    // The jsdom page's own host: the API base is relative here.
    expect(screen.getByTestId('server-host')).toHaveTextContent(/^localhost:3000$/);
    expect(screen.getByTestId('server-host')).toHaveStyle({ fontSize: '11px', color: '#666' });
  });
});

describe('TopBar — the rail toggle', () => {
  it('reports the rail state through aria-pressed', () => {
    const { unmount } = render(bar({ railOpen: false }));
    expect(screen.getByTestId('rail-toggle')).toHaveAttribute('aria-pressed', 'false');
    unmount();
    render(bar({ railOpen: true }));
    expect(screen.getByTestId('rail-toggle')).toHaveAttribute('aria-pressed', 'true');
  });

  it('is lit while the rail is open, and plain while it is not', () => {
    const { unmount } = render(bar({ railOpen: true }));
    expect(screen.getByTestId('rail-toggle')).toHaveStyle({ color: '#dfe9e2', background: '#1a231d' });
    unmount();
    render(bar({ railOpen: false }));
    expect(screen.getByTestId('rail-toggle')).not.toHaveStyle({ background: '#1a231d' });
  });

  it('is a 26x26 control', () => {
    render(bar());
    expect(screen.getByTestId('rail-toggle')).toHaveStyle({ width: '26px', height: '26px' });
  });

  it('clicking it asks the app to toggle the rail, once', () => {
    const onToggleRail = vi.fn();
    render(bar({ onToggleRail }));
    fireEvent.click(screen.getByTestId('rail-toggle'));
    expect(onToggleRail).toHaveBeenCalledTimes(1);
  });

  it('stays over the server pages: the rail sits beside them', () => {
    render(bar({ server: { page: 'fleet', pages: ['fleet', 'logs', 'auth'], onPage: vi.fn(), authCount: null } }));
    expect(screen.getByTestId('rail-toggle')).toBeInTheDocument();
    expect(screen.getByTestId('server-toggle')).toBeInTheDocument();
  });
});

// The toggle's dot is the cross-repo alert: SOME OTHER repo's remote is
// failing, so opening the rail is worth it. The current repo's failure has the
// banner (and, until the Settings tab exists, the gear's dot), so it must not
// light the toggle too.
describe("TopBar — the rail toggle's health dot", () => {
  const failing = (name: string): RepoInfo => ({ name, uid: `u-${name}`, remote: { sync: 'ok', push: 'error' } });
  const healthy = (name: string): RepoInfo => ({ name, uid: `u-${name}`, remote: { sync: 'ok', push: 'ok' } });

  it('lights when a repo OTHER than the current one is failing', () => {
    render(bar({ repos: [healthy('alpha'), failing('beta')] }));
    expect(screen.getByTestId('rail-toggle-dot')).toBeInTheDocument();
  });

  it('stays dark when only the current repo is failing', () => {
    render(bar({ repos: [failing('alpha'), healthy('beta')] }));
    // Control: the toggle it would sit on is rendered.
    expect(screen.getByTestId('rail-toggle')).toBeInTheDocument();
    expect(screen.queryByTestId('rail-toggle-dot')).toBeNull();
  });

  it('reads a failing sync as well as a failing push', () => {
    render(bar({ repos: [healthy('alpha'), { name: 'beta', uid: 'u-beta', remote: { sync: 'error', push: '' } }] }));
    expect(screen.getByTestId('rail-toggle-dot')).toBeInTheDocument();
  });

  it('is hidden on a read-only server', () => {
    render(bar({ state: { ...baseState, serverReadOnly: true }, repos: [healthy('alpha'), failing('beta')] }));
    expect(screen.getByTestId('rail-toggle')).toBeInTheDocument();
    expect(screen.queryByTestId('rail-toggle-dot')).toBeNull();
  });

  it('sits top-right on the toggle: 6px red with a 2px #111 ring', () => {
    render(bar({ repos: [healthy('alpha'), failing('beta')] }));
    const dot = screen.getByTestId('rail-toggle-dot');
    expect(screen.getByTestId('rail-toggle').contains(dot)).toBe(true);
    expect(dot).toHaveStyle({
      position: 'absolute', top: '3px', right: '3px', width: '6px', height: '6px',
      background: '#f44336', boxShadow: '0 0 0 2px #111',
    });
  });
});

describe('TopBar — the gear', () => {
  // The dot moved to the Settings tab: the gear opens server pages, and a
  // failing remote is a fact about the target. Paired with the control that
  // the Settings tab DOES carry it in the same render.
  it('has no red dot while the current repo fails; the Settings tab has it', () => {
    render(bar({
      state: { ...baseState, remoteSyncError: 'unreachable' },
      facets: ['browse', 'activity', 'settings'], facet: 'browse', settingsDot: true,
    }));
    expect(screen.getByTestId('server-toggle').querySelector('span')).toBeNull();
    expect(screen.getByTestId('server-toggle').style.color).toBe('rgb(102, 102, 102)');
    expect(screen.getByTestId('facet-tab-settings-dot')).toBeInTheDocument();
  });

  it('asks the app to open or close the server pages, once a click', () => {
    const onGear = vi.fn();
    render(bar({ onGear }));
    fireEvent.click(screen.getByTestId('server-toggle'));
    expect(onGear).toHaveBeenCalledTimes(1);
  });

  // The lit state IS the indicator: same glyph both ways, no exit icon.
  it('is lit over the server pages with the rail toggle\'s treatment, and plain elsewhere', () => {
    const { unmount } = render(bar());
    const plain = screen.getByTestId('server-toggle');
    expect(plain).toHaveAttribute('aria-pressed', 'false');
    expect(plain).not.toHaveStyle({ background: '#1a231d' });
    expect(plain.getAttribute('title')).toBe('Server: fleet, logs and authorizations');
    const glyph = plain.innerHTML;
    unmount();
    render(bar({ server: { page: 'fleet', pages: ['fleet', 'logs', 'auth'], onPage: vi.fn(), authCount: null } }));
    const lit = screen.getByTestId('server-toggle');
    expect(lit).toHaveAttribute('aria-pressed', 'true');
    expect(lit).toHaveStyle({ color: '#dfe9e2', background: '#1a231d', width: '26px', height: '26px' });
    expect(lit.innerHTML).toBe(glyph);
    // The tooltip names where it returns to.
    expect(lit.getAttribute('title')).toBe('Back to alpha');
  });

  it('refuses while a connect commit is writing, and says why', () => {
    render(bar({ locked: true, facets: ['browse', 'activity', 'settings'], facet: 'settings' }));
    const gear = screen.getByTestId('server-toggle');
    expect(gear).toBeDisabled();
    expect(gear.getAttribute('title')).toBe('A remote is being connected — leave it running');
  });
});

describe('TopBar — facet tabs', () => {
  it('draws exactly the facets it is given, in order, after the label', () => {
    render(bar({ facets: ['browse', 'activity', 'settings'], facet: 'browse' }));
    expect(screen.getAllByRole('tab').map(t => t.textContent)).toEqual(['Browse', 'Activity', 'Settings']);
    const label = screen.getByTestId('target-label');
    expect(label.compareDocumentPosition(screen.getByTestId('facet-tabs')) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('omits a facet the target lacks rather than disabling it', () => {
    render(bar({ facets: ['activity', 'settings'], facet: 'settings' }));
    expect(screen.getAllByRole('tab').map(t => t.textContent)).toEqual(['Activity', 'Settings']);
    expect(screen.queryByTestId('facet-tab-browse')).toBeNull();
  });

  it('marks the active tab #eee with the green underline, the others #7a7a7a', () => {
    render(bar({ facets: ['browse', 'activity', 'settings'], facet: 'activity' }));
    const on = screen.getByTestId('facet-tab-activity');
    expect(on).toHaveAttribute('aria-selected', 'true');
    expect(on.style.color).toBe('rgb(238, 238, 238)');
    expect(on.style.boxShadow).toBe('inset 0 -2px 0 #7c9');
    const off = screen.getByTestId('facet-tab-browse');
    expect(off).toHaveAttribute('aria-selected', 'false');
    expect(off.style.color).toBe('rgb(122, 122, 122)');
    expect(off.style.fontSize).toBe('12px');
    expect(off.style.padding).toBe('0px 11px');
  });

  it('reports a click with the facet', () => {
    const onFacet = vi.fn();
    render(bar({ facets: ['browse', 'activity', 'settings'], facet: 'browse', onFacet }));
    fireEvent.click(screen.getByTestId('facet-tab-settings'));
    expect(onFacet).toHaveBeenCalledWith('settings');
  });

  it('puts the dot on Settings only when asked', () => {
    const { unmount } = render(bar({ facets: ['browse', 'activity', 'settings'], facet: 'browse', settingsDot: true }));
    const dot = screen.getByTestId('facet-tab-settings-dot');
    expect(screen.getByTestId('facet-tab-settings').contains(dot)).toBe(true);
    expect(dot.style.width).toBe('6px');
    unmount();
    render(bar({ facets: ['browse', 'activity', 'settings'], facet: 'browse', settingsDot: false }));
    expect(screen.getByTestId('facet-tab-settings')).toBeInTheDocument();
    expect(screen.queryByTestId('facet-tab-settings-dot')).toBeNull();
  });

  it('disables every tab while locked', () => {
    render(bar({ facets: ['browse', 'activity', 'settings'], facet: 'settings', locked: true }));
    for (const t of screen.getAllByRole('tab')) expect(t).toBeDisabled();
  });

  it('draws the server tabs, not the facets, over the server pages (control: facets outside them)', () => {
    const { unmount } = render(bar({ facets: ['browse', 'activity', 'settings'], facet: 'browse' }));
    expect(screen.getAllByRole('tab').map(t => t.textContent)).toEqual(['Browse', 'Activity', 'Settings']);
    unmount();
    render(bar({ facets: ['browse', 'activity', 'settings'], facet: 'browse', server: { page: 'fleet', pages: ['fleet', 'logs', 'auth'], onPage: vi.fn(), authCount: null } }));
    expect(screen.queryByTestId('facet-tabs')).toBeNull();
    expect(screen.getAllByRole('tab').map(t => t.textContent)).toEqual(['Fleet', 'Logs', 'Authorizations']);
    expect(screen.getByTestId('server-tab-fleet')).toHaveAttribute('aria-selected', 'true');
    expect(screen.getByTestId('server-tab-fleet')).toHaveStyle({ boxShadow: 'inset 0 -2px 0 #7c9', color: '#eee' });
  });

  it('draws exactly the server pages it is given', () => {
    render(bar({ server: { page: 'fleet', pages: ['fleet'], onPage: vi.fn(), authCount: 3 } }));
    expect(screen.getAllByRole('tab').map(t => t.textContent)).toEqual(['Fleet']);
    expect(screen.queryByTestId('server-tab-auth-badge')).toBeNull();
  });

  it('a server tab click asks for that page', () => {
    const onPage = vi.fn();
    render(bar({ server: { page: 'fleet', pages: ['fleet', 'logs'], onPage, authCount: null } }));
    fireEvent.click(screen.getByTestId('server-tab-logs'));
    expect(onPage).toHaveBeenCalledWith('logs');
  });

  it('badges Authorizations with the waiting count above zero only', () => {
    const { unmount } = render(bar({ server: { page: 'fleet', pages: ['fleet', 'logs', 'auth'], onPage: vi.fn(), authCount: 2 } }));
    expect(screen.getByTestId('server-tab-auth-badge')).toHaveTextContent(/^2$/);
    unmount();
    render(bar({ server: { page: 'fleet', pages: ['fleet', 'logs', 'auth'], onPage: vi.fn(), authCount: 0 } }));
    expect(screen.getByTestId('server-tab-auth')).toBeInTheDocument();
    expect(screen.queryByTestId('server-tab-auth-badge')).toBeNull();
  });
});

// Regression: Wails' CSS --wails-draggable has dead zones over inline text/SVG
// (it gates on clientWidth, 0 for inline elements), so the title bar couldn't
// grab the window. The bar now triggers the native drag explicitly on mousedown
// over non-interactive regions.
describe('TopBar desktop window drag', () => {
  type WebkitWindow = Window & {
    __KNOMIT_DESKTOP__?: boolean;
    webkit?: { messageHandlers?: { external?: { postMessage: (m: string) => void } } };
  };
  const w = window as WebkitWindow;
  let post: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    post = vi.fn();
    w.__KNOMIT_DESKTOP__ = true;
    w.webkit = { messageHandlers: { external: { postMessage: post as unknown as (m: string) => void } } };
  });
  afterEach(() => {
    delete w.__KNOMIT_DESKTOP__;
    delete w.webkit;
  });

  it('starts a native drag on mousedown over a non-interactive bar region', () => {
    const { container } = render(bar());
    // The target label is inline text — the classic dead zone — yet must drag.
    fireEvent.mouseDown(within(screen.getByTestId('target-label')).getByText('alpha'));
    expect(post).toHaveBeenCalledWith('wails:drag');

    post.mockClear();
    // Also draggable from the bar's empty area (the row root).
    fireEvent.mouseDown(container.firstChild as Element);
    expect(post).toHaveBeenCalledWith('wails:drag');
  });

  it('does NOT start a drag from interactive controls (rail toggle, gear)', () => {
    render(bar());
    fireEvent.mouseDown(screen.getByTestId('rail-toggle'));
    fireEvent.mouseDown(screen.getByTestId('server-toggle'));
    expect(post).not.toHaveBeenCalled();
  });

  it('does nothing outside desktop mode', () => {
    delete w.__KNOMIT_DESKTOP__;
    render(bar());
    fireEvent.mouseDown(screen.getByTestId('target-label'));
    expect(post).not.toHaveBeenCalled();
  });
});

// THE CREATE COUNT IS NOT IN THE TOP BAR ANY MORE.
//
// It was a chip showing a bare number next to the settings and exit icons, and
// it read as an unexplained badge rather than as information: "there is this
// weird '1' icon at the top… that is not needed, or at least not like that".
// The count moved to the status footer, in words. The top bar is controls
// only — everything in it opens something — and a readout had no business
// there.
describe('TopBar — no create indicator', () => {
  it('renders no create count, however many creates are running', () => {
    render(bar({ state: { ...baseState, headCommit: 'head0001234' } }));
    expect(screen.queryByTestId('create-indicator')).toBeNull();
    expect(screen.getByTestId('toknomitr-bar').textContent).not.toMatch(/create/i);
  });
});
