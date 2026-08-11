// The band is the fact's chrome: what it is, where it came from, and what you
// can do to it. It has two states, and the second one is the reason it exists —
// three paragraphs into a long fact the title has scrolled away, taking the
// type, the confidence and every action with it.

import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { FactBand } from './FactBand';
import type { Fact } from './api';

const fact: Fact = {
  path: 'kb/technology/ai/security/b9f5e0ac.md',
  title: 'AI developer toolchain as the dominant new attack surface',
  body: '', domain: [], entities: [], refs: [],
  type: 'synthesis', origin: 'distilled', confidence: 0.85, sources: 1,
};
const lensMeta = { repo: 'core', branch: 'agent/mindev.local-8ef0cd32' };
const actions = <button data-testid="the-actions">actions</button>;

const band = () => screen.getByTestId('fact-band');

describe('FactBand', () => {
  it('opens with everything: type, origin, confidence, sources, mount, branch, path', () => {
    render(<FactBand fact={fact} dispatch={vi.fn()} lensMeta={lensMeta} pinned={false} actions={actions} />);
    const t = band().textContent!;
    for (const part of ['synthesis', 'distilled', '0.85', '1 source', 'core',
      'agent/mindev.local-8ef0cd32', fact.path]) {
      expect(t).toContain(part);
    }
  });

  it('does not repeat the title while the real one is on screen', () => {
    render(<FactBand fact={fact} dispatch={vi.fn()} lensMeta={lensMeta} pinned={false} actions={actions} />);
    expect(screen.queryByTestId('fact-band-title')).toBeNull();
  });

  it('pinned: keeps every value, and adds the title', () => {
    // With the path inline (the jsdom default layout) pinning only ADDS a row.
    // Two earlier cuts took things away instead — one dropped everything but
    // type and confidence, the other dropped the path unconditionally — and a
    // value that vanishes mid-scroll reads as a glitch: you cannot tell whether
    // the fact changed or the panel did. The one case where the path does yield
    // is below, and it yields because the title lands on top of it.
    render(<FactBand fact={fact} dispatch={vi.fn()} lensMeta={lensMeta} pinned actions={actions} />);
    const t = band().textContent!;
    expect(screen.getByTestId('fact-band-title').textContent).toBe(fact.title);
    for (const kept of ['synthesis', 'distilled', '0.85', '1 source', 'core',
      'agent/mindev.local-8ef0cd32', fact.path]) {
      expect(t).toContain(kept);
    }
  });

  it('pinned and unpinned differ by exactly one element: the title', () => {
    const { rerender } = render(
      <FactBand fact={fact} dispatch={vi.fn()} lensMeta={lensMeta} pinned={false} actions={actions} />);
    const open = screen.getByTestId('fact-meta').children.length;
    rerender(<FactBand fact={fact} dispatch={vi.fn()} lensMeta={lensMeta} pinned actions={actions} />);
    // +2: the title and the zero-height break that puts it on its own row.
    expect(screen.getByTestId('fact-meta').children.length).toBe(open + 2);
  });

  it('pinned: the title takes its OWN row, under the values', () => {
    // Not inline among them: a title is long and variable, so sharing the row
    // made the values it sits beside shift as it changed. It goes where the
    // path goes when the row runs out — the line below.
    render(<FactBand fact={fact} dispatch={vi.fn()} lensMeta={lensMeta} pinned actions={actions} />);
    const line = screen.getByTestId('fact-meta');
    const title = screen.getByTestId('fact-band-title');
    // The break before it is a full-width zero-height flex item, which is what
    // makes the wrap deterministic rather than dependent on the row's fullness.
    const brk = title.previousElementSibling as HTMLElement;
    expect(brk.style.flexBasis).toBe('100%');
    expect(brk.style.height).toBe('0px');
    expect(line.lastElementChild).toBe(title);
  });

  it('keeps the actions in both states — they are the point of pinning', () => {
    const { rerender } = render(
      <FactBand fact={fact} dispatch={vi.fn()} lensMeta={lensMeta} pinned={false} actions={actions} />);
    expect(screen.getByTestId('the-actions')).toBeTruthy();
    rerender(<FactBand fact={fact} dispatch={vi.fn()} lensMeta={lensMeta} pinned actions={actions} />);
    expect(screen.getByTestId('the-actions')).toBeTruthy();
  });

  it('wraps the meta, never the actions', () => {
    // When the row runs out of room the path drops to a second line INSIDE the
    // meta group; the actions sit outside it and never move. Aligned to the
    // top, so they stay on the first line rather than centring against two.
    render(<FactBand fact={fact} dispatch={vi.fn()} lensMeta={lensMeta} pinned={false} actions={actions} />);
    expect(band().style.alignItems).toBe('flex-start');

    const meta = screen.getByTestId('fact-band-meta');
    expect(meta.style.flexWrap).toBe('wrap');
    expect(meta.style.minWidth).toBe('0px');

    const act = screen.getByTestId('fact-band-actions');
    // jsdom expands `flex: none` to its longhand.
    expect(act.style.flex).toBe('0 0 auto');
    expect(act.contains(screen.getByTestId('the-actions'))).toBe(true);
    expect(meta.contains(screen.getByTestId('the-actions'))).toBe(false);
  });

  it('shows no mount or branch in a repo context', () => {
    render(<FactBand fact={fact} dispatch={vi.fn()} pinned={false} actions={actions} />);
    expect(screen.queryByTestId('source-badge')).toBeNull();
    expect(screen.queryByTestId('fact-branch')).toBeNull();
    expect(band().textContent).toContain(fact.path);
  });

  // jsdom has no layout: every offsetTop is 0, so the wrapped case cannot
  // happen by itself and has to be staged. Without this the "path yields"
  // branch would never run in the suite at all.
  function withWrappedPath(run: () => void) {
    const proto = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'offsetTop');
    Object.defineProperty(HTMLElement.prototype, 'offsetTop', {
      configurable: true,
      get(this: HTMLElement) {
        // The path is the only mono value that starts with kb/ or src://.
        return /^(kb|src)[:/]/.test(this.textContent || '') ? 30 : 0;
      },
    });
    try { run(); } finally {
      if (proto) Object.defineProperty(HTMLElement.prototype, 'offsetTop', proto);
    }
  }

  it('pinned: the path YIELDS when it had wrapped — the title takes that line', () => {
    // A wrapped path sits exactly where the title is about to go. Keeping both
    // makes a three-row band; the path is the one worth less while reading.
    withWrappedPath(() => {
      const { rerender } = render(
        <FactBand fact={fact} dispatch={vi.fn()} lensMeta={lensMeta} pinned={false} actions={actions} />);
      expect(band().textContent).toContain(fact.path);
      rerender(<FactBand fact={fact} dispatch={vi.fn()} lensMeta={lensMeta} pinned actions={actions} />);
      expect(band().textContent).not.toContain(fact.path);
      expect(screen.getByTestId('fact-band-title')).toBeTruthy();
    });
  });

  it('pinned: the path STAYS when it was inline — nothing is in the way', () => {
    // The default jsdom layout reports every element at the same offsetTop,
    // which is exactly the inline case.
    render(<FactBand fact={fact} dispatch={vi.fn()} lensMeta={lensMeta} pinned actions={actions} />);
    expect(band().textContent).toContain(fact.path);
    expect(screen.getByTestId('fact-band-title')).toBeTruthy();
  });
});
