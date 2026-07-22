import { describe, it, expect, vi, beforeEach } from 'vitest';
import { useState } from 'react';
import { render, screen, fireEvent, waitFor, act } from '@testing-library/react';
import { init } from './state';
import type { AppState } from './state';
import type { RepoInfo } from './api';
import { TopBar } from './TopBar';
import { FilterBar } from './FilterBar';
import { Console } from './Console';
import { RightPanel } from './RightPanel';

// Render-count evidence for the four panel memos that had NONE. Stripping
// `memo` from Console, FilterBar, TopBar and RightPanel simultaneously left the
// whole suite green — the memos were load-bearing by assertion only.
//
// HOW THE COUNT IS TAKEN. Each panel calls exactly one selector unconditionally
// as the first thing it does with its props, so a counting pass-through on that
// selector is a direct read of "did this component's body run":
//   TopBar     → isLensContext   FilterBar → isLensContext
//   RightPanel → currentPath     Console   → useConsoleState
// Counting through a mock of the panel's OWN module would not work: the mock
// replaces the memoized export with a plain function, so it re-renders whenever
// the parent does and measures the parent, not the memo. (That is exactly the
// mislabelled instrument in App.resilience.test.tsx — see the note there.)
//
// THE HARNESS. `Rerender` owns a tick of state and re-renders on demand. It
// takes a RENDER FUNCTION, not a child element, and that detail is the whole
// test: passing `children` would hand React the same element object on every
// pass, and React bails out on element identity before it ever consults the
// memo — the harness would then pass with every memo in this file deleted.
// Calling `renderPanel()` mints a fresh element each pass while every prop
// VALUE is held stable, the way App holds them (dispatch is a stable
// useCallback, the repo array comes from state, the callbacks are
// useCallback'd). A memoized panel absorbs that; an unmemoized one runs its
// body again.

const renders = vi.hoisted(() => ({ isLensContext: 0, currentPath: 0, consoleState: 0 }));

vi.mock('./state', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./state')>();
  return {
    ...actual,
    isLensContext: (s: AppState) => { renders.isLensContext += 1; return actual.isLensContext(s); },
    currentPath: (s: AppState) => { renders.currentPath += 1; return actual.currentPath(s); },
  };
});

vi.mock('./consoleStore', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./consoleStore')>();
  return {
    ...actual,
    useConsoleState: () => { renders.consoleState += 1; return actual.useConsoleState(); },
  };
});

vi.mock('./api', () => ({
  api: {
    fact: vi.fn().mockResolvedValue(null),
    stats: vi.fn().mockResolvedValue(null),
    activity: vi.fn().mockResolvedValue(null),
    explain: vi.fn().mockResolvedValue({ incoming: [], outgoing: [] }),
    factCommits: vi.fn().mockResolvedValue({ entries: [] }),
    factDiff: vi.fn().mockResolvedValue({ from: null, to: null }),
    commitDetail: vi.fn().mockResolvedValue(null),
    getAgentBranch: vi.fn().mockResolvedValue('agent/test'),
    completions: vi.fn().mockResolvedValue([]),
    lensStats: vi.fn().mockResolvedValue(null),
  },
  parseFilterQuery: (q: string) => ({ text: q, chips: [] }),
  apiUrl: (p: string) => p,
}));

function Rerender({ renderPanel }: { renderPanel: () => React.ReactNode }) {
  const [, setTick] = useState(0);
  return (
    <div>
      <button data-testid="bump" onClick={() => setTick(t => t + 1)}>bump</button>
      {renderPanel()}
    </div>
  );
}

const baseState: AppState = {
  ...init,
  repo: 'alpha',
  branch: 'agent/test',
  headCommit: 'head0001234',
  context: { kind: 'repo', repo: 'alpha' },
};

const repos: RepoInfo[] = [{ name: 'alpha' } as RepoInfo, { name: 'beta' } as RepoInfo];

beforeEach(() => {
  renders.isLensContext = 0;
  renders.currentPath = 0;
  renders.consoleState = 0;
});

describe('panel memoization — a parent re-render with stable props costs zero panel renders', () => {
  it('TopBar', () => {
    const dispatch = vi.fn();
    const onManageRepos = vi.fn();
    render(
      <Rerender renderPanel={() => (
        <TopBar state={baseState} repos={repos} dispatch={dispatch} onManageRepos={onManageRepos} leftWidth={300} />
      )} />,
    );
    const after = renders.isLensContext;
    expect(after).toBeGreaterThan(0);

    fireEvent.click(screen.getByTestId('bump'));
    fireEvent.click(screen.getByTestId('bump'));
    expect(renders.isLensContext).toBe(after);
  });

  it('FilterBar', () => {
    const dispatch = vi.fn();
    const onJumpTrail = vi.fn();
    render(
      <Rerender renderPanel={() => (
        <FilterBar state={baseState} dispatch={dispatch} onJumpTrail={onJumpTrail} />
      )} />,
    );
    const after = renders.isLensContext;
    expect(after).toBeGreaterThan(0);

    fireEvent.click(screen.getByTestId('bump'));
    fireEvent.click(screen.getByTestId('bump'));
    expect(renders.isLensContext).toBe(after);
  });

  it('Console', () => {
    const dispatch = vi.fn();
    render(
      <Rerender renderPanel={() => (
        <Console state={baseState} dispatch={dispatch} version="0.0.0.abc" />
      )} />,
    );
    const after = renders.consoleState;
    expect(after).toBeGreaterThan(0);

    fireEvent.click(screen.getByTestId('bump'));
    fireEvent.click(screen.getByTestId('bump'));
    expect(renders.consoleState).toBe(after);
  });

  it('RightPanel', async () => {
    const dispatch = vi.fn();
    const onScrub = vi.fn();
    const onHopRef = vi.fn();
    render(
      <Rerender renderPanel={() => (
        <RightPanel state={baseState} dispatch={dispatch} onScrub={onScrub} onHopRef={onHopRef} />
      )} />,
    );
    // RightPanel fetches on mount; let the effect cascade settle so the counter
    // starts from a quiescent tree rather than mid-fetch.
    await waitFor(() => expect(renders.currentPath).toBeGreaterThan(0));
    await act(async () => { await Promise.resolve(); });
    const after = renders.currentPath;

    fireEvent.click(screen.getByTestId('bump'));
    fireEvent.click(screen.getByTestId('bump'));
    expect(renders.currentPath).toBe(after);
  });
});

// Control: the counters are live instruments, not dead ones. A prop that really
// moved must still re-render the panel — otherwise a zero above would prove
// nothing about the memo.
describe('panel memoization — the counters are live', () => {
  it('a changed prop re-renders TopBar', () => {
    const dispatch = vi.fn();
    const onManageRepos = vi.fn();
    const { rerender } = render(
      <TopBar state={baseState} repos={repos} dispatch={dispatch} onManageRepos={onManageRepos} leftWidth={300} />,
    );
    const after = renders.isLensContext;
    rerender(
      <TopBar state={baseState} repos={repos} dispatch={dispatch} onManageRepos={onManageRepos} leftWidth={340} />,
    );
    expect(renders.isLensContext).toBeGreaterThan(after);
  });

  it('a changed prop re-renders Console', () => {
    const dispatch = vi.fn();
    const { rerender } = render(<Console state={baseState} dispatch={dispatch} version="0.0.0.abc" />);
    const after = renders.consoleState;
    rerender(<Console state={baseState} dispatch={dispatch} version="0.0.1.def" />);
    expect(renders.consoleState).toBeGreaterThan(after);
  });
});
