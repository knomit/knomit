// Ordering the row's motifs, and what happens to the ones it cannot draw.

import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { orderMotifs, MotifOverflowCell, OVERFLOW, MAX_SHOWN_MOTIFS } from './MotifRow';
import type { ResolvedMotif } from './useMotifClusters';

const ok = (motif: string, carrier_count: number): ResolvedMotif => ({
  motif, status: 'ok',
  cluster: { cluster_key: 'k', canonical: motif, members: [motif], df: carrier_count, carrier_count, carriers: [], aliases: [] },
});
const pending = (motif: string): ResolvedMotif => ({ motif, status: 'loading' });
const failed = (motif: string): ResolvedMotif => ({ motif, status: 'error', error: 'boom' });

describe('orderMotifs', () => {
  it('puts the most-carried motif first, whatever order the fact listed them', () => {
    // Frontmatter order is authoring order and means nothing; the panel sorts
    // the same way, so the row and the panel show one list in one order.
    const r = orderMotifs([ok('rare', 3), ok('common', 26), ok('middling', 9)]);
    expect(r.shown.map(m => m.motif)).toEqual(['common', 'middling']);
    expect(r.hidden.map(m => m.motif)).toEqual(['rare']);
  });

  it('breaks ties by name so two renders cannot disagree', () => {
    const a = orderMotifs([ok('zebra', 5), ok('alpha', 5)]);
    expect(a.shown.map(m => m.motif)).toEqual(['alpha', 'zebra']);
  });

  it('sorts an unresolved count last — unknown is not the same as small', () => {
    // A transient failure must not quietly demote the most-used motif on the
    // fact to the overflow count, where its name would disappear entirely.
    const r = orderMotifs([pending('waiting'), ok('known', 1), failed('broken')]);
    expect(r.shown[0].motif).toBe('known');
    expect(r.shown.length + r.hidden.length).toBe(3);
    expect(r.hidden.some(m => m.motif === 'waiting' || m.motif === 'broken')).toBe(true);
  });

  it('shows everything when the fact carries no more than the row can hold', () => {
    const r = orderMotifs([ok('a', 2), ok('b', 1)]);
    expect(r.shown).toHaveLength(2);
    expect(r.hidden).toEqual([]);
    expect(MAX_SHOWN_MOTIFS).toBe(2);
  });

  it('is empty for a fact with no motifs', () => {
    expect(orderMotifs([])).toEqual({ shown: [], hidden: [] });
  });
});

describe('MotifOverflowCell', () => {
  it('counts what it hid and names it on hover, without ever cutting a name', () => {
    render(<MotifOverflowCell hidden={[ok('check-then-act-race', 9), ok('absence-encodes-value', 7)]}
      open={false} onToggle={vi.fn()} panelId="p" />);
    const cell = screen.getByTestId('motif-overflow');
    expect(cell).toHaveTextContent('+2');
    // The names are already in hand — they came with the fact — so a reader can
    // learn what is behind the count without opening anything. And they appear
    // whole here, because half a motif name asserts something else.
    const title = cell.getAttribute('title')!;
    expect(title).toContain('check-then-act-race');
    expect(title).toContain('absence-encodes-value');
    expect(title).not.toContain('…');
  });

  it('is a button that opens the same panel, not a label', () => {
    // The bordered group holds only controls; a count you cannot act on would
    // be the one dead thing in it.
    const onToggle = vi.fn();
    render(<MotifOverflowCell hidden={[ok('a', 1)]} open={false} onToggle={onToggle} panelId="p" />);
    const cell = screen.getByTestId('motif-overflow');
    expect(cell.tagName).toBe('BUTTON');
    fireEvent.click(cell);
    expect(onToggle).toHaveBeenCalledWith(OVERFLOW);
  });

  it('wears the open marker on its bottom edge like every other cell', () => {
    render(<MotifOverflowCell hidden={[ok('a', 1)]} open onToggle={vi.fn()} panelId="p" />);
    expect(screen.getByTestId('motif-overflow').style.boxShadow).toContain('currentColor');
  });
});
