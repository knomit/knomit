// The fact-chrome panels keep their nominal size but never draw past the fact
// pane, which clips them (overflow:hidden) — they shrink to what is visible and
// their list scrolls instead. jsdom has no layout, so every rect here is
// stubbed: these tests pin the ARITHMETIC and the wiring, and the browser pass
// is what shows the panel actually fits.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
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

// jsdom has no ResizeObserver, and it is the production path: the rail drag
// resizes the pane without resizing the window. A fake that records what it
// observes and fires on demand, so both the observer and the window fallback
// are exercised — the fallback by removing this global.
class FakeResizeObserver {
  static instances: FakeResizeObserver[] = [];
  observed: Element[] = [];
  disconnected = false;
  private cb: ResizeObserverCallback;
  constructor(cb: ResizeObserverCallback) { this.cb = cb; FakeResizeObserver.instances.push(this); }
  observe(el: Element) { this.observed.push(el); }
  unobserve(el: Element) { this.observed = this.observed.filter(e => e !== el); }
  disconnect() { this.disconnected = true; this.observed = []; }
  fire() { this.cb([], this as unknown as ResizeObserver); }
}
const liveObservers = () => FakeResizeObserver.instances.filter(o => !o.disconnected);

beforeEach(() => {
  FakeResizeObserver.instances = [];
  vi.stubGlobal('ResizeObserver', FakeResizeObserver);
});
afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

// The real tree: pane (bounds) > edges-row span (the panel's containing block) > panel.
const inPane = (panel: ReactNode) => (
  <div data-testid="bounds" {...{ [PANEL_BOUNDS_ATTR]: '' }}>
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
  { name: 'ConnectionsPanel', testid: 'connections-panel', list: 'connections-panel-list', el: () => connections(), nominalHeight: CONNECTIONS_PANEL_MAX_HEIGHT },
  { name: 'MotifPanel', testid: 'motif-panel', list: 'motif-panel-list', el: motifs, nominalHeight: MOTIF_PANEL_MAX_HEIGHT },
];

// A roomy pane and a small one, both with the anchor at (550, 80)-(700, 100).
const ROOMY: Box = { left: 500, top: 40, right: 2000, bottom: 1400 };
const SMALL: Box = { left: 500, top: 40, right: 800, bottom: 300 };
const ANCHOR: Box = { left: 550, top: 80, right: 700, bottom: 100 };
const clampedW = (p: Box, a: Box) => `${p.right - PANEL_BOUNDS_GUTTER - a.left}px`;
const clampedH = (p: Box, a: Box) => `${p.bottom - PANEL_BOUNDS_GUTTER - (a.bottom + PANEL_GAP)}px`;

describe.each(PANELS)('$name — clamped to the fact pane', ({ testid, list, el, nominalHeight }) => {
  it('shrinks to the space between its anchor and the pane\'s right and bottom edges', () => {
    pane = SMALL; anchor = ANCHOR;
    stubRects();
    render(inPane(el()));
    const panel = screen.getByTestId(testid);
    expect(panel.style.maxWidth).toBe(clampedW(SMALL, ANCHOR));
    expect(panel.style.maxHeight).toBe(clampedH(SMALL, ANCHOR));
  });

  it('keeps its nominal size when the pane has room', () => {
    pane = ROOMY; anchor = ANCHOR;
    stubRects();
    render(inPane(el()));
    const panel = screen.getByTestId(testid);
    expect(panel.style.maxHeight).toBe(`${nominalHeight}px`);
    // maxWidth may be the (large) available width — it must not undercut the nominal width.
    expect(parseFloat(panel.style.maxWidth)).toBeGreaterThan(420);
  });

  // The production path. Asserting WHAT is observed, not only that a fire
  // re-measures: the fake's callback re-measures whichever element it was
  // told about, so only the observed list shows a target being dropped.
  it('observes the pane and the anchor, and re-measures when either resizes', () => {
    pane = ROOMY; anchor = ANCHOR;
    stubRects();
    render(inPane(el()));
    const [ro] = liveObservers();
    expect(ro.observed).toContain(screen.getByTestId('bounds'));
    expect(ro.observed).toContain(screen.getByTestId('anchor'));

    pane = SMALL;
    act(() => ro.fire());
    const panel = screen.getByTestId(testid);
    expect(panel.style.maxWidth).toBe(clampedW(SMALL, ANCHOR));
    expect(panel.style.maxHeight).toBe(clampedH(SMALL, ANCHOR));
  });

  // An overflow:hidden pane still scrolls programmatically (scroll-into-view),
  // which moves the anchor inside a pane of unchanged size — no resize fires.
  it('re-measures when the pane scrolls and the anchor moves inside it', () => {
    pane = SMALL; anchor = { ...ANCHOR, left: ANCHOR.left - 29, right: ANCHOR.right - 29 };
    stubRects();
    render(inPane(el()));
    const panel = screen.getByTestId(testid);
    expect(panel.style.maxWidth).toBe(clampedW(SMALL, anchor));

    anchor = ANCHOR;
    act(() => { screen.getByTestId('bounds').dispatchEvent(new Event('scroll')); });
    expect(panel.style.maxWidth).toBe(clampedW(SMALL, ANCHOR));
  });

  it('falls back to window resize only where ResizeObserver does not exist', () => {
    const add = vi.spyOn(window, 'addEventListener');
    pane = ROOMY; anchor = ANCHOR;
    stubRects();
    const { unmount } = render(inPane(el()));
    // With an observer, a window listener would only measure each resize twice.
    expect(add.mock.calls.filter(([type]) => type === 'resize')).toHaveLength(0);
    unmount();

    vi.stubGlobal('ResizeObserver', undefined);
    render(inPane(el()));
    const panel = screen.getByTestId(testid);
    expect(panel.style.maxHeight).toBe(`${nominalHeight}px`);
    pane = SMALL;
    act(() => { window.dispatchEvent(new Event('resize')); });
    expect(panel.style.maxWidth).toBe(clampedW(SMALL, ANCHOR));
    expect(panel.style.maxHeight).toBe(clampedH(SMALL, ANCHOR));
  });

  it('drops its observer and listeners on unmount', () => {
    pane = ROOMY; anchor = ANCHOR;
    stubRects();
    const { unmount } = render(inPane(el()));
    const bounds = screen.getByTestId('bounds');
    const removeScroll = vi.spyOn(bounds, 'removeEventListener');
    expect(liveObservers()).toHaveLength(1);
    unmount();
    expect(liveObservers()).toHaveLength(0);
    expect(removeScroll.mock.calls.some(([type]) => type === 'scroll')).toBe(true);

    vi.stubGlobal('ResizeObserver', undefined);
    const removeWin = vi.spyOn(window, 'removeEventListener');
    render(inPane(el())).unmount();
    expect(removeWin.mock.calls.some(([type]) => type === 'resize')).toBe(true);
  });

  it('never goes negative when the anchor is already at the pane\'s edge', () => {
    // Both raw differences are negative: 555 - 8 - 550 and 105 - 8 - 106.
    pane = { left: 500, top: 40, right: 555, bottom: 105 };
    anchor = ANCHOR;
    stubRects();
    render(inPane(el()));
    const panel = screen.getByTestId(testid);
    expect(panel.style.maxWidth).toBe('0px');
    expect(panel.style.maxHeight).toBe('0px');
  });

  it('is left unclamped, at its nominal height, outside a bounds element', () => {
    pane = SMALL; anchor = ANCHOR;
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
    pane = SMALL; anchor = ANCHOR;
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

// ConnectionsPanel stays mounted when closed (it animates out), so closing is
// a prop change, not an unmount — the clamp must stand down there too.
describe('ConnectionsPanel — clamp stands down when closed', () => {
  it('drops its observer and scroll listener on close, and stops re-measuring', () => {
    pane = ROOMY; anchor = ANCHOR;
    stubRects();
    const { rerender } = render(inPane(connections('out')));
    const bounds = screen.getByTestId('bounds');
    const removeScroll = vi.spyOn(bounds, 'removeEventListener');
    expect(liveObservers()).toHaveLength(1);

    rerender(inPane(connections(null)));
    expect(liveObservers()).toHaveLength(0);
    expect(removeScroll.mock.calls.some(([type]) => type === 'scroll')).toBe(true);

    const panel = screen.getByTestId('connections-panel');
    const before = panel.style.maxWidth;
    pane = SMALL;
    act(() => { bounds.dispatchEvent(new Event('scroll')); });
    expect(panel.style.maxWidth).toBe(before);
  });
});
