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
    // Three rows now: values, path, edges. The path moved to a row of its own
    // when the edges row arrived — motif names are long enough that sharing a
    // line with them was never going to hold.
    expect(screen.getByTestId('fact-band-path').textContent).toBe(fact.path);
  });

  it('strips the kb://<id12>/ qualifier from the displayed path', () => {
    // Moved here from FactMetaLine with the path itself. The mount is already
    // named by the source badge on row 1, so repeating its opaque id on row 2
    // spends characters on something the reader has just been told.
    render(<FactBand fact={{ ...fact, path: 'kb://bbbbbbbbbbbb/kb/api/auth.md' }}
      dispatch={vi.fn()} lensMeta={lensMeta} pinned={false} actions={actions} />);
    expect(screen.getByTestId('fact-band-path').textContent).toBe('kb/api/auth.md');
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
    // Every VALUE survives — that is the half of the old rule that still holds.
    for (const kept of ['synthesis', 'distilled', '0.85', '1 source', 'core',
      'agent/mindev.local-8ef0cd32']) {
      expect(t).toContain(kept);
    }
    // The path is what yields, and now it yields unconditionally: with rows
    // explicit, keeping both it and the title always costs a fourth row. The
    // old band measured whether the path had wrapped and let it yield only
    // then; there is nothing left to measure, so the judgement that survived
    // that measurement — the path is worth less than the title while reading —
    // simply applies every time.
    expect(screen.queryByTestId('fact-band-path')).toBeNull();
  });

  it('pinned and unpinned are the same shape — only row 2 changes', () => {
    // The band must not change HEIGHT as a fact scrolls: growing a row
    // mid-scroll shifts the prose under the reader's eye, which is the thing
    // the old wrap-measurement was contorting itself to avoid. Three rows in
    // both states, and the swap is contained to the middle one.
    const { rerender } = render(
      <FactBand fact={fact} dispatch={vi.fn()} lensMeta={lensMeta} pinned={false} actions={actions} />);
    const rows = band().children.length;
    const values = screen.getByTestId('fact-meta').children.length;

    rerender(<FactBand fact={fact} dispatch={vi.fn()} lensMeta={lensMeta} pinned actions={actions} />);
    expect(band().children.length).toBe(rows);
    expect(screen.getByTestId('fact-meta').children.length).toBe(values);
  });

  it('pinned: the title takes its OWN row, under the values', () => {
    // Still true, and now structural rather than coaxed out of a wrapping flex
    // row with a zero-height break. A title is long and variable; sharing the
    // values' row made them shift as it changed.
    render(<FactBand fact={fact} dispatch={vi.fn()} lensMeta={lensMeta} pinned actions={actions} />);
    const title = screen.getByTestId('fact-band-title');
    expect(title.parentElement).toBe(band());
    expect(screen.getByTestId('fact-meta').contains(title)).toBe(false);
    // Row 2 of three: under the values, above the edges.
    expect(band().children[1]).toBe(title);
  });

  it('keeps the actions in both states — they are the point of pinning', () => {
    const { rerender } = render(
      <FactBand fact={fact} dispatch={vi.fn()} lensMeta={lensMeta} pinned={false} actions={actions} />);
    expect(screen.getByTestId('the-actions')).toBeTruthy();
    rerender(<FactBand fact={fact} dispatch={vi.fn()} lensMeta={lensMeta} pinned actions={actions} />);
    expect(screen.getByTestId('the-actions')).toBeTruthy();
  });

  it('wraps the values, and keeps the actions out of them', () => {
    // The band is a column of rows now, so "the path drops to a second line"
    // is no longer a thing that can happen — it has a line. What survives is
    // the part that still matters: the values may wrap among themselves, and
    // the actions are never inside them, so a long value cannot push the
    // retract button somewhere new.
    render(<FactBand fact={fact} dispatch={vi.fn()} lensMeta={lensMeta} pinned={false} actions={actions} />);
    expect(band().style.flexDirection).toBe('column');

    const meta = screen.getByTestId('fact-band-meta');
    expect(meta.style.flexWrap).toBe('wrap');
    expect(meta.style.minWidth).toBe('0px');

    const act = screen.getByTestId('fact-band-actions');
    // jsdom expands `flex: none` to its longhand.
    expect(act.style.flex).toBe('0 0 auto');
    expect(act.contains(screen.getByTestId('the-actions'))).toBe(true);
    expect(meta.contains(screen.getByTestId('the-actions'))).toBe(false);
    // And they live on the edges row, pinned to its right — outside the border
    // that will hold the panel-openers.
    expect(screen.getByTestId('fact-band-edges').contains(act)).toBe(true);
    expect(act.style.marginLeft).toBe('auto');
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
  it('pinned: the path yields every time, not only when it had wrapped', () => {
    // Supersedes "the path STAYS when it was inline". That test described a
    // band where the path shared the values' row, so keeping it cost nothing
    // unless it had already wrapped — a fact the old code established by
    // measuring offsetTop. With the path on a row of its own, keeping it
    // alongside the title always costs a fourth row, so it always yields and
    // the measurement is gone.
    render(<FactBand fact={fact} dispatch={vi.fn()} lensMeta={lensMeta} pinned actions={actions} />);
    expect(band().textContent).not.toContain(fact.path);
    expect(screen.getByTestId('fact-band-title')).toBeTruthy();
  });
});
