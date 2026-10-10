import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup, waitFor } from '@testing-library/react';
import { BranchPicker } from './BranchPicker';
import { init } from './state';
import type { AppState } from './state';

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>();
  return {
    ...actual,
    api: { ...actual.api, listExperiments: vi.fn().mockResolvedValue({ experiments: [] }) },
  };
});

afterEach(() => { cleanup(); vi.clearAllMocks(); });

// Moved from TopBar.test.tsx with the picker: the branch chip left the top bar
// for the Browse toolbar, because it acts on the data being read.
describe('BranchPicker — the branch', () => {
  const withBranch = (branch: string) => {
    const state: AppState = { ...init, repo: 'alpha', branch, context: { kind: 'repo', repo: 'alpha' } };
    render(<BranchPicker state={state} />);
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

  it('carries a caret, so the chip reads as something that opens', () => {
    expect(withBranch('main').querySelector('svg')).toBeTruthy();
  });

  it('renders nothing before the branch is known', () => {
    const state: AppState = { ...init, repo: 'alpha', branch: '', context: { kind: 'repo', repo: 'alpha' } };
    const { container } = render(<BranchPicker state={state} onEnterBranch={vi.fn()} />);
    expect(container.firstChild).toBeNull();
  });
});

describe('BranchPicker — the picker', () => {
  const state: AppState = { ...init, repo: 'alpha', branch: 'agent/box-1a2b3c4d', context: { kind: 'repo', repo: 'alpha' } };

  it('is a BUTTON only when it opens the picker; without onEnterBranch it is a label', () => {
    // Both directions: a chip that is always a button would be a control that
    // does nothing without a handler; one that never is has no picker.
    const { unmount } = render(<BranchPicker state={state} onEnterBranch={vi.fn()} />);
    const chip = screen.getByTestId('toknomitr-branch');
    expect(chip.tagName).toBe('BUTTON');
    expect(chip).toHaveAttribute('aria-haspopup', 'listbox');
    unmount();

    render(<BranchPicker state={state} />);
    expect(screen.getByTestId('toknomitr-branch').tagName).toBe('SPAN');
  });

  it('opens the menu with the agent branch, and fetches this repo\'s experiments on open', async () => {
    const { api } = await import('./api');
    render(<BranchPicker state={state} onEnterBranch={vi.fn()} />);
    expect(screen.queryByTestId('toknomitr-branch-menu')).toBeNull();
    expect(api.listExperiments).not.toHaveBeenCalled();

    fireEvent.click(screen.getByTestId('toknomitr-branch'));
    expect(screen.getByTestId('toknomitr-branch-menu')).toBeInTheDocument();
    expect(screen.getByTestId('toknomitr-branch-option-agent')).toHaveAttribute('aria-selected', 'true');
    await waitFor(() => expect(api.listExperiments).toHaveBeenCalledWith('alpha'));
  });

  it('picking a different branch enters it; the current one does not', async () => {
    const onEnterBranch = vi.fn();
    const inExperiment: AppState = {
      ...state, branch: 'exp/try',
      experiment: { name: 'try', parent: 'agent/box-1a2b3c4d' } as AppState['experiment'],
    };
    render(<BranchPicker state={inExperiment} onEnterBranch={onEnterBranch} />);
    fireEvent.click(screen.getByTestId('toknomitr-branch'));
    fireEvent.click(screen.getByTestId('toknomitr-branch-option-agent'));
    expect(onEnterBranch).toHaveBeenCalledTimes(1);
    expect(onEnterBranch).toHaveBeenCalledWith('agent/box-1a2b3c4d');
    expect(screen.queryByTestId('toknomitr-branch-menu')).toBeNull();
  });
});
