import { describe, it, expect, vi } from 'vitest';
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
    render(<TopBar state={baseState} repos={repos} dispatch={vi.fn()} onSettingsClick={() => {}} />);
    const trigger = screen.getByTestId('toknomitr-repo-select');
    expect(trigger.tagName.toLowerCase()).toBe('button');
    expect(trigger).toHaveTextContent('alpha');
  });

  it('opens a portal-based dropdown listing all repos when the trigger is clicked', () => {
    render(<TopBar state={baseState} repos={repos} dispatch={vi.fn()} onSettingsClick={() => {}} />);
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
    render(<TopBar state={baseState} repos={repos} dispatch={dispatch} onSettingsClick={() => {}} />);

    fireEvent.click(screen.getByTestId('toknomitr-repo-select'));
    fireEvent.click(screen.getByTestId('toknomitr-repo-option-beta'));

    expect(dispatch).toHaveBeenCalledWith({ type: 'SET_REPO', repo: 'beta' });
    expect(screen.queryByTestId('toknomitr-repo-menu')).toBeNull();
  });

  it('clicking the active repo does not dispatch (no-op) but still closes', () => {
    const dispatch = vi.fn();
    render(<TopBar state={baseState} repos={repos} dispatch={dispatch} onSettingsClick={() => {}} />);

    fireEvent.click(screen.getByTestId('toknomitr-repo-select'));
    fireEvent.click(screen.getByTestId('toknomitr-repo-option-alpha'));

    expect(dispatch).not.toHaveBeenCalled();
    expect(screen.queryByTestId('toknomitr-repo-menu')).toBeNull();
  });

  it('with a single repo, renders the plain repo name (no dropdown)', () => {
    render(<TopBar state={baseState} repos={[repos[0]]} dispatch={vi.fn()} onSettingsClick={() => {}} />);
    expect(screen.getByTestId('toknomitr-repo-name')).toHaveTextContent('alpha');
    expect(screen.queryByTestId('toknomitr-repo-select')).toBeNull();
  });
});
