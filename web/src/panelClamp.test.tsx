// The fact-chrome panels keep their nominal size but never draw past the fact
// pane, which clips them (overflow:hidden) — they shrink to what is visible and
// their list scrolls instead. jsdom has no layout, so every rect here is
// stubbed: these tests pin the ARITHMETIC and the wiring, and the browser pass
// is what shows the panel actually fits.
import { describe, it, expect, vi, afterEach } from 'vitest';
import { createRef } from 'react';
import type { ReactNode } from 'react';
import { render, screen, act } from '@testing-library/react';
import { ConnectionsPanel, CONNECTIONS_PANEL_MAX_HEIGHT } from './ConnectionsPanel';
import { MotifPanel, MOTIF_PANEL_MAX_HEIGHT } from './MotifPanel';
import { PANEL_BOUNDS_ATTR, PANEL_BOUNDS_GUTTER, PANEL_GAP } from './hooks';
import type { RefGroup } from './api';
import type { ResolvedMotif } from './useMotifClusters';

type Box = { left: number; top: number; right: number; bottom: number };

const toRect = (b: Box) => ({
  ...b, x: b.left, y: b.top, width: b.right - b.left, height: b.bottom - b.top, toJSON: () => b,
}) as DOMRect;

// The pane and the span the panel hangs from are the only two rects the clamp
// reads. Everything else answers zero, as jsdom would.
let pane: Box;
let anchor: Box;
const stubRects = () => vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(function (this: Element) {
  if (this.hasAttribute(PANEL_BOUNDS_ATTR)) return toRect(pane);
  if (this.getAttribute('data-testid') === 'anchor') return toRect(anchor);
  return toRect({ left: 0, top: 0, right: 0, bottom: 0 });
});

afterEach(() => vi.restoreAllMocks());

// The real tree: pane (bounds) > edges-row span (the panel's containing block) > panel.
const inPane = (panel: ReactNode) => (
  <div {...{ [PANEL_BOUNDS_ATTR]: '' }}>
    <span data-testid="anchor" style={{ position: 'relative' }}>{panel}</span>
  </div>
);

const group = (i: number): RefGroup => ({
  path: `kb/g${i}.md`, title: `G${i}`, type: 'observation', deleted: false,
  versions: [{ commit: `c${i}`, committed_at: 1, deleted: false }],
});

const connections = (open: 'in' | 'out' | null = 'out') => (
  <ConnectionsPanel id="p" open={open} incoming={[group(1)]} outgoing={[group(2)]} error={null}
    onClose={() => {}} onHop={() => {}} menuRef={createRef<HTMLElement>()}
    onMouseEnter={() => {}} onMouseLeave={() => {}} />
);

const motif: ResolvedMotif = { motif: 'failure-presents-as-success', status: 'loading' };
const motifs = () => (
  <MotifPanel id="m" motifs={[motif]} focused={null} onClose={() => {}} onPivot={() => {}}
    menuRef={createRef<HTMLElement>()} onMouseEnter={() => {}} onMouseLeave={() => {}} />
);

const PANELS = [
  { name: 'ConnectionsPanel', testid: 'connections-panel', list: 'connections-panel-list', el: connections, nominalHeight: CONNECTIONS_PANEL_MAX_HEIGHT },
  { name: 'MotifPanel', testid: 'motif-panel', list: 'motif-panel-list', el: motifs, nominalHeight: MOTIF_PANEL_MAX_HEIGHT },
];

describe.each(PANELS)('$name — clamped to the fact pane', ({ testid, list, el, nominalHeight }) => {
  it('shrinks to the space between its anchor and the pane\'s right and bottom edges', () => {
    // A narrow, short pane: 250px wide from the anchor, 200px tall below it.
    pane = { left: 500, top: 40, right: 800, bottom: 300 };
    anchor = { left: 550, top: 80, right: 700, bottom: 100 };
    stubRects();
    render(inPane(el()));
    const panel = screen.getByTestId(testid);
    expect(panel.style.maxWidth).toBe(`${800 - PANEL_BOUNDS_GUTTER - 550}px`);
    expect(panel.style.maxHeight).toBe(`${300 - PANEL_BOUNDS_GUTTER - (100 + PANEL_GAP)}px`);
  });

  it('keeps its nominal size when the pane has room', () => {
    pane = { left: 500, top: 40, right: 2000, bottom: 1400 };
    anchor = { left: 550, top: 80, right: 700, bottom: 100 };
    stubRects();
    render(inPane(el()));
    const panel = screen.getByTestId(testid);
    expect(panel.style.maxHeight).toBe(`${nominalHeight}px`);
    // maxWidth may be the (large) available width — it must not undercut the nominal width.
    expect(parseFloat(panel.style.maxWidth)).toBeGreaterThan(420);
  });

  it('re-measures when the window resizes', () => {
    pane = { left: 500, top: 40, right: 2000, bottom: 1400 };
    anchor = { left: 550, top: 80, right: 700, bottom: 100 };
    stubRects();
    render(inPane(el()));
    const panel = screen.getByTestId(testid);
    expect(panel.style.maxHeight).toBe(`${nominalHeight}px`);

    pane = { left: 500, top: 40, right: 800, bottom: 300 };
    act(() => { window.dispatchEvent(new Event('resize')); });
    expect(panel.style.maxWidth).toBe(`${800 - PANEL_BOUNDS_GUTTER - 550}px`);
    expect(panel.style.maxHeight).toBe(`${300 - PANEL_BOUNDS_GUTTER - (100 + PANEL_GAP)}px`);
  });

  it('never goes negative when the anchor is already at the pane\'s edge', () => {
    // Both raw differences are negative: 555 - 8 - 550 and 105 - 8 - 106.
    pane = { left: 500, top: 40, right: 555, bottom: 105 };
    anchor = { left: 550, top: 80, right: 700, bottom: 100 };
    stubRects();
    render(inPane(el()));
    const panel = screen.getByTestId(testid);
    expect(panel.style.maxWidth).toBe('0px');
    expect(panel.style.maxHeight).toBe('0px');
  });

  it('is left unclamped, at its nominal height, outside a bounds element', () => {
    pane = { left: 500, top: 40, right: 800, bottom: 300 };
    anchor = { left: 550, top: 80, right: 700, bottom: 100 };
    stubRects();
    render(<span data-testid="anchor" style={{ position: 'relative' }}>{el()}</span>);
    const panel = screen.getByTestId(testid);
    expect(panel.style.maxWidth).toBe('');
    expect(panel.style.maxHeight).toBe(`${nominalHeight}px`);
  });

  // A clamped height is only a scroll, not a cut, if the list is the part that
  // gives: a flex:1 child at min-height:auto grows to its whole content and
  // pushes past the panel (kb gotcha flex-min-content-propagation), and a header
  // that shrinks would take the esc chip and × with it.
  it('scrolls its list, not the header, when clamped', () => {
    pane = { left: 500, top: 40, right: 800, bottom: 300 };
    anchor = { left: 550, top: 80, right: 700, bottom: 100 };
    stubRects();
    render(inPane(el()));
    const panel = screen.getByTestId(testid);
    expect(panel.style.display).toBe('flex');
    expect(panel.style.flexDirection).toBe('column');
    expect(panel.style.overflow).toBe('hidden');
    const body = screen.getByTestId(list);
    expect(body.style.overflowY).toBe('auto');
    expect(body.style.minHeight).toBe('0px');
    expect(body.style.flex).toMatch(/^1/);
    const header = panel.firstElementChild as HTMLElement;
    expect(header).not.toBe(body);
    expect(header.style.flexShrink).toBe('0');
  });
});
