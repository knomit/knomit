// The count beside the root label was rows.length — which on the paged tabs is
// how far you have scrolled, not how many facts there are. A repo with 385
// facts read "50", then "100" as you scrolled, and would have read "385" only
// if you reached the bottom. The server's total was already in component state,
// gating the infinite-scroll sentinel; it just was not the number on screen.
//
// The Path tab keeps rows.length: a directory listing is not paged, so there
// the two numbers are the same thing.

import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { Library } from './Library';
import { init } from './state';
import type { AppState } from './state';

vi.mock('./api', () => ({
  api: {
    browse: vi.fn(async () => ({ children: [
      { name: 'architecture', is_dir: true }, { name: 'business', is_dir: true },
    ] })),
    recent: vi.fn(async () => ({
      // one page of 2 out of a much larger corpus
      facts: [
        { path: 'kb/a.md', title: 'A', type: 'observation', committed_at: 2 },
        { path: 'kb/b.md', title: 'B', type: 'observation', committed_at: 1 },
      ],
      total: 385,
    })),
    lensBrowse: vi.fn(async () => ({ children: [] })),
    listLensFacts: vi.fn(async () => ({ facts: [], total: 0 })),
    lensSearch: vi.fn(async () => []),
    listLensFactsSearch: vi.fn(async () => ({ facts: [], total: 0 })),
  },
}));

const repoState = (over: Partial<AppState> = {}): AppState => ({
  ...init, repo: 'knomit-kb', branch: 'agent/test',
  context: { kind: 'repo', repo: 'knomit-kb' }, ...over,
});

const count = () => screen.getByTestId('library-header').querySelector('[data-testid="library-count"]')?.textContent;

describe('the root count', () => {
  beforeEach(() => vi.clearAllMocks());

  it('is the server total on Recent, not the number of rows loaded', async () => {
    render(<Library state={repoState({ librarySort: 'recent' })} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('chrono-item').length).toBe(2));
    expect(count()).toBe('385');
  });

  it('is the row count on Path, where the listing is complete', async () => {
    render(<Library state={repoState({ librarySort: 'path' })} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('dir-entry').length).toBe(2));
    expect(count()).toBe('2');
  });
});
