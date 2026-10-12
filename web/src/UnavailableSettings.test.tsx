import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent, within, cleanup } from '@testing-library/react';
import { UnavailableSettings } from './UnavailableSettings';
import { api, type RepoInfo } from './api';

// A registered repo with no live store is a target of its own: pinned from the
// rail, Settings only. Its page says what is wrong in the server's own words
// and offers the one way out the server has — Archive, with an optional note.
// "Check again", "Locate store…" and "Clone from remote" are deliberately NOT
// offered: the server has no such operations.

vi.mock('./api', async importOriginal => ({
  ...(await importOriginal<typeof import('./api')>()),
  api: {
    archiveRepo: vi.fn(),
    getRepo: vi.fn(), getAgentBranch: vi.fn(), getOrigin: vi.fn(),
  },
}));

const DETAILS: Record<string, string> = {
  missing: 'database file not found: /data/repos/u-ghost.db',
  unopenable: 'ontology on agent/test: .knomit/ontology.yaml is a symlink, and knomit does not follow symlinks',
  conflict: 'knowledge base already held by core-kb',
};
const repo = (state: string): RepoInfo => ({ name: 'ghost', uid: 'u-ghost', state, detail: DETAILS[state] });

function renderPage(r: RepoInfo, over: Partial<React.ComponentProps<typeof UnavailableSettings>> = {}) {
  const onArchived = vi.fn();
  render(<UnavailableSettings repo={r} serverReadOnly={false} onArchived={onArchived} {...over} />);
  return { onArchived };
}

beforeEach(() => { vi.clearAllMocks(); });
afterEach(() => { cleanup(); });

describe('UnavailableSettings', () => {
  it.each(['missing', 'unopenable', 'conflict'])('the Store callout quotes the server detail verbatim for %s', state => {
    renderPage(repo(state));
    const store = screen.getByTestId('block-store');
    const callout = within(store).getByTestId('repo-unavailable-callout');
    expect(within(callout).getByTestId('repo-unavailable-detail').textContent).toBe(DETAILS[state]);
  });

  it('heads the callout by the state, in the amber tone', () => {
    renderPage(repo('missing'));
    const callout = screen.getByTestId('repo-unavailable-callout');
    expect(callout).toHaveTextContent('Store missing');
    expect(callout).toHaveStyle({ background: '#1d1a12', color: '#e2c07a' });
  });

  it('gives advice that names something the product can do, per state', () => {
    renderPage(repo('missing'));
    expect(screen.getByTestId('block-store').textContent).toContain('Put the file back');
    cleanup();
    renderPage(repo('unopenable'));
    const store = screen.getByTestId('block-store').textContent ?? '';
    expect(store).toContain('an ontology that is a symlink');
    expect(store).toContain('does not sync');
  });

  it('has exactly the Store and Danger zone sections, and marks Store as the problem', () => {
    renderPage(repo('missing'));
    const ids = Array.from(document.querySelectorAll('section[id]')).map(s => s.id);
    expect(ids).toEqual(['store', 'danger']);
    expect(screen.getByTestId('toc-store-problem')).toBeInTheDocument();
    expect(screen.queryByTestId('toc-danger-problem')).toBeNull();
    expect(screen.getByTestId('settings-index-heading')).toHaveTextContent(/^Repository$/);
  });

  it('reads nothing through the store it does not have', () => {
    renderPage(repo('missing'));
    expect(api.getRepo).not.toHaveBeenCalled();
    expect(api.getAgentBranch).not.toHaveBeenCalled();
    expect(api.getOrigin).not.toHaveBeenCalled();
  });

  it('archives with the exact note and reports the archived row', async () => {
    const archivedRow = { id: 'u-ghost', name: 'ghost', origin: '', archivedAt: '2026-10-09T00:00:00Z' };
    vi.mocked(api.archiveRepo).mockResolvedValue(archivedRow);
    const { onArchived } = renderPage(repo('missing'));
    fireEvent.click(screen.getByTestId('repo-unavailable-archive'));
    fireEvent.change(screen.getByTestId('repo-unavailable-archive-note'), { target: { value: 'disk is gone' } });
    fireEvent.click(screen.getByTestId('repo-unavailable-archive-confirm'));
    await waitFor(() => expect(onArchived).toHaveBeenCalledWith(archivedRow));
    expect(api.archiveRepo).toHaveBeenCalledWith('ghost', 'disk is gone');
  });

  it('disables Archive on a read-only server, and enables it otherwise', () => {
    renderPage(repo('missing'));
    expect(screen.getByTestId('repo-unavailable-archive')).toBeEnabled();
    cleanup();
    renderPage(repo('missing'), { serverReadOnly: true });
    expect(screen.getByTestId('repo-unavailable-archive')).toBeDisabled();
  });

  it('offers no Check again, Locate store or Clone from remote: the server has no such operations', () => {
    renderPage(repo('missing'));
    // Control: the one action that exists is a button on the page.
    expect(screen.getByRole('button', { name: /Archive/ })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /check again/i })).toBeNull();
    expect(screen.queryByRole('button', { name: /locate/i })).toBeNull();
    expect(screen.queryByRole('button', { name: /clone/i })).toBeNull();
  });
});
