import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { createRef } from 'react';
import { MotifPanel } from './MotifPanel';
import type { ResolvedMotif } from './useMotifClusters';
import type { MotifCluster } from './api';

const cluster = (over: Partial<MotifCluster> & { canonical: string; carrier_count: number }): MotifCluster => ({
  cluster_key: `key-${over.canonical}`, members: [over.canonical], df: over.carrier_count,
  carriers: [], aliases: [{ motif: over.canonical, method: 'canonical' }], ...over,
});

const ok = (motif: string, c: Partial<MotifCluster> & { carrier_count: number }): ResolvedMotif => ({
  motif, status: 'ok', cluster: cluster({ canonical: motif, ...c }),
});

const panel = (motifs: ResolvedMotif[], focused: string | null, onPivot = vi.fn()) => {
  render(<MotifPanel motifs={motifs} focused={focused} onClose={vi.fn()} onPivot={onPivot}
    menuRef={createRef<HTMLElement>()} onMouseEnter={vi.fn()} onMouseLeave={vi.fn()} id="p" />);
  return onPivot;
};

const FPS = ok('failure-presents-as-success', {
  carrier_count: 26,
  definition: 'An operation that did not achieve its effect returns the same signals a successful one would.',
  definition_state: 'current',
  carriers: [
    { path: 'kb/gotchas/store/testing/a.md', title: 'Limit 0 discards every row', type: 'observation', committed_at: 3 },
    { path: 'kb/invariants/build/b.md', title: 'Verify before the atomic rename', type: 'principle', committed_at: 2 },
    { path: 'kb/gotchas/desktop/c.md', title: 'Unsigned app refuses banners', type: 'observation', committed_at: 1 },
  ],
});

describe('MotifPanel', () => {
  it('expands the motif that was clicked and collapses the others', () => {
    panel([FPS, ok('absence-encodes-value', { carrier_count: 7 })], 'absence-encodes-value');
    const sections = screen.getAllByTestId('motif-section');
    expect(sections.map(s => s.getAttribute('data-expanded'))).toEqual(['false', 'true']);
    // Marked down the left edge, so the panel reads as one list with a focus
    // rather than as two different things.
    const open = sections[1];
    expect(open.style.boxShadow).toContain('inset 3px 0 0');
  });

  it('opens on the first motif when nothing in particular was clicked', () => {
    // The +N route: the caller sorted by carrier count, so the first is the
    // most-carried and the best default.
    panel([FPS, ok('rare', { carrier_count: 1 })], null);
    expect(screen.getAllByTestId('motif-section')[0].getAttribute('data-expanded')).toBe('true');
  });

  it('shows the meaning, the count and three carriers', () => {
    panel([FPS], 'failure-presents-as-success');
    expect(screen.getByTestId('motif-definition').textContent).toContain('same signals a successful one would');
    // carrier_count, the number the pivot will actually land on.
    expect(screen.getByTestId('motif-carrier-count').textContent).toBe('26 carriers');
    expect(screen.getAllByTestId('motif-carrier')).toHaveLength(3);
  });

  it('names the areas the other facts are about, and says the sample is partial', () => {
    panel([FPS], 'failure-presents-as-success');
    const subjects = screen.getByTestId('motif-subjects').textContent!;
    // Path segment 3, not the domain field.
    expect(subjects).toContain('store');
    expect(subjects).toContain('build');
    expect(subjects).toContain('desktop');
    // Three carriers of twenty-six: the line can only understate, and says so.
    expect(subjects).toContain('of the first 3');
  });

  it('marks an interim definition in lowercase, never as a warning', () => {
    panel([ok('config-drift', {
      carrier_count: 14, definition: 'Configured state diverges from applied state.',
      definition_state: 'stale',
    })], 'config-drift');
    const note = screen.getByTestId('motif-definition-state');
    expect(note.textContent).toContain('interim');
    expect(note.textContent).toBe(note.textContent!.toLowerCase());
    // A living vocabulary, not a fault: no warning colour anywhere near it.
    expect(note.style.color).not.toBe('rgb(224, 160, 160)');
    // And the sentence is still shown — a stale definition is the best
    // description anyone has, not something to withhold.
    expect(screen.getByTestId('motif-definition')).toBeTruthy();
  });

  it('leaves no hole when there is no definition at all', () => {
    panel([ok('never-defined', { carrier_count: 2 })], 'never-defined');
    expect(screen.queryByTestId('motif-definition')).toBeNull();
    expect(screen.queryByTestId('motif-definition-state')).toBeNull();
    // The rest of the section is still there — an absent sentence is not a
    // broken cluster.
    expect(screen.getByTestId('motif-pivot')).toBeTruthy();
  });

  it('does not pivot until the pivot button is pressed', () => {
    // The first click inspected; nothing irreversible may happen on a click
    // made to find out what something was.
    const onPivot = panel([FPS], 'failure-presents-as-success');
    fireEvent.click(screen.getByTestId('motif-definition'));
    fireEvent.click(screen.getAllByTestId('motif-carrier')[0]);
    expect(onPivot).not.toHaveBeenCalled();

    fireEvent.click(screen.getByTestId('motif-pivot'));
    expect(onPivot).toHaveBeenCalledWith('failure-presents-as-success');
  });

  it('says how many facts the pivot will open, on the button itself', () => {
    panel([FPS], 'failure-presents-as-success');
    expect(screen.getByTestId('motif-pivot').textContent).toContain('26');
  });

  it('offers the spellings, and the judge’s reason for merging them', () => {
    panel([ok('defection-collapses-restraint', {
      carrier_count: 6,
      members: ['defection-collapses-restraint', 'restraint-without-reciprocity'],
      aliases: [
        { motif: 'defection-collapses-restraint', method: 'canonical' },
        { motif: 'restraint-without-reciprocity', method: 'judge', rationale: 'Both name the same collapse.' },
      ],
    })], 'defection-collapses-restraint');

    expect(screen.getByTestId('motif-spellings').textContent).toBe('2 spellings');
    // One click away, never in the way: an alias raises exactly one question
    // and the written answer exists.
    expect(screen.queryByTestId('motif-aliases')).toBeNull();
    fireEvent.click(screen.getByTestId('motif-spellings'));
    expect(screen.getByTestId('motif-rationale').textContent).toContain('Both name the same collapse');
  });

  it('offers nothing to open when there is only one spelling', () => {
    panel([FPS], 'failure-presents-as-success');
    const b = screen.getByTestId('motif-spellings') as HTMLButtonElement;
    expect(b.textContent).toBe('1 spelling');
    expect(b.disabled).toBe(true);
  });

  it('says a cluster could not be read, rather than showing it as empty', () => {
    panel([{ motif: 'broken', status: 'error', error: 'HTTP 502' }], 'broken');
    expect(screen.getByTestId('motif-error')).toBeTruthy();
    expect(screen.getByTestId('motif-carrier-count').textContent).toBe('count unavailable');
    // No pivot button: there is nothing to promise.
    expect(screen.queryByTestId('motif-pivot')).toBeNull();
  });
});
