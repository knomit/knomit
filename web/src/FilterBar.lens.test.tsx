import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';

// Keep the real parseFilterQuery; stub only the `api` object so completions
// fetches are observable and never hit the network.
vi.mock('./api', async () => {
  const actual = await vi.importActual<typeof import('./api')>('./api');
  return {
    ...actual,
    api: {
      completions: vi.fn().mockResolvedValue({ values: ['kb', 'kb/ops'] }),
      lensCompletions: vi.fn().mockResolvedValue({ values: ['infra', 'docs'] }),
    },
  };
});

import { FilterBar } from './FilterBar';
import { parseFilterQuery } from './api';
import { init } from './state';
import type { AppState } from './state';
import type { Lens } from './api';
import { repoHue } from './utils';

const lens: Lens = {
  name: 'eng',
  write: 'core',
  reads: [{ repo: 'core' }, { repo: 'docs' }, { repo: 'infra' }],
};

function lensState(overrides: Partial<AppState> = {}): AppState {
  return {
    ...init,
    repo: 'core',
    branch: 'agent/main',
    headCommit: 'aaaaaaa',
    context: { kind: 'lens', name: 'eng' },
    lens,
    lensSources: null,
    ...overrides,
  };
}

function repoState(overrides: Partial<AppState> = {}): AppState {
  return { ...init, repo: 'core', branch: 'agent/main', headCommit: 'aaaaaaa', ...overrides };
}

// jsdom serializes inline `color` as `rgb(r, g, b)`.
function hexToRgb(hex: string): string {
  const r = parseInt(hex.slice(1, 3), 16);
  const g = parseInt(hex.slice(3, 5), 16);
  const b = parseInt(hex.slice(5, 7), 16);
  return `rgb(${r}, ${g}, ${b})`;
}

describe('parseFilterQuery — repo: facet is context-aware', () => {
  it('parses repo:infra as a chip when allowRepo is set (lens context)', () => {
    const r = parseFilterQuery('repo:infra', undefined, { allowRepo: true });
    expect(r.chips).toEqual([{ category: 'repo', value: 'infra' }]);
    expect(r.text).toBe('');
  });

  it('leaves repo:infra as free text by default (repo context)', () => {
    const r = parseFilterQuery('repo:infra');
    expect(r.chips).toEqual([]);
    expect(r.text).toBe('repo:infra');
  });

  it('does not disturb the other categories in lens context', () => {
    const r = parseFilterQuery('domain:ai repo:infra path:kb/ops', undefined, { allowRepo: true });
    // Bare tokens are extracted in left-to-right string order.
    expect(r.chips).toEqual([
      { category: 'domain', value: 'ai' },
      { category: 'repo', value: 'infra' },
      { category: 'path', value: 'kb/ops' },
    ]);
  });
});

describe('FilterBar — repo: facet (lens context only)', () => {
  beforeEach(() => { vi.clearAllMocks(); });

  it('offers NO Repo category in the picker, in either context', () => {
    // Scoping a lens to one mount is a move, not a content filter, and it has
    // two controls already: the sources dropdown and the summary's Repos rows.
    // A third one shaped like a chip made narrowing the fan-out look like
    // narrowing the results, and left the sources dropdown disagreeing with the
    // union it was describing.
    const { unmount } = render(<FilterBar state={repoState()} dispatch={vi.fn()} />);
    fireEvent.click(screen.getByTitle('Add filter'));
    expect(screen.queryByTestId('picker-cat-repo')).toBeNull();
    unmount();

    render(<FilterBar state={lensState()} dispatch={vi.fn()} />);
    fireEvent.click(screen.getByTitle('Add filter'));
    expect(screen.queryByTestId('picker-cat-repo')).toBeNull();
  });

  it('does not autocomplete repo: — it is free text now, in every context', async () => {
    const { api } = await import('./api');
    render(<FilterBar state={lensState()} dispatch={vi.fn()} />);
    const input = document.getElementById('filter-input') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'repo:inf' } });
    await new Promise(r => setTimeout(r, 250));
    expect(api.lensCompletions).not.toHaveBeenCalled();
    expect(api.completions).not.toHaveBeenCalled();
  });

  it('fetches ALL category completions via api.lensCompletions in lens context (union across mounts)', async () => {
    // Regression: only the repo category was routed through the lens endpoint;
    // domain/entity/path/etc hit the repo endpoint with state.repo (the write
    // repo), silently hiding every read mount's values from autocomplete.
    const { api } = await import('./api');
    render(<FilterBar state={lensState()} dispatch={vi.fn()} />);
    const input = document.getElementById('filter-input') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'domain:se' } });
    await waitFor(() => expect(api.lensCompletions).toHaveBeenCalledWith('eng', 'domain', 'se'));
    expect(api.completions).not.toHaveBeenCalled();
  });

  it('domain completions in repo context still use the repo endpoint', async () => {
    const { api } = await import('./api');
    render(<FilterBar state={repoState()} dispatch={vi.fn()} />);
    const input = document.getElementById('filter-input') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'domain:se' } });
    await waitFor(() => expect(api.completions).toHaveBeenCalled());
    expect(api.lensCompletions).not.toHaveBeenCalled();
  });

  it('does not recognise repo: as a facet in repo context (free text)', async () => {
    const { api } = await import('./api');
    render(<FilterBar state={repoState()} dispatch={vi.fn()} />);
    const input = document.getElementById('filter-input') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'repo:inf' } });
    // No prefix match → no completion fetch of any kind.
    await new Promise(r => setTimeout(r, 200));
    expect(api.lensCompletions).not.toHaveBeenCalled();
    expect(api.completions).not.toHaveBeenCalled();
  });

  it('Enter does NOT commit a repo chip in lens context either', () => {
    // The bar stopped passing allowRepo, so `repo:infra` parses as free text
    // here exactly as it always has in a repo context.
    const dispatch = vi.fn();
    render(<FilterBar state={lensState()} dispatch={dispatch} />);
    const input = document.getElementById('filter-input') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'repo:infra' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(dispatch).not.toHaveBeenCalledWith(
      expect.objectContaining({ type: 'ADD_FILTER', chip: expect.objectContaining({ category: 'repo' }) }),
    );
    expect(dispatch).not.toHaveBeenCalledWith(
      expect.objectContaining({ type: 'FOCUS_LENS_SOURCE' }),
    );
  });

  it('Enter does NOT commit a repo chip in repo context', () => {
    const dispatch = vi.fn();
    render(<FilterBar state={repoState()} dispatch={dispatch} />);
    const input = document.getElementById('filter-input') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'repo:infra' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(dispatch).not.toHaveBeenCalledWith(
      expect.objectContaining({ type: 'ADD_FILTER', chip: expect.objectContaining({ category: 'repo' }) }),
    );
  });

  it('renders a repo chip in the deterministic repo hue', () => {
    render(<FilterBar state={lensState({ filters: [{ category: 'repo', value: 'infra' }] })} dispatch={vi.fn()} />);
    const chip = screen.getByTestId('repo-chip');
    expect(chip.getAttribute('data-repo')).toBe('infra');
    expect(chip.textContent).toContain('repo:infra');
    expect(chip.style.color).toBe(hexToRgb(repoHue('infra')));
  });
});
