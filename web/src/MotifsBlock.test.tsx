import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { MotifsBlock } from './MotifsBlock';
import { api } from './api';

vi.mock('./api', () => ({ api: { motifs: vi.fn(), lensMotifs: vi.fn() } }));

// The endpoint the block reads from. A repo branch for most of the file; the
// lens block below asserts the same behaviour against /lenses/{lens}/motifs.
const REPO = { kind: 'repo', repo: 'knomit-kb', branch: 'agent/test' } as const;

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

const draw = (onPick = vi.fn(), path = '') => {
  render(<MotifsBlock endpoint={REPO} path={path} onPick={onPick} />);
  return onPick;
};

describe('MotifsBlock', () => {
  beforeEach(() => vi.clearAllMocks());

  it('ranks by use and offers the rest behind an overflow row', async () => {
    resolve();
    draw();
    await waitFor(() => expect(screen.getAllByTestId('motifs-row').length).toBe(6));
    expect(screen.getAllByTestId('motifs-row')[0].getAttribute('data-motif'))
      .toBe('failure-presents-as-success');
    // Seven reused names, six rows: one behind the overflow. The names used
    // once are not counted here — they are not part of the ranked reading.
    expect(screen.getByTestId('motifs-more')).toHaveTextContent('+1 more');
  });

  it('picking a name goes somewhere, which is what picking means everywhere else', async () => {
    resolve();
    const onPick = draw();
    await waitFor(() => expect(screen.getAllByTestId('motifs-row').length).toBe(6));
    fireEvent.click(screen.getAllByTestId('motifs-row')[0]);
    expect(onPick).toHaveBeenCalledWith('failure-presents-as-success');
  });

  it('folds the names used once into a band, not into rows', async () => {
    resolve();
    draw();
    await waitFor(() => expect(screen.getByTestId('motifs-more')).toBeTruthy());
    fireEvent.click(screen.getByTestId('motifs-more'));
    const band = await screen.findByTestId('motifs-singletons');
    expect(band).toHaveTextContent('2 names used once');
    expect(band).toHaveTextContent('layout-shifts-under-cursor');
    // As text, not as N rows with N empty share bars — that would be the list
    // telling you about itself rather than about the corpus.
    expect(band.querySelectorAll('[data-testid="motifs-row"]')).toHaveLength(0);
  });

  it('shows the meanings once opened — the thing the sibling columns cannot do', async () => {
    resolve();
    draw();
    await waitFor(() => expect(screen.getByTestId('motifs-more')).toBeTruthy());
    fireEvent.click(screen.getByTestId('motifs-more'));
    await waitFor(() => expect(screen.getByTestId('motifs-search')).toBeTruthy());
    expect(screen.getAllByTestId('motifs-row')[0])
      .toHaveTextContent('returns success signals');
  });

  it('lets a definition wrap rather than cutting it mid-claim', async () => {
    // The sentence the reader opened the browser to read, and one whose point
    // is usually in its second half — "…so callers record it as having worked"
    // IS the motif. Clipping mid-clause is the failure that banned the ellipsis
    // on names, one level up: what survives the cut reads as a complete and
    // different statement.
    resolve();
    draw();
    await waitFor(() => expect(screen.getByTestId('motifs-more')).toBeTruthy());
    fireEvent.click(screen.getByTestId('motifs-more'));
    const def = (await screen.findAllByTestId('motifs-definition'))[0];
    expect(def.style.whiteSpace).not.toBe('nowrap');
    expect(def.style.textOverflow).toBe('');
    expect(def.textContent).toBe(REUSED[0].definition);
  });

  it('promises to search meanings as well as names', async () => {
    resolve();
    draw();
    await waitFor(() => expect(screen.getByTestId('motifs-more')).toBeTruthy());
    fireEvent.click(screen.getByTestId('motifs-more'));
    const box = await screen.findByTestId('motifs-search');
    expect(box.getAttribute('placeholder')).toBe('Search names and meanings…');
    fireEvent.change(box, { target: { value: 'silent' } });
    await waitFor(() => expect(api.motifs).toHaveBeenLastCalledWith(
      'knomit-kb', 'agent/test', expect.objectContaining({ q: 'silent' })));
  });

  it('narrows the block count but NEVER the health figures', async () => {
    resolve();
    draw();
    await waitFor(() => expect(screen.getByTestId('motifs-count').textContent).toBe('73'));
    const healthBefore = screen.getByTestId('motifs-health').textContent;

    fireEvent.click(screen.getByTestId('motifs-more'));
    (api.motifs as ReturnType<typeof vi.fn>).mockResolvedValue({
      count: 3, health: { ...HEALTH, authored_recurring: 1, authored_links: 2 }, motifs: REUSED.slice(0, 3),
    });
    fireEvent.change(await screen.findByTestId('motifs-search'), { target: { value: 'signal' } });

    await waitFor(() => expect(screen.getByTestId('motifs-count').textContent).toBe('3 of 73'));
    // The figures describe the VOCABULARY, not whatever the list happens to be
    // showing — so a narrowing query must not move them, even if the server
    // sends different ones.
    expect(screen.getByTestId('motifs-health').textContent).toBe(healthBefore);
  });

  it('holds its height while asking, and shows no zero', async () => {
    (api.motifs as ReturnType<typeof vi.fn>).mockReturnValue(new Promise(() => {}));
    draw();
    expect(screen.getByTestId('motifs-loading')).toBeTruthy();
    expect(screen.queryByTestId('motifs-empty')).toBeNull();
    expect(screen.getByTestId('motifs-count').textContent).toBe('');
  });

  it('says it could not read the vocabulary, rather than showing an empty one', async () => {
    (api.motifs as ReturnType<typeof vi.fn>).mockRejectedValue(new Error('502'));
    draw();
    // "No names" would claim this corpus has no shared motifs at all — the most
    // misleading thing this surface could say.
    await waitFor(() => expect(screen.getByTestId('motifs-error')).toBeTruthy());
    expect(screen.queryByTestId('motifs-empty')).toBeNull();
    expect(screen.queryAllByTestId('motifs-row')).toHaveLength(0);
  });

  it('offers a way back from a failure', async () => {
    (api.motifs as ReturnType<typeof vi.fn>).mockRejectedValueOnce(new Error('502'));
    draw();
    await waitFor(() => expect(screen.getByTestId('motifs-retry')).toBeTruthy());
    resolve();
    fireEvent.click(screen.getByTestId('motifs-retry'));
    await waitFor(() => expect(screen.getAllByTestId('motifs-row').length).toBe(6));
  });

  // The overflow row opens a BROWSER, exactly as the facet columns' does, and a
  // browser you cannot leave is a mode: `+N more` a few pixels from a `+N more`
  // that behaves differently is the whole reason this is shared grammar.
  it('opens a browser with a way back, not an expansion in place', async () => {
    resolve();
    draw();
    await waitFor(() => expect(screen.getByTestId('motifs-more')).toBeTruthy());
    fireEvent.click(screen.getByTestId('motifs-more'));

    const back = await screen.findByTestId('motifs-back');
    expect(screen.getByTestId('motifs-search')).toBeTruthy();
    // In the browser, every reused name — the overflow row itself is gone,
    // because there is nothing left behind it.
    expect(screen.getAllByTestId('motifs-row')).toHaveLength(REUSED.length);
    expect(screen.queryByTestId('motifs-more')).toBeNull();

    fireEvent.click(back);
    await waitFor(() => expect(screen.getAllByTestId('motifs-row')).toHaveLength(6));
    expect(screen.queryByTestId('motifs-search')).toBeNull();
    expect(screen.getByTestId('motifs-more')).toHaveTextContent('+1 more');
  });

  // Going back drops the search with it: a summary rendered under a query the
  // reader can no longer see would be six rows claiming to be the top six.
  it('leaves the search behind when it closes', async () => {
    resolve();
    draw();
    await waitFor(() => expect(screen.getByTestId('motifs-more')).toBeTruthy());
    fireEvent.click(screen.getByTestId('motifs-more'));
    fireEvent.change(await screen.findByTestId('motifs-search'), { target: { value: 'silent' } });
    await waitFor(() => expect(api.motifs).toHaveBeenLastCalledWith(
      'knomit-kb', 'agent/test', expect.objectContaining({ q: 'silent' })));

    fireEvent.click(screen.getByTestId('motifs-back'));
    await waitFor(() => expect(api.motifs).toHaveBeenLastCalledWith(
      'knomit-kb', 'agent/test', expect.objectContaining({ q: undefined })));
  });

  // The a–z ordering belongs to the browser. The collapsed six claim to be the
  // top six, so a summary sliced from an alphabetical list would show the six
  // alphabetically-first — with every share bar scaled against whatever df
  // happened to come first instead of the maximum.
  it('leaves the sort behind when it closes', async () => {
    resolve();
    draw();
    await waitFor(() => expect(screen.getByTestId('motifs-more')).toBeTruthy());
    fireEvent.click(screen.getByTestId('motifs-more'));
    fireEvent.click(await screen.findByTestId('motifs-sort-name'));
    await waitFor(() => expect(api.motifs).toHaveBeenLastCalledWith(
      'knomit-kb', 'agent/test', expect.objectContaining({ sort: 'name' })));

    fireEvent.click(screen.getByTestId('motifs-back'));
    await waitFor(() => expect(api.motifs).toHaveBeenLastCalledWith(
      'knomit-kb', 'agent/test', expect.objectContaining({ sort: 'df' })));
  });

  // A pick leaves through the same door as ← Back, so the same state goes with
  // it: a collapsed block still narrowed by a search nobody can see, or ordered
  // a–z under bars scaled to the wrong maximum, is the browser leaking into the
  // summary.
  it('drops the search and the sort when a pick closes the browser', async () => {
    resolve();
    draw();
    await waitFor(() => expect(screen.getByTestId('motifs-more')).toBeTruthy());
    fireEvent.click(screen.getByTestId('motifs-more'));
    fireEvent.change(await screen.findByTestId('motifs-search'), { target: { value: 'signal' } });
    fireEvent.click(screen.getByTestId('motifs-sort-name'));
    await waitFor(() => expect(api.motifs).toHaveBeenLastCalledWith(
      'knomit-kb', 'agent/test', expect.objectContaining({ q: 'signal', sort: 'name' })));

    fireEvent.click(screen.getAllByTestId('motifs-row')[0]);
    await waitFor(() => expect(api.motifs).toHaveBeenLastCalledWith(
      'knomit-kb', 'agent/test', expect.objectContaining({ q: undefined, sort: 'df' })));
  });

  // A pick is a move, not a selection: the browser closes behind it the way the
  // facet browser does, so returning to the panel does not find it still open.
  it('closes the browser when a name is picked', async () => {
    resolve();
    const onPick = draw();
    await waitFor(() => expect(screen.getByTestId('motifs-more')).toBeTruthy());
    fireEvent.click(screen.getByTestId('motifs-more'));
    fireEvent.click((await screen.findAllByTestId('motifs-row'))[0]);
    expect(onPick).toHaveBeenCalledWith('failure-presents-as-success');
    await waitFor(() => expect(screen.queryByTestId('motifs-search')).toBeNull());
  });

  // The path is SCOPE — the same thing it is for the columns beside this block.
  // The server drops the clusters no fact here carries; the client's whole job
  // is to send the path at all, and to re-ask when it changes.
  it('asks for the vocabulary of the path it is describing', async () => {
    resolve();
    draw(vi.fn(), 'kb/decisions');
    await waitFor(() => expect(api.motifs).toHaveBeenLastCalledWith(
      'knomit-kb', 'agent/test', expect.objectContaining({ path: 'kb/decisions' })));
  });

  it('re-asks when the reader moves to another folder', async () => {
    resolve();
    const { rerender } = render(
      <MotifsBlock endpoint={REPO} path="kb/decisions" onPick={vi.fn()} />);
    await waitFor(() => expect(api.motifs).toHaveBeenLastCalledWith(
      'knomit-kb', 'agent/test', expect.objectContaining({ path: 'kb/decisions' })));
    rerender(<MotifsBlock endpoint={REPO} path="kb/gotchas" onPick={vi.fn()} />);
    await waitFor(() => expect(api.motifs).toHaveBeenLastCalledWith(
      'knomit-kb', 'agent/test', expect.objectContaining({ path: 'kb/gotchas' })));
  });

  // A folder click mid-search: the dashboard stays mounted, the path prop
  // moves, the query survives. The list is then the new folder's, and health
  // held from the old one must not sit beside it under a tooltip claiming they
  // are one population — better no strip than that strip.
  it('drops health counted over another folder rather than pairing it with the new list', async () => {
    resolve();
    const { rerender } = render(
      <MotifsBlock endpoint={REPO} path="kb/decisions" onPick={vi.fn()} />);
    await waitFor(() => expect(screen.getByTestId('motifs-health')).toBeTruthy());

    fireEvent.click(screen.getByTestId('motifs-more'));
    fireEvent.change(await screen.findByTestId('motifs-search'), { target: { value: 'signal' } });
    await waitFor(() => expect(api.motifs).toHaveBeenLastCalledWith(
      'knomit-kb', 'agent/test', expect.objectContaining({ q: 'signal' })));

    rerender(<MotifsBlock endpoint={REPO} path="kb/gotchas" onPick={vi.fn()} />);
    await waitFor(() => expect(api.motifs).toHaveBeenLastCalledWith(
      'knomit-kb', 'agent/test', expect.objectContaining({ path: 'kb/gotchas', q: 'signal' })));
    await waitFor(() => expect(screen.queryByTestId('motifs-health')).toBeNull());

    // Clearing the search refills the strip from the new folder's response.
    fireEvent.change(screen.getByTestId('motifs-search'), { target: { value: '' } });
    await waitFor(() => expect(screen.getByTestId('motifs-health')).toBeTruthy());
  });

  // The row shows what is HERE, because that is what every count in this panel
  // means. But the pivot it opens drops the path, so where the repo holds more
  // than the folder does, the title says both — the reader should not discover
  // the widening by landing in it.
  it('names both counts when the repo holds more than this folder', async () => {
    (api.motifs as ReturnType<typeof vi.fn>).mockResolvedValue({
      count: 2, health: HEALTH, motifs: [
        { ...entry('failure-presents-as-success', 3), df_total: 26 },
        { ...entry('check-then-act-race', 2), df_total: 2 },
      ],
    });
    draw(vi.fn(), 'kb/decisions');
    const rows = await screen.findAllByTestId('motifs-row');
    expect(rows[0].getAttribute('title'))
      .toBe('Open the facts that share this motif — 3 here, 26 in the repo');
    // Equal counts say it once. Two numbers where there is one fact would read
    // as a narrowing that did not happen.
    expect(rows[1].getAttribute('title'))
      .toBe('Open the facts that share this motif — 2 of them');
  });

  // The bug this pins: a folder holding one fact, whose one motif is used once
  // in the whole repo. The block counted it and then folded it into a band the
  // summary never draws — "Motifs 1" over empty space, with no overflow row to
  // open because there was no seventh ranked name to open it for.
  it('never shows a count with nothing under it', async () => {
    (api.motifs as ReturnType<typeof vi.fn>).mockResolvedValue({
      count: 1, health: HEALTH,
      motifs: [{ ...entry('coverage-stops-at-boundary', 1), df_total: 1 }],
    });
    draw(vi.fn(), 'kb/invariants/ai/governance');

    const band = await screen.findByTestId('motifs-singletons');
    expect(band).toHaveTextContent('coverage-stops-at-boundary');
    // Singular. "1 names used once" is the grammar carrierLabel exists to make
    // unrepresentable one surface over; a path scope makes this the ordinary
    // case rather than the rare one.
    expect(band).toHaveTextContent('1 name used once');
    expect(screen.getByTestId('motifs-count').textContent).toBe('1');
  });

  // The fold means "minted once and never reused", which is a fact about the
  // REPO. Under a path, df=1 means "one fact here carries it" — folding on that
  // would hide a motif carried by twenty-six facts because this folder holds
  // one of them.
  it('folds by the repo-wide count, not by the scoped one', async () => {
    (api.motifs as ReturnType<typeof vi.fn>).mockResolvedValue({
      count: 2, health: HEALTH,
      motifs: [
        { ...entry('failure-presents-as-success', 1), df_total: 26 },
        { ...entry('sentinel-never-occurs', 1), df_total: 1 },
      ],
    });
    draw(vi.fn(), 'kb/invariants');

    const rows = await screen.findAllByTestId('motifs-row');
    expect(rows).toHaveLength(1);
    expect(rows[0].getAttribute('data-motif')).toBe('failure-presents-as-success');
    // The repo singleton is the only one folded.
    const band = screen.getByTestId('motifs-singletons');
    expect(band).toHaveTextContent('sentinel-never-occurs');
    expect(band).not.toHaveTextContent('failure-presents-as-success');
  });

  // Unscoped, the band stays a browse-mode fold: the overflow row is there to
  // open it, and drawing 36 names under the six would be the list telling you
  // about itself instead of about the corpus.
  it('keeps the band behind the browser while the overflow row can open it', async () => {
    resolve();
    draw();
    await waitFor(() => expect(screen.getAllByTestId('motifs-row')).toHaveLength(6));
    expect(screen.queryByTestId('motifs-singletons')).toBeNull();
  });

  it('distinguishes "nothing matched" from "nothing loaded"', async () => {
    resolve({ count: 0, motifs: [] });
    draw();
    await waitFor(() => expect(screen.getByTestId('motifs-empty')).toBeTruthy());
    expect(screen.queryByTestId('motifs-error')).toBeNull();
  });
});

// The v1 block was repo-only: there was no single vocabulary across a lens to
// show, so it was absent there. /lenses/{lens}/motifs merges every mount's
// clusters into one, and the block reads it through the same props — the point
// of these three is that NOTHING about the block itself is lens-aware.
describe('in a lens', () => {
  const LENS = { kind: 'lens', lens: 'eng' } as const;

  beforeEach(() => vi.clearAllMocks());

  it('reads the lens vocabulary and never the write repo', async () => {
    (api.lensMotifs as ReturnType<typeof vi.fn>).mockResolvedValue({
      count: 73, health: HEALTH, motifs: [...REUSED, ...ONCE],
    });
    render(<MotifsBlock endpoint={LENS} path="kb/decisions" onPick={vi.fn()} />);
    await waitFor(() => expect(api.lensMotifs).toHaveBeenLastCalledWith(
      'eng', expect.objectContaining({ path: 'kb/decisions' })));
    // Not a fallback to one mount: a write-repo vocabulary shown in a lens
    // would be one mount's names presented as the union's.
    expect(api.motifs).not.toHaveBeenCalled();
    expect(await screen.findByText('failure-presents-as-success')).toBeTruthy();
  });

  it('pivots on a pick exactly as it does in a repo', async () => {
    (api.lensMotifs as ReturnType<typeof vi.fn>).mockResolvedValue({
      count: 73, health: HEALTH, motifs: [...REUSED, ...ONCE],
    });
    const onPick = vi.fn();
    render(<MotifsBlock endpoint={LENS} path="" onPick={onPick} />);
    fireEvent.click(await screen.findByText('failure-presents-as-success'));
    expect(onPick).toHaveBeenCalledWith('failure-presents-as-success');
  });

  it('absents itself where the lens vocabulary cannot be read', () => {
    const real = api.lensMotifs;
    // @ts-expect-error — modelling the vendored client, which has no such call
    api.lensMotifs = undefined;
    const { container } = render(<MotifsBlock endpoint={LENS} path="" onPick={vi.fn()} />);
    expect(container.firstChild).toBeNull();
    api.lensMotifs = real;
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
    const { container } = render(<MotifsBlock endpoint={{ kind: 'repo', repo: 'r', branch: 'b' }} path="" onPick={vi.fn()} />);
    expect(container.firstChild).toBeNull();
    api.motifs = real;
  });
});
