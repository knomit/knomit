// The motif row in the filter picker.
//
// It is the one category that ASKS. Every other row reads a completions list of
// bare strings; this reads the motif collection, which arrives ranked and whose
// search reaches the definitions. That buys something the siblings cannot do,
// and costs a wait and the possibility of no answer — so it needs a third state
// the others never did.

import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { FilterBar } from './FilterBar';
import { api } from './api';
import { init } from './state';
import type { AppState } from './state';

vi.mock('./api', () => ({
  api: { motifs: vi.fn(), completions: vi.fn(async () => ({ values: [] })), lensCompletions: vi.fn(async () => ({ values: [] })) },
  parseFilterQuery: (raw: string) => ({ chips: [], text: raw, warnings: [] }),
}));

// Deliberately NOT alphabetical: df-descending is the order the server chose and
// it carries meaning, so a picker that sorted these would destroy information.
const RANKED = ['failure-presents-as-success', 'bypass-defeats-guarantee', 'absence-encodes-value'];

const repoState = (over: Partial<AppState> = {}): AppState => ({
  ...init, repo: 'knomit-kb', branch: 'agent/test',
  context: { kind: 'repo', repo: 'knomit-kb' }, ...over,
});

const openPicker = (state = repoState()) => {
  render(<FilterBar state={state} dispatch={vi.fn()} />);
  fireEvent.click(screen.getByTitle('Add filter'));
};

describe('the motif category in the picker', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    (api.motifs as ReturnType<typeof vi.fn>).mockResolvedValue({
      count: 3, health: {}, motifs: RANKED.map((canonical, i) => ({
        cluster_key: `k${i}`, canonical, members: [canonical], df: 26 - i * 9,
      })),
    });
  });

  it('offers Motif in a repo, and keeps the server’s order', async () => {
    openPicker();
    fireEvent.click(screen.getByTestId('picker-cat-motif'));
    await waitFor(() => expect(screen.getAllByTestId('picker-value').length).toBe(3));
    // Most-used first, exactly as sent. Alphabetical would be
    // absence/bypass/failure — a different list, and a lie about rank.
    expect(screen.getAllByTestId('picker-value').map(e => e.getAttribute('data-value')))
      .toEqual(RANKED);
  });

  it('reads the motif collection, not the shared completions endpoint', async () => {
    openPicker();
    fireEvent.click(screen.getByTestId('picker-cat-motif'));
    await waitFor(() => expect(api.motifs).toHaveBeenCalled());
    expect(api.motifs).toHaveBeenCalledWith('knomit-kb', 'agent/test',
      expect.objectContaining({ sort: 'df' }));
    expect(api.completions).not.toHaveBeenCalled();
  });

  it('shows no counts, like every other row here', async () => {
    openPicker();
    fireEvent.click(screen.getByTestId('picker-cat-motif'));
    await waitFor(() => expect(screen.getAllByTestId('picker-value').length).toBe(3));
    const pane = screen.getByTestId('picker-values');
    expect(pane.textContent).not.toContain('26');
    expect(pane.textContent).not.toContain('17');
  });

  it('promises the search reaches meanings, which its siblings cannot', async () => {
    openPicker();
    fireEvent.click(screen.getByTestId('picker-cat-motif'));
    await waitFor(() => expect(screen.getAllByTestId('picker-value').length).toBe(3));
    // Only appears above SEARCHABLE_FROM values, so ask for a big vocabulary.
    (api.motifs as ReturnType<typeof vi.fn>).mockResolvedValue({
      count: 40, health: {},
      motifs: Array.from({ length: 40 }, (_, i) => ({
        cluster_key: `k${i}`, canonical: `motif-number-${i}`, members: [], df: 40 - i,
      })),
    });
    fireEvent.click(screen.getByTestId('picker-cat-domain'));
    fireEvent.click(screen.getByTestId('picker-cat-motif'));
    await waitFor(() => expect(screen.getByTestId('picker-search')).toBeTruthy());
    expect(screen.getByTestId('picker-search').getAttribute('placeholder'))
      .toBe('Search names and meanings…');
  });

  it('says the vocabulary could not be read, rather than showing none', async () => {
    (api.motifs as ReturnType<typeof vi.fn>).mockRejectedValue(new Error('502'));
    openPicker();
    fireEvent.click(screen.getByTestId('picker-cat-motif'));
    // An empty pane here would read as "this repo has no shared shapes", which
    // is what the siblings' single empty state would have said.
    await waitFor(() => expect(screen.getByTestId('picker-empty').textContent)
      .toContain('Couldn’t read the vocabulary'));
  });

  it('is absent in a lens, where no single vocabulary exists', () => {
    openPicker(repoState({ context: { kind: 'lens', name: 'dev' } }));
    // Nothing to list: cross-mount cluster identity does not exist, and
    // offering the write repo's names would hand the reader a vocabulary the
    // union does not have. The same reasoning keeps `repo` out of this rail.
    expect(screen.queryByTestId('picker-cat-motif')).toBeNull();
    expect(screen.getByTestId('picker-cat-domain')).toBeTruthy();
  });
});
