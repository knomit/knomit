import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { TopBar } from './TopBar';
import { init } from './state';
import type { AppState } from './state';
import type { RepoInfo, Lens } from './api';

describe('TopBar commit chip', () => {
  it('shows a borderless amber commit chip (clock + hash, no "as of") when history', () => {
    const state: AppState = {
      ...init,
      repo: 'alpha',
      branch: 'agent/test',
      headCommit: 'head0001234',
      asOf: { mode: 'history', commit: 'sc123456abcd' },
    };
    render(<TopBar state={state} repos={[]} dispatch={vi.fn()} onManageRepos={vi.fn()} leftWidth={300} />);
    expect(screen.getByTestId('toknomitr-commit')).toHaveTextContent('sc12345');
    expect(screen.queryByText(/as of/)).toBeNull();
  });

  it('does NOT show the as-of chip in diff mode', () => {
    const state: AppState = {
      ...init,
      repo: 'alpha',
      branch: 'agent/test',
      headCommit: 'head0001234',
      asOf: { mode: 'diff', from: 'aaa1111bbbb', to: 'bbb2222cccc' },
    };
    render(<TopBar state={state} repos={[]} dispatch={vi.fn()} onManageRepos={vi.fn()} leftWidth={300} />);
    expect(screen.queryByText(/as of/)).toBeNull();
  });
});

const baseState: AppState = { ...init, repo: 'alpha', branch: 'agent/test' };

const repos: RepoInfo[] = [
  { name: 'alpha' } as RepoInfo,
  { name: 'beta' } as RepoInfo,
  { name: 'gamma' } as RepoInfo,
];

const engLens: Lens = {
  name: 'eng',
  write: 'core',
  reads: [{ repo: 'core' }, { repo: 'docs' }, { repo: 'infra' }],
};
const researchLens: Lens = {
  name: 'research',
  write: 'scratch',
  reads: [{ repo: 'papers' }, { repo: 'notes' }],
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

  it('replaces the branch chip with an N-mounts summary', () => {
    render(<TopBar state={lensState} repos={repos} lenses={lenses} dispatch={vi.fn()} onManageRepos={() => {}} leftWidth={300} />);
    expect(screen.getByTestId('toknomitr-mounts')).toHaveTextContent('3 mounts');
  });

  it('shows a writes-→ pill with the lens write target', () => {
    render(<TopBar state={lensState} repos={repos} lenses={lenses} dispatch={vi.fn()} onManageRepos={() => {}} leftWidth={300} />);
    expect(screen.getByTestId('toknomitr-writes')).toHaveTextContent('writes → core');
  });

  it('clicking the lens chip opens the two-group switcher with eng active', () => {
    render(<TopBar state={lensState} repos={repos} lenses={lenses} dispatch={vi.fn()} onManageRepos={() => {}} leftWidth={300} />);
    fireEvent.click(screen.getByTestId('toknomitr-lens-select'));
    const active = screen.getByTestId('toknomitr-lens-option-eng');
    expect(active).toHaveAttribute('aria-selected', 'true');
  });

  it('hides the commit chip when live even with a fact open', () => {
    const state: AppState = { ...lensState, factPath: 'kb/a/b.md', asOf: { mode: 'live' } };
    render(<TopBar state={state} repos={repos} lenses={lenses} dispatch={vi.fn()} onManageRepos={() => {}} leftWidth={300} />);
    expect(screen.queryByTestId('toknomitr-commit')).toBeNull();
  });

  it('shows the open fact anchor commit when a fact is open in history', () => {
    const state: AppState = { ...lensState, factPath: 'kb/a/b.md', asOf: { mode: 'history', commit: 'abc1234def5' } };
    render(<TopBar state={state} repos={repos} lenses={lenses} dispatch={vi.fn()} onManageRepos={() => {}} leftWidth={300} />);
    expect(screen.getByTestId('toknomitr-commit')).toHaveTextContent('abc1234');
  });

  it('hides the commit chip in a lens context when no fact is open', () => {
    const state: AppState = { ...lensState, factPath: null, asOf: { mode: 'history', commit: 'abc1234def5' } };
    render(<TopBar state={state} repos={repos} lenses={lenses} dispatch={vi.fn()} onManageRepos={() => {}} leftWidth={300} />);
    expect(screen.queryByTestId('toknomitr-commit')).toBeNull();
  });
});
