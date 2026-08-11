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
    search: vi.fn(async () => []),
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

// A chip is a FILTER; only text is a QUERY.
//
// searchActive drove effectiveSort to 'relevance', and it was true for any
// content chip — so clicking "ai · 666" on the dashboard routed to the lens
// SEARCH endpoint: retrieval-capped, unpaged in this panel (its sentinel is
// disabled), and reporting the size of the fetched candidate set. The reader
// clicked a 666 and got a list of 50 that would not scroll.
//
// With no text there is nothing to rank BY. Ordering stays recency, the facts
// endpoint already forwards every content filter, and it pages against a real
// COUNT(*) — so a chip belongs in whatever mode was already showing.
describe('a content chip filters; it does not become a search', () => {
  beforeEach(() => vi.clearAllMocks());

  it('keeps Recent sort (and its paging) when only a chip is set', async () => {
    render(<Library
      state={repoState({ librarySort: 'recent', filters: [{ category: 'domain', value: 'ai' }] })}
      dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('chrono-item').length).toBe(2));
    expect(screen.getByTestId('left-panel').getAttribute('data-sort')).toBe('recent');
    // The count is the server's, not the page's — 385 from the stubbed total.
    expect(screen.getByTestId('library-count').textContent).toBe('385');
  });

  it('leaves Path sort for a list, because a tree cannot express a chip', async () => {
    // The ontology tree is a directory structure and the topics endpoint takes
    // no content filters — so a chip applied in Path mode is silently ignored,
    // which is what clicking "ai · 666" on the dashboard used to look like:
    // a chip in the bar and an unchanged tree of 16 topics. A filter needs a
    // flat list to be a filter OF.
    render(<Library
      state={repoState({ librarySort: 'path', filters: [{ category: 'domain', value: 'ai' }] })}
      dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getByTestId('left-panel').getAttribute('data-sort')).toBe('recent'));
  });

  it('goes back to Path when the chip is removed', async () => {
    // librarySort is never overwritten — only derived from — so removing the
    // chip returns the reader to the mode they were actually in.
    const { rerender } = render(<Library
      state={repoState({ librarySort: 'path', filters: [{ category: 'domain', value: 'ai' }] })}
      dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getByTestId('left-panel').getAttribute('data-sort')).toBe('recent'));
    rerender(<Library state={repoState({ librarySort: 'path', filters: [] })} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getByTestId('left-panel').getAttribute('data-sort')).toBe('path'));
  });

  it('still switches to relevance for free text, which DOES have a ranking', async () => {
    render(<Library
      state={repoState({ librarySort: 'recent', freeText: 'context rot' })}
      dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getByTestId('left-panel').getAttribute('data-sort')).toBe('relevance'));
  });

  it('forwards the chip to the facts endpoint rather than dropping it', async () => {
    const { api } = await import('./api');
    render(<Library
      state={repoState({ librarySort: 'recent', filters: [{ category: 'domain', value: 'ai' }] })}
      dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(api.recent).toHaveBeenCalled());
    expect(JSON.stringify(vi.mocked(api.recent).mock.calls[0])).toContain('ai');
  });

  // Type chips are OR-combined, so a second one must WIDEN the match. The
  // facts endpoint splits `type` on commas into SearchOptions.IncludeTypes and
  // has always taken a list; the client was the narrow end, holding one
  // `typeFilter` string. That was survivable while any chip routed through
  // /search (which forwards the whole array) and became a hole the moment
  // chips stopped being searches: two chips collapsed to undefined, no `type=`
  // was sent at all, and the list silently widened to the whole folder while
  // both chips sat in the bar claiming to narrow it.
  it('forwards EVERY type chip, so a second one widens rather than disabling the filter', async () => {
    const { api } = await import('./api');
    render(<Library
      state={repoState({
        librarySort: 'recent',
        filters: [{ category: 'type', value: 'synthesis' }, { category: 'type', value: 'invariant' }],
      })}
      dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(api.recent).toHaveBeenCalled());
    const opts = vi.mocked(api.recent).mock.calls[0][6];
    expect(opts?.types).toEqual(['synthesis', 'invariant']);
  });

  it('sends one type chip as a one-element list, not as a bare string', async () => {
    const { api } = await import('./api');
    render(<Library
      state={repoState({ librarySort: 'recent', filters: [{ category: 'type', value: 'synthesis' }] })}
      dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(api.recent).toHaveBeenCalled());
    expect(vi.mocked(api.recent).mock.calls[0][6]?.types).toEqual(['synthesis']);
  });
});
