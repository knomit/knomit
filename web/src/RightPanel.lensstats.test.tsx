// Tests for the RightPanel SUMMARY view (no fact open) in lens vs repo
// context: a lens context fetches ONE union stats call through the lens
// endpoint (getLensStats) and renders the roll-up header + per-repo rows; a
// repo context keeps the existing api.stats/api.activity pair and rendering.

import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { RightPanel } from './RightPanel';
import { init } from './state';
import type { AppState } from './state';
import type { Lens, LensStats } from './api';

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

const lens: Lens = { name: 'eng', write: 'core', reads: [{ repo: 'core' }, { repo: 'docs' }] };

const unionStats: LensStats = {
  total: 250, repo_count: 2, last_commit: '2026-07-20T10:00:00Z', avg_confidence: 0.82,
  domains: { go: 7, ai: 10 }, entities: { chi: 3 },
  types: {}, highlights: [], default_axis: 'confidence',
  repos: [
    { id: 'coreid123456', name: 'core', source: '', branch: 'agent/main', is_write: true,
      total: 200, avg_confidence: 0.9, domains: { go: 5, ai: 10 }, entities: { chi: 3 },
      last_commit: '2026-07-19T09:00:00Z', changes_7d: 1, changes_30d: 2, changes_90d: 3 },
    { id: 'docsid123456', name: 'docs', source: 'src://docs', branch: 'main', is_write: false,
      total: 50, avg_confidence: 0.5, domains: { go: 2 }, entities: {},
      last_commit: '2026-07-20T10:00:00Z', changes_7d: 4, changes_30d: 5, changes_90d: 6 },
  ],
};

function lensSummaryState(): AppState {
  return {
    ...init,
    repo: 'core',
    branch: 'agent/main',
    headCommit: 'aaaaaaa',
    context: { kind: 'lens', name: 'eng' },
    lens,
    factPath: null,
  };
}

function repoSummaryState(): AppState {
  return { ...init, repo: 'core', branch: 'agent/main', headCommit: 'aaaaaaa', factPath: null };
}

describe('RightPanel — summary view stats routing', () => {
  beforeEach(() => { vi.clearAllMocks(); });

  it('lens context fetches union stats through the lens endpoint, never the repo pair', async () => {
    (api.getLensStats as ReturnType<typeof vi.fn>).mockResolvedValue(unionStats);
    render(<RightPanel state={lensSummaryState()} dispatch={vi.fn()} />);
    await screen.findByTestId('lens-stats-header');
    expect(api.getLensStats).toHaveBeenCalledWith('eng', 'kb');
    expect(api.stats).not.toHaveBeenCalled();
    expect(api.activity).not.toHaveBeenCalled();
  });

  it('renders the union header and merged stat boxes', async () => {
    (api.getLensStats as ReturnType<typeof vi.fn>).mockResolvedValue(unionStats);
    const { container } = render(<RightPanel state={lensSummaryState()} dispatch={vi.fn()} />);
    const header = await screen.findByTestId('lens-stats-header');
    expect(header.textContent).toContain('250 facts');
    expect(header.textContent).toContain('2 repos');
    expect(header.textContent).toContain('updated');
    expect(container.textContent).toContain('0.82'); // weighted union confidence
  });

  it('renders one row per mount with the write marker on the write repo only', async () => {
    (api.getLensStats as ReturnType<typeof vi.fn>).mockResolvedValue(unionStats);
    render(<RightPanel state={lensSummaryState()} dispatch={vi.fn()} />);
    await screen.findByTestId('lens-stats-header');
    const rows = screen.getAllByTestId('lens-repo-row');
    expect(rows).toHaveLength(2);
    expect(rows[0].getAttribute('data-repo')).toBe('core');
    expect(rows[1].getAttribute('data-repo')).toBe('docs');
    const markers = screen.getAllByTestId('write-marker');
    expect(markers).toHaveLength(1);
    expect(rows[0].contains(markers[0])).toBe(true);
    expect(rows[1].textContent).toContain('50 facts');
    expect(rows[1].textContent).toContain('0.50');
  });

  it('merged histogram tags dispatch ADD_FILTER on click', async () => {
    (api.getLensStats as ReturnType<typeof vi.fn>).mockResolvedValue(unionStats);
    const dispatch = vi.fn();
    render(<RightPanel state={lensSummaryState()} dispatch={dispatch} />);
    await screen.findByTestId('lens-stats-header');
    const tag = screen.getAllByTestId('tag-item').find(el => el.getAttribute('data-value') === 'ai')!;
    fireEvent.click(tag);
    expect(dispatch).toHaveBeenCalledWith({ type: 'ADD_FILTER', chip: { category: 'domain', value: 'ai' } });
  });

  it('a failed union fetch surfaces an error, never the false "no facts" empty state', async () => {
    // The backend fails the WHOLE request on any mount error (RFC §9.1), so a
    // rejection means "a mount failed", NOT "the lens is empty".
    (api.getLensStats as ReturnType<typeof vi.fn>).mockRejectedValue(new Error('mount 500'));
    render(<RightPanel state={lensSummaryState()} dispatch={vi.fn()} />);
    await screen.findByTestId('lens-stats-error');
    expect(screen.queryByTestId('lens-stats-header')).toBeNull();
    expect(screen.queryByText(/no facts indexed/i)).toBeNull();
  });

  it('repo context keeps the repo stats/activity pair and never calls getLensStats', async () => {
    (api.stats as ReturnType<typeof vi.fn>).mockResolvedValue(
      { total: 42, avg_confidence: 0.85, domains: { ai: 10 }, entities: {} });
    (api.activity as ReturnType<typeof vi.fn>).mockResolvedValue(
      { last_commit: '2026-07-20T10:00:00Z', total: 9, changes_7d: 1, changes_30d: 2, changes_90d: 3 });
    render(<RightPanel state={repoSummaryState()} dispatch={vi.fn()} />);
    await waitFor(() => expect(screen.getByTestId('stats-view').textContent).toContain('42 facts'));
    expect(api.stats).toHaveBeenCalledWith('core', 'agent/main', 'kb');
    expect(api.activity).toHaveBeenCalledWith('core', 'agent/main', 'kb');
    expect(api.getLensStats).not.toHaveBeenCalled();
    expect(screen.queryByTestId('lens-stats-header')).toBeNull();
  });
});
