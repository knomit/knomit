import { afterEach, describe, it, expect, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { RightPanel } from './RightPanel';
import { init } from './state';
import type { AppState } from './state';
import { makeFixtureApi } from './materializeFixture';
import { ScrollTextIcon } from './icons';
import { LayersIcon, BookIcon } from './icons';

const fixture = makeFixtureApi({ total: 4, pageSize: 4 });

vi.mock('./materializeApi', async importOriginal => ({
  ...(await importOriginal<typeof import('./materializeApi')>()),
  materializeApi: { page: (...a: Parameters<typeof fixture.api.page>) => fixture.api.page(...a), exportMarkdown: vi.fn() },
}));

vi.mock('./api', () => ({
  api: {
    fact: vi.fn().mockResolvedValue({
      path: 'kb/test/foo.md', title: 'Foo', body: 'body', type: 'observation', confidence: 0.9, sources: 1,
      domain: [], entities: [], refs: [], commit_hash: 'aaa1111', commit_date: '2026-05-01T00:00:00Z',
    }),
    factDiff: vi.fn().mockResolvedValue({ from: null, to: null }),
    stats: vi.fn().mockResolvedValue(null),
    activity: vi.fn().mockResolvedValue(null),
    commitDetail: vi.fn().mockResolvedValue(null),
    factCommits: vi.fn().mockResolvedValue({ entries: [] }),
    explain: vi.fn().mockResolvedValue({ incoming: [], outgoing: [] }),
  },
}));

const state: AppState = { ...init, repo: 'knomit', branch: 'machine/test', factPath: 'kb/test/foo.md', asOf: { mode: 'live' } };

afterEach(() => { window.history.replaceState(null, '', '/'); });

describe('the Materialize action in the fact header', () => {
  it('sits to the left of retract, icon-only, titled "Materialize"', async () => {
    render(<RightPanel state={state} dispatch={vi.fn()} />);
    const btn = await screen.findByTestId('materialize-btn');
    expect(btn).toHaveAttribute('title', 'Materialize');
    expect(btn.textContent).toBe('');
    expect(btn.querySelector('svg')).not.toBeNull();
    const retract = screen.getByTestId('retract-btn');
    expect(btn.compareDocumentPosition(retract) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(btn.parentElement).toBe(retract.parentElement);
  });

  it('draws a glyph of its own, not the lens mark or the repo mark', () => {
    const html = (el: React.ReactElement) => render(el).container.innerHTML;
    const scroll = html(<ScrollTextIcon color="#fff" />);
    expect(scroll).not.toBe(html(<LayersIcon color="#fff" />));
    expect(scroll).not.toBe(html(<BookIcon color="#fff" />));
  });

  it('swaps the fact view for the materialized view, and Back returns to the fact', async () => {
    render(<RightPanel state={state} dispatch={vi.fn()} />);
    fireEvent.click(await screen.findByTestId('materialize-btn'));
    expect(await screen.findByTestId('materialize-view')).toBeInTheDocument();
    expect(screen.queryByTestId('retract-btn')).toBeNull();
    await waitFor(() => expect(screen.getAllByTestId('mat-section')).toHaveLength(4));
    expect(window.location.search).toBe('?materialize=1');

    fireEvent.click(screen.getByTestId('mat-back'));
    expect(await screen.findByTestId('retract-btn')).toBeInTheDocument();
    expect(screen.queryByTestId('materialize-view')).toBeNull();
    expect(window.location.search).toBe('');
  });

  it('writes the options into the URL as they change', async () => {
    render(<RightPanel state={state} dispatch={vi.fn()} />);
    fireEvent.click(await screen.findByTestId('materialize-btn'));
    await screen.findByTestId('materialize-view');
    fireEvent.click(screen.getByLabelText('Raise Max depth'));
    fireEvent.click(screen.getByTestId('mat-unpin'));
    await waitFor(() => expect(new URLSearchParams(window.location.search).get('mat_depth')).toBe('5'));
    expect(new URLSearchParams(window.location.search).get('mat_unpin')).toBe('1');
  });

  it('opens straight into the view, with its options, from a shared URL', async () => {
    window.history.replaceState(null, '', '/?materialize=1&mat_depth=7&mat_facts=120');
    render(<RightPanel state={state} dispatch={vi.fn()} />);
    expect(await screen.findByTestId('materialize-view')).toBeInTheDocument();
    expect((screen.getByTestId('mat-depth') as HTMLInputElement).value).toBe('7');
    expect((screen.getByTestId('mat-facts') as HTMLInputElement).value).toBe('120');
  });
});
