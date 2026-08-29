// The widen control: rungs that say what they would add.
//
// On a young vocabulary almost every looser rung adds nothing — this repo's
// knowledge base has 73 clusters and not one judge merge, so `stem` cannot
// differ from `exact` for any motif in it. A three-way control that visibly
// does nothing twice reads as broken rather than as an answer about the corpus.
// So each rung measures its own delta and goes inert when it has none, which is
// the rule the zero connection counter already follows.
//
// Nothing here is hard-coded. Emptiness is a property of THIS vocabulary today
// and stops being true the moment two spellings are merged; baking it in would
// be a corpus property frozen as a constant, which this project forbids.

import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { MotifTiers, stemCanAdd, admittedBy } from './MotifTiers';
import type { MotifCluster } from './api';

const cluster = (aliases: MotifCluster['aliases']): MotifCluster => ({
  cluster_key: 'k', canonical: 'c', members: ['c'], df: 5, carrier_count: 5,
  carriers: [], aliases,
});

describe('stemCanAdd', () => {
  it('is false with no judge merge — provably, without asking the server', () => {
    // exact matches the whole cluster; stem matches the mechanical stemmed-token
    // key, which IS how the cluster was formed. The only way they can differ is
    // a cluster a judge merged out of two mechanical groups.
    expect(stemCanAdd(cluster([{ motif: 'c', method: 'canonical' }]))).toBe(false);
  });

  it('is true when a judge merged a spelling in', () => {
    expect(stemCanAdd(cluster([
      { motif: 'c', method: 'canonical' },
      { motif: 'other', method: 'judge', rationale: 'same mechanism' },
    ]))).toBe(true);
  });

  it('is false when there is no cluster to read yet', () => {
    expect(stemCanAdd(undefined)).toBe(false);
  });
});

describe('MotifTiers', () => {
  const draw = (over: Partial<Parameters<typeof MotifTiers>[0]> = {}) => {
    const onPick = vi.fn();
    render(<MotifTiers active="exact" exactCount={26} stemDelta={0} tokenDelta={0}
      onPick={onPick} {...over} />);
    return onPick;
  };

  it('draws a rung that would add nothing as present but dead', () => {
    const onPick = draw();
    const stem = screen.getByTestId('motif-tier-stem') as HTMLButtonElement;
    expect(stem.disabled).toBe(true);
    expect(stem).toHaveTextContent('—');
    // Present, not hidden: a dead rung is a statement about the vocabulary —
    // nothing else in this base is even close to that name — and hiding it
    // would throw that away and reflow the control between motifs.
    expect(stem).toBeTruthy();
    fireEvent.click(stem);
    expect(onPick).not.toHaveBeenCalled();
  });

  it('draws a rung that would add rows as live, with its delta', () => {
    const onPick = draw({ tokenDelta: 2 });
    const t2 = screen.getByTestId('motif-tier-token-2') as HTMLButtonElement;
    expect(t2.disabled).toBe(false);
    expect(t2).toHaveTextContent('+2');
    fireEvent.click(t2);
    expect(onPick).toHaveBeenCalledWith('token-2');
  });

  it('keeps an unmeasured rung live — a rung is not dead until something looked', () => {
    draw({ tokenDelta: null });
    const t2 = screen.getByTestId('motif-tier-token-2') as HTMLButtonElement;
    expect(t2.disabled).toBe(false);
    expect(t2.getAttribute('data-live')).toBe('true');
  });

  it('shows the current tier with its own count, not a delta', () => {
    draw();
    const exact = screen.getByTestId('motif-tier-exact');
    expect(exact.getAttribute('data-active')).toBe('true');
    expect(exact).toHaveTextContent('26');
    expect(exact).not.toHaveTextContent('+26');
  });

  it('says the list contains near matches only while widened', () => {
    const { rerender } = render(<MotifTiers active="exact" exactCount={26} stemDelta={0}
      tokenDelta={2} onPick={vi.fn()} />);
    expect(screen.queryByTestId('motif-tiers-note')).toBeNull();
    rerender(<MotifTiers active="token-2" exactCount={26} stemDelta={0} tokenDelta={2} onPick={vi.fn()} />);
    expect(screen.getByTestId('motif-tiers-note')).toHaveTextContent('near matches');
  });

  it('leaves the active rung clickable-looking but does nothing on re-pick', () => {
    // The reducer no-ops an unchanged tier so the history is not buried under
    // identical entries; the control simply does not offer it as a move.
    const onPick = draw();
    fireEvent.click(screen.getByTestId('motif-tier-exact'));
    expect(onPick).toHaveBeenCalledWith('exact');
  });
});

describe('admittedBy', () => {
  it('names the spelling that let a non-carrier row in', () => {
    expect(admittedBy(['silence-cannot-signal-recovery'], ['signal-cannot-distinguish-cases']))
      .toBe('silence-cannot-signal-recovery');
  });

  it('returns null for a real carrier, whatever the spelling’s case', () => {
    expect(admittedBy(['Signal-Cannot-Distinguish-Cases'], ['signal-cannot-distinguish-cases'])).toBeNull();
  });

  it('returns null for a row carrying one of several cluster members', () => {
    expect(admittedBy(['restraint-without-reciprocity'],
      ['defection-collapses-restraint', 'restraint-without-reciprocity'])).toBeNull();
  });

  it('returns null when the row carries no motifs at all', () => {
    // Nothing to name, so nothing is claimed — an unmarked row rather than a
    // row marked as admitted by "undefined".
    expect(admittedBy(undefined, ['a'])).toBeNull();
    expect(admittedBy([], ['a'])).toBeNull();
  });
});
