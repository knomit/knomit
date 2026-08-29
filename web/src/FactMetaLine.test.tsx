// One line carries everything the old header stacked in four rows and three
// visual languages: two chips, two 70px boxes, a mount badge and a path.

import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { FactMetaLine } from './FactMetaLine';
import { repoHue, typeStyles } from './utils';
import type { Fact } from './api';

const fact: Fact = {
  path: 'kb/technology/ai/agents/ca7ef5ab.md',
  title: 'T', body: '', domain: [], entities: [], refs: [],
  type: 'synthesis', origin: 'distilled', confidence: 0.6, sources: 1,
};

const line = () => screen.getByTestId('fact-meta');

describe('FactMetaLine', () => {
  it('says everything the four stacked rows said, on one line', () => {
    render(<FactMetaLine fact={fact} dispatch={vi.fn()} />);
    const t = line().textContent!;
    // The path is no longer among them: it has a row of its own in FactBand
    // since the edges row arrived, and this component is now row 1 alone.
    for (const part of ['synthesis', 'distilled', '0.60', '1']) {
      expect(t).toContain(part);
    }
  });

  it('gives the type its own colour and its icon', () => {
    // TypeIcon (the SVG), not the typeStyles text glyph — the same mark a
    // Library row wears, so a type looks like itself in both places.
    render(<FactMetaLine fact={fact} dispatch={vi.fn()} />);
    const badge = screen.getByTestId('fact-type-badge');
    expect(badge.style.color).toBe(hexToRgb(typeStyles.synthesis.color));
    expect(badge.querySelector('svg')).toBeTruthy();
    expect(badge.querySelector('svg')!.getAttribute('stroke')).toBe(typeStyles.synthesis.color);
  });

  it('boxes no type, not even hypothesis', () => {
    // hypothesis used to carry a dashed border, marking it as a claim about the
    // future. On a flat line that reads as a leftover chip, and the meaning is
    // already carried by the word, the colour and the icon.
    render(<FactMetaLine fact={{ ...fact, type: 'hypothesis' }} dispatch={vi.fn()} />);
    const badge = screen.getByTestId('fact-type-badge');
    expect(badge.style.border).toBe('');
    expect(badge.textContent).toContain('hypothesis');
    expect(badge.style.color).toBe(hexToRgb(typeStyles.hypothesis.color));
  });

  it('counts sources in words, since a bare number has no label to lean on', () => {
    // The boxes could say "1" under SOURCES. On one line the label has to be
    // part of the phrase or the number means nothing.
    const { rerender } = render(<FactMetaLine fact={fact} dispatch={vi.fn()} />);
    expect(line().textContent).toContain('1 source');
    rerender(<FactMetaLine fact={{ ...fact, sources: 4 }} dispatch={vi.fn()} />);
    expect(line().textContent).toContain('4 sources');
  });

  it('keeps origin clickable as a filter — filtering is not editing', () => {
    // Deliberately NOT gated on a read-only fact: see TagCloud. Asking which
    // other facts were distilled is a question, not a write.
    const dispatch = vi.fn();
    render(<FactMetaLine fact={fact} dispatch={dispatch} />);
    fireEvent.click(screen.getByTestId('fact-origin-badge'));
    expect(dispatch).toHaveBeenCalledWith({
      type: 'ADD_FILTER', chip: { category: 'origin', value: 'distilled' },
    });
  });

  // The other half of that rule, and a different question. Read-only is about
  // WRITES, and filtering is not one. `filterable` is about the BAR: while the
  // view is anchored, FilterBar renders the trail breadcrumb instead of the chip
  // row, so a chip minted here would be invisible, unremovable, and waiting to
  // narrow the list the moment the reader pressed `h` to return to live.
  it('does not filter by origin while the view is anchored', () => {
    const dispatch = vi.fn();
    render(<FactMetaLine fact={fact} dispatch={dispatch} filterable={false} />);
    fireEvent.click(screen.getByTestId('fact-origin-badge'));
    expect(dispatch).not.toHaveBeenCalled();
  });

  it('stops looking clickable when it is not', () => {
    // A control that silently does nothing is the failure this replaced, not a
    // fix for it: the cursor has to stop promising.
    const { rerender } = render(<FactMetaLine fact={fact} dispatch={vi.fn()} />);
    expect(screen.getByTestId('fact-origin-badge').style.cursor).toBe('pointer');
    rerender(<FactMetaLine fact={fact} dispatch={vi.fn()} filterable={false} />);
    expect(screen.getByTestId('fact-origin-badge').style.cursor).toBe('default');
  });

  it('marks a pragmatic fact, and says nothing for an epistemic one', () => {
    const { rerender } = render(
      <FactMetaLine fact={{ ...fact, kind: 'pragmatic' }} dispatch={vi.fn()} />);
    expect(screen.getByTestId('fact-kind-badge')).toBeTruthy();
    rerender(<FactMetaLine fact={{ ...fact, kind: 'epistemic' }} dispatch={vi.fn()} />);
    expect(screen.queryByTestId('fact-kind-badge')).toBeNull();
  });

  it('shows no mount in a repo context — there is only one repo to be in', () => {
    render(<FactMetaLine fact={fact} dispatch={vi.fn()} />);
    expect(screen.queryByTestId('source-badge')).toBeNull();
    expect(screen.queryByTestId('fact-branch')).toBeNull();
  });

  it('names the mount and its branch in a lens, in the dashboard\'s own treatment', () => {
    // A hue dot and a plain mono name — what the summary's Repo rows use. NOT
    // the bordered, filled pill this header used to draw, which was a third
    // treatment for a thing the app already had two of.
    render(<FactMetaLine fact={fact} dispatch={vi.fn()}
      lensMeta={{ repo: 'agentic-engineering', branch: 'agent/mindev.local-8ef0cd32' }} />);
    const badge = screen.getByTestId('source-badge');
    expect(badge.getAttribute('data-repo')).toBe('agentic-engineering');
    expect(badge.textContent).toContain('agentic-engineering');
    expect(badge.style.background).toBe('');
    expect(badge.style.border).toBe('');
    const dot = badge.querySelector('span[aria-hidden]') as HTMLElement;
    expect(dot.style.background).toBe(hexToRgb(repoHue('agentic-engineering')));
  });

  it('keeps the branch, which a lens shows nowhere else', () => {
    // The top bar names the lens, its mount count and its write target — never
    // a READ mount's branch. This line is the only place it appears.
    render(<FactMetaLine fact={fact} dispatch={vi.fn()}
      lensMeta={{ repo: 'agentic-engineering', branch: 'agent/mindev.local-8ef0cd32' }} />);
    expect(screen.getByTestId('fact-branch').textContent).toContain('agent/mindev.local-8ef0cd32');
  });


  it('omits what a fact does not have rather than printing a gap', () => {
    render(<FactMetaLine
      fact={{ ...fact, type: undefined, origin: undefined, confidence: 0, sources: 0 }}
      dispatch={vi.fn()} />);
    expect(screen.queryByTestId('fact-type-badge')).toBeNull();
    expect(screen.queryByTestId('fact-origin-badge')).toBeNull();
    // confidence 0 is a VALUE (a fact nobody trusts), so it still prints; the
    // parts that are genuinely absent are the ones that must not.
    expect(line().textContent).toContain('conf 0.00');
    expect(line().textContent).not.toContain('source');
    // …and never a stray separator at either end.
    expect(line().textContent!.trim().startsWith('·')).toBe(false);
    expect(line().textContent!.trim().endsWith('·')).toBe(false);
  });
});

function hexToRgb(hex: string): string {
  const h = hex.length === 4 ? '#' + [...hex.slice(1)].map(c => c + c).join('') : hex;
  const [r, g, b] = [1, 3, 5].map(i => parseInt(h.slice(i, i + 2), 16));
  return `rgb(${r}, ${g}, ${b})`;
}
