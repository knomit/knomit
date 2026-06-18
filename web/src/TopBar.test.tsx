import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { TopBar } from './TopBar';
import { init } from './state';
import type { AppState } from './state';
import type { RepoInfo } from './api';

const baseState: AppState = { ...init, repo: 'alpha', branch: 'agent/test' };

const repos: RepoInfo[] = [
  { name: 'alpha' } as RepoInfo,
  { name: 'beta' } as RepoInfo,
  { name: 'gamma' } as RepoInfo,
];

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
