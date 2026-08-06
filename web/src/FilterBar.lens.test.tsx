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

  it('offers a Repo category in the picker only in lens context', () => {
    const { unmount } = render(<FilterBar state={repoState()} dispatch={vi.fn()} />);
    fireEvent.click(screen.getByTitle('Add filter'));
    expect(screen.queryByText('Repo')).toBeNull();
    unmount();

    render(<FilterBar state={lensState()} dispatch={vi.fn()} />);
    fireEvent.click(screen.getByTitle('Add filter'));
    expect(screen.getByText('Repo')).toBeInTheDocument();
  });

  it('fetches completions for repo: via api.lensCompletions', async () => {
    const { api } = await import('./api');
    render(<FilterBar state={lensState()} dispatch={vi.fn()} />);
    const input = document.getElementById('filter-input') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'repo:inf' } });
    await waitFor(() => expect(api.lensCompletions).toHaveBeenCalledWith('eng', 'repo', 'inf'));
    expect(api.completions).not.toHaveBeenCalled();
    // 'docs' has no 'inf' substring so it renders as a single (unsplit) text node.
    await waitFor(() => expect(screen.getByText('docs')).toBeInTheDocument());
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

  it('Enter commits repo:infra to a repo chip in lens context', () => {
    const dispatch = vi.fn();
    render(<FilterBar state={lensState()} dispatch={dispatch} />);
    const input = document.getElementById('filter-input') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'repo:infra' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(dispatch).toHaveBeenCalledWith({ type: 'ADD_FILTER', chip: { category: 'repo', value: 'infra' } });
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

  it('picking a repo from the picker FOCUSES that mount — same as a dashboard repo row', async () => {
    // A repo is a scope, not a content filter. Picking one from the rail means
    // "show me this mount", which is one action that moves the sources
    // selection, the sort and the open fact together — not a chip that narrows
    // the fan-out while the sources dropdown still reads "All mounts".
    const dispatch = vi.fn();
    render(<FilterBar state={lensState()} dispatch={dispatch} />);
    fireEvent.click(screen.getByTitle('Add filter'));
    fireEvent.click(screen.getByTestId('picker-cat-repo'));
    await waitFor(() => expect(screen.getAllByTestId('picker-value').length).toBeGreaterThan(0));
    fireEvent.mouseDown(screen.getAllByTestId('picker-value').find(e => e.textContent === 'infra')!);
    expect(dispatch).toHaveBeenCalledWith({ type: 'FOCUS_LENS_SOURCE', repo: 'infra' });
    expect(dispatch).not.toHaveBeenCalledWith(
      expect.objectContaining({ type: 'ADD_FILTER', chip: expect.objectContaining({ category: 'repo' }) }),
    );
  });

  it('committing a repo: autocomplete suggestion focuses the mount too', async () => {
    // The other dropdown. Both are "I chose a mount from a list", so both must
    // land in the same place.
    const dispatch = vi.fn();
    render(<FilterBar state={lensState()} dispatch={dispatch} />);
    const input = document.getElementById('filter-input') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'repo:inf' } });
    // Wait on 'docs': the matched part of 'infra' is split into a highlight
    // element, so it is not one text node. 'docs' has no 'inf' in it and
    // renders whole — the same trick the completions test above uses.
    await waitFor(() => expect(screen.getByText('docs')).toBeInTheDocument());
    fireEvent.keyDown(input, { key: 'Enter' });   // commits suggestion 0 = infra
    expect(dispatch).toHaveBeenCalledWith({ type: 'FOCUS_LENS_SOURCE', repo: 'infra' });
  });

  it('leaves the other facets alone — only repo is a scope', async () => {
    const dispatch = vi.fn();
    render(<FilterBar state={lensState()} dispatch={dispatch} />);
    fireEvent.click(screen.getByTitle('Add filter'));
    fireEvent.click(screen.getByTestId('picker-cat-domain'));
    await waitFor(() => expect(screen.getAllByTestId('picker-value').length).toBeGreaterThan(0));
    fireEvent.mouseDown(screen.getAllByTestId('picker-value')[0]);
    expect(dispatch).toHaveBeenCalledWith(
      expect.objectContaining({ type: 'ADD_FILTER', chip: expect.objectContaining({ category: 'domain' }) }),
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
