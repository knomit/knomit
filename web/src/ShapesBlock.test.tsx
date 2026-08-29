import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { ShapesBlock } from './ShapesBlock';
import { api } from './api';

vi.mock('./api', () => ({ api: { motifs: vi.fn() } }));

const entry = (canonical: string, df: number, definition?: string) => ({
  cluster_key: `key-${canonical}`, canonical, members: [canonical], df, definition,
  definition_state: 'current' as const,
});

// Distinct figures throughout, so a field read from the wrong key fails.
const HEALTH = {
  authored_clusters: 73, authored_recurring: 37, authored_mints: 73,
  authored_links: 183, authored_epistemic_recurring: 29,
  recurrence_rate: 0.51, mint_to_link_ratio: 0.4,
};

const REUSED = [
  entry('failure-presents-as-success', 26, 'An operation that did not achieve its effect returns success signals.'),
  entry('parallel-implementations-diverge', 18, 'The same rule is implemented independently in several places.'),
  entry('bypass-defeats-guarantee', 17, 'A required intermediary can be circumvented.'),
  entry('false-universal-default', 17, 'A value that varies between contexts is fixed as a constant.'),
  entry('illegal-state-unrepresentable', 13, 'The structure permits only valid values.'),
  entry('check-then-act-race', 9, 'A condition is tested and acted upon as two separate steps.'),
  entry('test-mode-hides-condition', 8, 'The configuration used for reproduction removes the state a fault needs.'),
];
const ONCE = [entry('layout-shifts-under-cursor', 1), entry('sentinel-never-occurs', 1)];

const resolve = (over: Partial<{ count: number; motifs: typeof REUSED }> = {}) =>
  (api.motifs as ReturnType<typeof vi.fn>).mockResolvedValue({
    count: 73, health: HEALTH, motifs: [...REUSED, ...ONCE], ...over,
  });

const draw = (onPick = vi.fn()) => {
  render(<ShapesBlock repo="knomit-kb" branch="agent/test" onPick={onPick} />);
  return onPick;
};

describe('ShapesBlock', () => {
  beforeEach(() => vi.clearAllMocks());

  it('ranks by use and offers the rest behind an overflow row', async () => {
    resolve();
    draw();
    await waitFor(() => expect(screen.getAllByTestId('shapes-row').length).toBe(6));
    expect(screen.getAllByTestId('shapes-row')[0].getAttribute('data-motif'))
      .toBe('failure-presents-as-success');
    // Seven reused names, six rows: one behind the overflow. The names used
    // once are not counted here — they are not part of the ranked reading.
    expect(screen.getByTestId('shapes-more')).toHaveTextContent('+1 more');
  });

  it('picking a name goes somewhere, which is what picking means everywhere else', async () => {
    resolve();
    const onPick = draw();
    await waitFor(() => expect(screen.getAllByTestId('shapes-row').length).toBe(6));
    fireEvent.click(screen.getAllByTestId('shapes-row')[0]);
    expect(onPick).toHaveBeenCalledWith('failure-presents-as-success');
  });

  it('folds the names used once into a band, not into rows', async () => {
    resolve();
    draw();
    await waitFor(() => expect(screen.getByTestId('shapes-more')).toBeTruthy());
    fireEvent.click(screen.getByTestId('shapes-more'));
    const band = await screen.findByTestId('shapes-singletons');
    expect(band).toHaveTextContent('2 names used once');
    expect(band).toHaveTextContent('layout-shifts-under-cursor');
    // As text, not as N rows with N empty share bars — that would be the list
    // telling you about itself rather than about the corpus.
    expect(band.querySelectorAll('[data-testid="shapes-row"]')).toHaveLength(0);
  });

  it('shows the meanings once opened — the thing the sibling columns cannot do', async () => {
    resolve();
    draw();
    await waitFor(() => expect(screen.getByTestId('shapes-more')).toBeTruthy());
    fireEvent.click(screen.getByTestId('shapes-more'));
    await waitFor(() => expect(screen.getByTestId('shapes-search')).toBeTruthy());
    expect(screen.getAllByTestId('shapes-row')[0])
      .toHaveTextContent('returns success signals');
  });

  it('promises to search meanings as well as names', async () => {
    resolve();
    draw();
    await waitFor(() => expect(screen.getByTestId('shapes-more')).toBeTruthy());
    fireEvent.click(screen.getByTestId('shapes-more'));
    const box = await screen.findByTestId('shapes-search');
    expect(box.getAttribute('placeholder')).toBe('Search names and meanings…');
    fireEvent.change(box, { target: { value: 'silent' } });
    await waitFor(() => expect(api.motifs).toHaveBeenLastCalledWith(
      'knomit-kb', 'agent/test', expect.objectContaining({ q: 'silent' })));
  });

  it('narrows the block count but NEVER the health figures', async () => {
    resolve();
    draw();
    await waitFor(() => expect(screen.getByTestId('shapes-count').textContent).toBe('73'));
    const healthBefore = screen.getByTestId('shapes-health').textContent;

    fireEvent.click(screen.getByTestId('shapes-more'));
    (api.motifs as ReturnType<typeof vi.fn>).mockResolvedValue({
      count: 3, health: { ...HEALTH, authored_recurring: 1, authored_links: 2 }, motifs: REUSED.slice(0, 3),
    });
    fireEvent.change(await screen.findByTestId('shapes-search'), { target: { value: 'signal' } });

    await waitFor(() => expect(screen.getByTestId('shapes-count').textContent).toBe('3 of 73'));
    // The figures describe the VOCABULARY, not whatever the list happens to be
    // showing — so a narrowing query must not move them, even if the server
    // sends different ones.
    expect(screen.getByTestId('shapes-health').textContent).toBe(healthBefore);
  });

  it('holds its height while asking, and shows no zero', async () => {
    (api.motifs as ReturnType<typeof vi.fn>).mockReturnValue(new Promise(() => {}));
    draw();
    expect(screen.getByTestId('shapes-loading')).toBeTruthy();
    expect(screen.queryByTestId('shapes-empty')).toBeNull();
    expect(screen.getByTestId('shapes-count').textContent).toBe('');
  });

  it('says it could not read the vocabulary, rather than showing an empty one', async () => {
    (api.motifs as ReturnType<typeof vi.fn>).mockRejectedValue(new Error('502'));
    draw();
    // "No names" would claim this corpus has no shared shapes at all — the most
    // misleading thing this surface could say.
    await waitFor(() => expect(screen.getByTestId('shapes-error')).toBeTruthy());
    expect(screen.queryByTestId('shapes-empty')).toBeNull();
    expect(screen.queryAllByTestId('shapes-row')).toHaveLength(0);
  });

  it('offers a way back from a failure', async () => {
    (api.motifs as ReturnType<typeof vi.fn>).mockRejectedValueOnce(new Error('502'));
    draw();
    await waitFor(() => expect(screen.getByTestId('shapes-retry')).toBeTruthy());
    resolve();
    fireEvent.click(screen.getByTestId('shapes-retry'));
    await waitFor(() => expect(screen.getAllByTestId('shapes-row').length).toBe(6));
  });

  it('distinguishes "nothing matched" from "nothing loaded"', async () => {
    resolve({ count: 0, motifs: [] });
    draw();
    await waitFor(() => expect(screen.getByTestId('shapes-empty')).toBeTruthy());
    expect(screen.queryByTestId('shapes-error')).toBeNull();
  });
});

describe('where there is no vocabulary endpoint', () => {
  it('absents itself rather than reporting a fault', () => {
    // The public /explore build vendors these components against a static
    // bundle with no live endpoint. "Unavailable" is a third thing from
    // "failed" and "empty": an error there would report a fault in a build
    // working exactly as intended, and a hole in the panel would be worse.
    const real = api.motifs;
    // @ts-expect-error — modelling the vendored client, which has no such call
    api.motifs = undefined;
    const { container } = render(<ShapesBlock repo="r" branch="b" onPick={vi.fn()} />);
    expect(container.firstChild).toBeNull();
    api.motifs = real;
  });
});
