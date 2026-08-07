// Tests for the highlights list wired into the repo summary view: rendering,
// opening a row through the shared live navigate path (no commit pin — see
// kb/invariants/ui/navigation/every-hop-is-path-plus-commit), and picking an
// axis triggering a REFETCH rather than a client-side resort.

import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { RightPanel } from './RightPanel';
import { init } from './state';
import type { AppState } from './state';

vi.mock('./api', () => ({
  api: {
    fact: vi.fn(),
    getLensFact: vi.fn(),
    getLensStats: vi.fn(),
    getAgentBranch: vi.fn().mockResolvedValue('agent/main'),
    stats: vi.fn().mockResolvedValue(null),
    activity: vi.fn().mockResolvedValue(null),
    updateFact: vi.fn(),
    retractFact: vi.fn(),
    factDiff: vi.fn().mockResolvedValue({ from: null, to: null }),
    commitDetail: vi.fn().mockResolvedValue(null),
    factCommits: vi.fn().mockResolvedValue({ entries: [] }),
  },
}));

import { api } from './api';

function repoState(): AppState {
  return { ...init, repo: 'core', branch: 'agent/main', headCommit: 'aaaaaaa', factPath: null };
}

describe('RightPanel — highlights in the repo summary view', () => {
  beforeEach(() => { vi.clearAllMocks(); });

  it('renders highlights in the repo summary view', async () => {
    vi.mocked(api.stats).mockResolvedValue({
      total: 100, avg_confidence: 0.8, domains: { ai: 5 }, entities: {},
      types: { synthesis: 2, observation: 98 },
      default_axis: 'impact',
      highlights: [{
        path: 'kb/s/a.md', title: 'Load-bearing synthesis', type: 'synthesis',
        confidence: 0.6, impact: 22, committed_at: 1780000000,
      }],
    });

    render(<RightPanel state={repoState()} dispatch={vi.fn()} navigate={vi.fn()}
      onScrub={vi.fn()} onHopRef={vi.fn()} repoNames={{}} />);

    await waitFor(() => {
      expect(screen.getByText('Load-bearing synthesis')).toBeInTheDocument();
    });
    expect(screen.getAllByTestId('highlight-type-icon').length).toBeGreaterThan(0);
  });

  it('opens a highlight through the shared navigate path, and REVEALS it', async () => {
    const navigate = vi.fn();
    vi.mocked(api.stats).mockResolvedValue({
      total: 1, avg_confidence: 0.8, domains: {}, entities: {},
      types: { synthesis: 1 }, default_axis: 'impact',
      highlights: [{
        path: 'kb/s/a.md', title: 'Listed fact', type: 'synthesis',
        confidence: 0.6, impact: 5, committed_at: 1780000000,
      }],
    });

    render(<RightPanel state={repoState()} dispatch={vi.fn()} navigate={navigate}
      onScrub={vi.fn()} onHopRef={vi.fn()} repoNames={{}} />);

    await waitFor(() => screen.getByText('Listed fact'));
    fireEvent.click(screen.getByText('Listed fact'));

    // reveal, not a bare selection: a highlight should land you in the folder
    // the fact lives in, the way browsing to it would. Without it you arrived at
    // a fact floating over whatever the left panel happened to be showing.
    expect(navigate).toHaveBeenCalledWith({ view: 'library', factPath: 'kb/s/a.md', reveal: true });
  });

  it('picking an axis refetches rather than re-sorting', async () => {
    vi.mocked(api.stats).mockResolvedValue({
      total: 1, avg_confidence: 0.8, domains: {}, entities: {},
      types: { synthesis: 1 }, default_axis: 'impact',
      highlights: [{
        path: 'kb/s/a.md', title: 'Only row', type: 'synthesis',
        confidence: 0.6, impact: 5, committed_at: 1780000000,
      }],
    });

    render(<RightPanel state={repoState()} dispatch={vi.fn()} navigate={vi.fn()}
      onScrub={vi.fn()} onHopRef={vi.fn()} repoNames={{}} />);
    await waitFor(() => screen.getByText('Only row'));

    fireEvent.click(screen.getByRole('button', { name: 'Recent' }));

    await waitFor(() => {
      expect(vi.mocked(api.stats)).toHaveBeenLastCalledWith(
        expect.anything(), expect.anything(), expect.anything(), 'recent');
    });
  });
});
