import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, within } from '@testing-library/react';
import { TopBar } from './TopBar';
import { init } from './state';
import type { AppState } from './state';
import type { RepoInfo, Lens } from './api';

// The top bar holds controls: everything on it opens something. The commit is
// the one element you can never act on, so it moved to the footer — see
// StatusFooter.test.tsx, where it is now shown unconditionally rather than only
// when a fact happened to be open.
describe('TopBar — the commit is not here any more', () => {
  it('renders no commit chip when anchored', () => {
    const state: AppState = {
      ...init,
      repo: 'alpha',
      branch: 'agent/test',
      headCommit: 'head0001234',
      asOf: { mode: 'history', commit: 'sc123456abcd' },
    };
    render(<TopBar state={state} repos={[]} dispatch={vi.fn()} onManageRepos={vi.fn()} leftWidth={300} />);
    expect(screen.queryByTestId('toknomitr-commit')).toBeNull();
    expect(screen.getByTestId('toknomitr-bar').textContent).not.toContain('sc12345');
  });

  it('renders no commit chip when live', () => {
    const state: AppState = { ...init, repo: 'alpha', branch: 'agent/test', headCommit: 'head0001234' };
    render(<TopBar state={state} repos={[]} dispatch={vi.fn()} onManageRepos={vi.fn()} leftWidth={300} />);
    expect(screen.queryByTestId('toknomitr-commit')).toBeNull();
    expect(screen.getByTestId('toknomitr-bar').textContent).not.toContain('head000');
  });
});

describe('TopBar — the branch', () => {
  const withBranch = (branch: string) => {
    const state: AppState = { ...init, repo: 'alpha', branch, context: { kind: 'repo', repo: 'alpha' } };
    render(<TopBar state={state} repos={[]} dispatch={vi.fn()} onManageRepos={vi.fn()} leftWidth={300} />);
    return screen.getByTestId('toknomitr-branch');
  };

  it('shows the machine, not the whole agent branch', () => {
    // Exact, not substring: the whole point is that `agent/` and the key
    // fingerprint are gone, and both are substrings of the untrimmed name.
    expect(withBranch('agent/mindev.local-8ef0cd32').textContent).toBe('mindev.local');
  });

  it('keeps hostname hyphens, cutting only the fingerprint', () => {
    expect(withBranch('agent/build-box-01-a1b2c3d4').textContent).toBe('build-box-01');
  });

  it('keeps the full name reachable rather than merely hiding it', () => {
    expect(withBranch('agent/mindev.local-8ef0cd32').title).toContain('agent/mindev.local-8ef0cd32');
  });

  it('leaves an ordinary branch alone', () => {
    expect(withBranch('main').textContent).toBe('main');
  });

  it('leaves a slashed non-agent branch alone', () => {
    expect(withBranch('feat/facet-panel-density').textContent).toBe('feat/facet-panel-density');
  });

  it('carries a caret before the picker exists, so the row does not reflow later', () => {
    // Switching branches is coming. Adding the affordance with the layout means
    // the day it lands is a behaviour change, not a visual one.
    expect(withBranch('main').querySelector('svg')).toBeTruthy();
  });
});

describe('TopBar — the search slot', () => {
  it('renders whatever search the app hands it', () => {
    const state: AppState = { ...init, repo: 'alpha', branch: 'agent/test' };
    render(<TopBar state={state} repos={[]} dispatch={vi.fn()} onManageRepos={vi.fn()} leftWidth={300}
      search={<div data-testid="the-search">search</div>} />);
    expect(screen.getByTestId('the-search')).toBeTruthy();
  });

  it('gives it the remaining width, so it is the element that absorbs a squeeze', () => {
    const state: AppState = { ...init, repo: 'alpha', branch: 'agent/test' };
    render(<TopBar state={state} repos={[]} dispatch={vi.fn()} onManageRepos={vi.fn()} leftWidth={300}
      search={<div data-testid="the-search">search</div>} />);
    const slot = screen.getByTestId('toknomitr-search');
    expect(slot.style.flex).toBe('1 1 0%');
    expect(slot.style.minWidth).toBe('0px');
  });

  it('leaves no gap in the row when the app passes nothing', () => {
    // History mode has no filter input — the trail breadcrumb takes over below.
    const state: AppState = { ...init, repo: 'alpha', branch: 'agent/test' };
    render(<TopBar state={state} repos={[]} dispatch={vi.fn()} onManageRepos={vi.fn()} leftWidth={300} />);
    expect(screen.queryByTestId('toknomitr-search')).toBeNull();
  });
});

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
const researchLens: Lens = {
  name: 'research',
  write: { uid: 'uid-scratch', name: 'scratch' },
  reads: [{ uid: 'uid-papers', name: 'papers' }, { uid: 'uid-notes', name: 'notes' }],
};
const lenses: Lens[] = [engLens, researchLens];

// A lens-context state: browsing the "eng" lens (write mount core / agent/main).
const lensState: AppState = {
  ...init,
  repo: 'core',
  branch: 'agent/main',
  context: { kind: 'lens', name: 'eng' },
  lens: engLens,
};

describe('TopBar repo selector', () => {
  it('renders a button trigger (not a native select) when multiple repos exist', () => {
    render(<TopBar state={baseState} repos={repos} dispatch={vi.fn()} onManageRepos={() => {}} leftWidth={300} />);
    const trigger = screen.getByTestId('toknomitr-repo-select');
    expect(trigger.tagName.toLowerCase()).toBe('button');
    expect(trigger).toHaveTextContent('alpha');
  });

  it('opens a portal-based dropdown listing all repos when the trigger is clicked', () => {
    render(<TopBar state={baseState} repos={repos} dispatch={vi.fn()} onManageRepos={() => {}} leftWidth={300} />);
    expect(screen.queryByTestId('toknomitr-repo-menu')).toBeNull();

    fireEvent.click(screen.getByTestId('toknomitr-repo-select'));

    const menu = screen.getByTestId('toknomitr-repo-menu');
    expect(menu).toBeInTheDocument();
    // Menu lives in document.body (portal), not as a descendant of the trigger.
    const trigger = screen.getByTestId('toknomitr-repo-select');
    expect(trigger.contains(menu)).toBe(false);

    expect(screen.getByTestId('toknomitr-repo-option-alpha')).toBeInTheDocument();
    expect(screen.getByTestId('toknomitr-repo-option-beta')).toBeInTheDocument();
    expect(screen.getByTestId('toknomitr-repo-option-gamma')).toBeInTheDocument();
  });

  it('clicking an option dispatches SET_REPO and closes the dropdown', () => {
    const dispatch = vi.fn();
    render(<TopBar state={baseState} repos={repos} dispatch={dispatch} onManageRepos={() => {}} leftWidth={300} />);

    fireEvent.click(screen.getByTestId('toknomitr-repo-select'));
    fireEvent.click(screen.getByTestId('toknomitr-repo-option-beta'));

    expect(dispatch).toHaveBeenCalledWith({ type: 'SET_REPO', repo: 'beta' });
    expect(screen.queryByTestId('toknomitr-repo-menu')).toBeNull();
  });

  it('clicking the active repo does not dispatch (no-op) but still closes', () => {
    const dispatch = vi.fn();
    render(<TopBar state={baseState} repos={repos} dispatch={dispatch} onManageRepos={() => {}} leftWidth={300} />);

    fireEvent.click(screen.getByTestId('toknomitr-repo-select'));
    fireEvent.click(screen.getByTestId('toknomitr-repo-option-alpha'));

    expect(dispatch).not.toHaveBeenCalled();
    expect(screen.queryByTestId('toknomitr-repo-menu')).toBeNull();
  });

  // A registered repo whose store failed to open stays in the switcher — it
  // used to vanish from the API entirely, which is the failure this whole
  // surface exists to end — but it is not somewhere you can go.
  describe('a repo with no live store', () => {
    const withBroken: RepoInfo[] = [
      ...repos,
      { name: 'ghost', uid: 'uid-ghost', state: 'missing', detail: 'database file not found' },
    ];

    it('lists it, chipped with the reason and explained on hover', () => {
      render(<TopBar state={baseState} repos={withBroken} dispatch={vi.fn()} onManageRepos={() => {}} leftWidth={300} />);
      fireEvent.click(screen.getByTestId('toknomitr-repo-select'));

      const option = screen.getByTestId('toknomitr-repo-option-ghost');
      expect(option).toBeInTheDocument();
      expect(within(option).getByTestId('repo-state-missing')).toHaveTextContent('missing');
      // The chip is the class of failure; the server's sentence is the hover.
      expect(option).toHaveAttribute('title', 'database file not found');
      expect(option).toHaveAttribute('aria-disabled', 'true');
    });

    it('does not navigate when it is clicked', () => {
      const dispatch = vi.fn();
      render(<TopBar state={baseState} repos={withBroken} dispatch={dispatch} onManageRepos={() => {}} leftWidth={300} />);
      fireEvent.click(screen.getByTestId('toknomitr-repo-select'));
      fireEvent.click(screen.getByTestId('toknomitr-repo-option-ghost'));

      // No SET_REPO, and the menu stays open: nothing happened, so closing it
      // would read as though something had.
      expect(dispatch).not.toHaveBeenCalled();
      expect(screen.getByTestId('toknomitr-repo-menu')).toBeInTheDocument();
    });

    it('leaves readable repos alone', () => {
      const dispatch = vi.fn();
      render(<TopBar state={baseState} repos={withBroken} dispatch={dispatch} onManageRepos={() => {}} leftWidth={300} />);
      fireEvent.click(screen.getByTestId('toknomitr-repo-select'));
      fireEvent.click(screen.getByTestId('toknomitr-repo-option-beta'));

      expect(dispatch).toHaveBeenCalledWith({ type: 'SET_REPO', repo: 'beta' });
      expect(screen.queryByTestId('repo-state-missing')).toBeNull();
    });
  });

  it('with a single repo, renders the plain repo name (no dropdown)', () => {
    render(<TopBar state={baseState} repos={[repos[0]]} dispatch={vi.fn()} onManageRepos={() => {}} leftWidth={300} />);
    expect(screen.getByTestId('toknomitr-repo-name')).toHaveTextContent('alpha');
    expect(screen.queryByTestId('toknomitr-repo-select')).toBeNull();
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
    const { container } = render(
      <TopBar state={baseState} repos={repos} dispatch={vi.fn()} onManageRepos={() => {}} leftWidth={300} />,
    );
    // The branch label is inline text — the classic dead zone — yet must drag.
    fireEvent.mouseDown(screen.getByTestId('toknomitr-branch'));
    expect(post).toHaveBeenCalledWith('wails:drag');

    post.mockClear();
    // Also draggable from the bar's empty area (the row root).
    fireEvent.mouseDown(container.firstChild as Element);
    expect(post).toHaveBeenCalledWith('wails:drag');
  });

  it('does NOT start a drag from interactive controls (repo select, gear)', () => {
    render(<TopBar state={baseState} repos={repos} dispatch={vi.fn()} onManageRepos={() => {}} leftWidth={300} />);
    fireEvent.mouseDown(screen.getByTestId('toknomitr-repo-select'));
    fireEvent.mouseDown(screen.getByTestId('toknomitr-manage-btn'));
    expect(post).not.toHaveBeenCalled();
  });

  it('does nothing outside desktop mode', () => {
    delete w.__KNOMIT_DESKTOP__;
    render(<TopBar state={baseState} repos={repos} dispatch={vi.fn()} onManageRepos={() => {}} leftWidth={300} />);
    fireEvent.mouseDown(screen.getByTestId('toknomitr-branch'));
    expect(post).not.toHaveBeenCalled();
  });
});

describe('TopBar two-group context switcher', () => {
  it('opens a dropdown with Repositories and Lenses groups listing both', () => {
    render(<TopBar state={baseState} repos={repos} lenses={lenses} dispatch={vi.fn()} onManageRepos={() => {}} leftWidth={300} />);
    fireEvent.click(screen.getByTestId('toknomitr-repo-select'));

    // Group headers.
    expect(screen.getByTestId('toknomitr-group-repos')).toHaveTextContent('Repositories');
    expect(screen.getByTestId('toknomitr-group-lenses')).toHaveTextContent('Lenses');

    // Repo rows (unchanged testids) still present.
    expect(screen.getByTestId('toknomitr-repo-option-alpha')).toBeInTheDocument();
    expect(screen.getByTestId('toknomitr-repo-option-gamma')).toBeInTheDocument();

    // Lens rows with two-line label (name + `N mounts · → <write>`).
    expect(screen.getByTestId('toknomitr-lens-option-eng')).toBeInTheDocument();
    expect(screen.getByTestId('toknomitr-lens-option-research')).toBeInTheDocument();
    expect(screen.getByTestId('toknomitr-lens-option-eng')).toHaveTextContent('3 mounts · → core');
    expect(screen.getByTestId('toknomitr-lens-option-research')).toHaveTextContent('2 mounts · → scratch');
  });

  it('omits the Lenses group when there are no lenses', () => {
    render(<TopBar state={baseState} repos={repos} lenses={[]} dispatch={vi.fn()} onManageRepos={() => {}} leftWidth={300} />);
    fireEvent.click(screen.getByTestId('toknomitr-repo-select'));
    expect(screen.getByTestId('toknomitr-group-repos')).toBeInTheDocument();
    expect(screen.queryByTestId('toknomitr-group-lenses')).toBeNull();
  });

  it('offers the switcher with a single repo when lenses exist', () => {
    render(<TopBar state={baseState} repos={[repos[0]]} lenses={lenses} dispatch={vi.fn()} onManageRepos={() => {}} leftWidth={300} />);
    expect(screen.getByTestId('toknomitr-repo-select')).toBeInTheDocument();
    expect(screen.queryByTestId('toknomitr-repo-name')).toBeNull();
  });

  it('selecting a lens dispatches SET_CONTEXT {kind:lens,name} and closes', () => {
    const dispatch = vi.fn();
    render(<TopBar state={baseState} repos={repos} lenses={lenses} dispatch={dispatch} onManageRepos={() => {}} leftWidth={300} />);
    fireEvent.click(screen.getByTestId('toknomitr-repo-select'));
    fireEvent.click(screen.getByTestId('toknomitr-lens-option-eng'));

    expect(dispatch).toHaveBeenCalledWith({ type: 'SET_CONTEXT', context: { kind: 'lens', name: 'eng' } });
    expect(screen.queryByTestId('toknomitr-repo-menu')).toBeNull();
  });

  it('selecting a repo still dispatches SET_REPO (repo rows unchanged)', () => {
    const dispatch = vi.fn();
    render(<TopBar state={baseState} repos={repos} lenses={lenses} dispatch={dispatch} onManageRepos={() => {}} leftWidth={300} />);
    fireEvent.click(screen.getByTestId('toknomitr-repo-select'));
    fireEvent.click(screen.getByTestId('toknomitr-repo-option-beta'));
    expect(dispatch).toHaveBeenCalledWith({ type: 'SET_REPO', repo: 'beta' });
  });
});

describe('TopBar lens-context chips', () => {
  it('shows a lens chip (name) instead of the repo book chip', () => {
    render(<TopBar state={lensState} repos={repos} lenses={lenses} dispatch={vi.fn()} onManageRepos={() => {}} leftWidth={300} />);
    const trigger = screen.getByTestId('toknomitr-lens-select');
    expect(trigger.tagName.toLowerCase()).toBe('button');
    expect(trigger).toHaveTextContent('eng');
    // The repo book chip / branch chip are not rendered in a lens context.
    expect(screen.queryByTestId('toknomitr-repo-select')).toBeNull();
    expect(screen.queryByTestId('toknomitr-branch')).toBeNull();
  });

  it('replaces the branch chip with the mounts PICKER, not a mounts readout', () => {
    // It used to render lens.reads.length — the total, always, whatever was
    // selected — while the real control sat in the left panel. The prominent
    // one was the untrue one, so the readout became the control and the left
    // panel's SOURCES block went away.
    render(<TopBar state={lensState} repos={repos} lenses={lenses} dispatch={vi.fn()} onManageRepos={() => {}} leftWidth={300} />);
    expect(screen.getByTestId('mounts-picker')).toBeTruthy();
    expect(screen.getByTestId('mounts-label')).toHaveTextContent('3');
  });

  it('the mounts picker reflects the SELECTION, which the old chip could not', () => {
    const narrowed: AppState = { ...lensState, lensSources: ['core'] };
    render(<TopBar state={narrowed} repos={repos} lenses={lenses} dispatch={vi.fn()} onManageRepos={() => {}} leftWidth={300} />);
    expect(screen.getByTestId('mounts-label')).toHaveTextContent('1/3');
  });

  it('does not name the write target — that is a readout, and it is in the footer', () => {
    // You cannot change it here: the lens sets it. See StatusFooter.test.tsx.
    render(<TopBar state={lensState} repos={repos} lenses={lenses} dispatch={vi.fn()} onManageRepos={() => {}} leftWidth={300} />);
    expect(screen.queryByTestId('toknomitr-writes')).toBeNull();
  });

  it('clicking the lens chip opens the two-group switcher with eng active', () => {
    render(<TopBar state={lensState} repos={repos} lenses={lenses} dispatch={vi.fn()} onManageRepos={() => {}} leftWidth={300} />);
    fireEvent.click(screen.getByTestId('toknomitr-lens-select'));
    const active = screen.getByTestId('toknomitr-lens-option-eng');
    expect(active).toHaveAttribute('aria-selected', 'true');
  });

  it('shows no commit in any lens state — anchored, with a fact open, either way', () => {
    // The old rule was "only when a fact is open AND the view is anchored",
    // which meant no commit on screen at all during ordinary reading. The
    // footer now shows one unconditionally.
    for (const state of [
      { ...lensState, factPath: 'kb/a/b.md', asOf: { mode: 'live' } } as AppState,
      { ...lensState, factPath: 'kb/a/b.md', asOf: { mode: 'history', commit: 'abc1234def5' } } as AppState,
      { ...lensState, factPath: null, asOf: { mode: 'history', commit: 'abc1234def5' } } as AppState,
    ]) {
      const { unmount } = render(
        <TopBar state={state} repos={repos} lenses={lenses} dispatch={vi.fn()} onManageRepos={() => {}} leftWidth={300} />);
      expect(screen.queryByTestId('toknomitr-commit')).toBeNull();
      unmount();
    }
  });
});
