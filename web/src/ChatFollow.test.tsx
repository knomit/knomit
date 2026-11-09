import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { StrictMode } from 'react';
import type { ReactElement } from 'react';
import { render, screen, act, fireEvent, cleanup } from '@testing-library/react';
import { ChatFacet } from './ChatFacet';
import { installFakeEventSource, uninstallFakeEventSource, chatStream } from './testEventSource';
import { stubFleetFetch } from './fleetTestFetch';
import type { ChatSnapshot, ChatViewItem } from './chatApi';
import { CHAT_JUMP_LATEST } from './chatCopy';

// Follow or read: at the bottom the transcript follows the stream; scrolled up,
// nothing moves it, and "Jump to latest ↓" brings it back. jsdom has no
// layout, so the transcript's geometry is stubbed: scrollHeight and
// clientHeight are read from `geom`, and scrollTop is a plain stored number.
// jsdom has no ResizeObserver either: a fake one records its callbacks, and
// `resized()` fires them as a real one would on a size change.

const CHAT = { id: 'chat-a', provider: 'anthropic', model: 'claude-test-x', name: 'Haiku 5.5' };
const BASE = '/api/v1/chats/chat-a';
const snap = (items: ChatViewItem[], over: Partial<ChatSnapshot> = {}): ChatSnapshot =>
  ({ ...CHAT, running: false, grants: [], pending_approvals: [], items, ...over });

const geom = { height: 1000, client: 200 };
const BOTTOM = () => geom.height - geom.client;

let observers: { cb: () => void; live: boolean }[] = [];
class FakeResizeObserver {
  private o: { cb: () => void; live: boolean };
  constructor(cb: () => void) { this.o = { cb, live: true }; observers.push(this.o); }
  observe() {}
  unobserve() {}
  disconnect() { this.o.live = false; }
}
const resized = () => act(() => { for (const o of observers) if (o.live) o.cb(); });

/** Let the follow's one-per-frame write run. */
const frame = () => act(async () => { await new Promise(r => requestAnimationFrame(() => r(null))); });
const scrollTo = (el: HTMLElement, top: number) => { el.scrollTop = top; fireEvent.scroll(el); };
const delta = (text: string) => act(() => { chatStream('chat-a')!.emit('text_delta', { text, turn: 0 }); });
const pill = () => screen.queryByRole('button', { name: CHAT_JUMP_LATEST });
const reducedMotion = (on: boolean) => vi.stubGlobal('matchMedia', vi.fn((q: string) => ({ matches: on && q.includes('reduce'), media: q, addEventListener() {}, removeEventListener() {} })));

beforeEach(() => {
  installFakeEventSource(); geom.height = 1000; geom.client = 200; reducedMotion(true);
  observers = [];
  vi.stubGlobal('ResizeObserver', FakeResizeObserver);
});
afterEach(() => { cleanup(); uninstallFakeEventSource(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

// Everything runs twice: plain, and under StrictMode, which main.tsx uses and
// which mounts, cleans up and mounts again with the same refs.
describe.each([['plain', false], ['StrictMode', true]] as const)('follow or read (%s)', (_mode, strict) => {
  const wrap = (ui: ReactElement) => (strict ? <StrictMode>{ui}</StrictMode> : ui);

  async function mountStreaming(running = true) {
    stubFleetFetch({
      [`GET ${BASE}`]: { body: snap([{ kind: 'user', text: 'q' }], { running }) },
      [`GET ${BASE}/context`]: { body: { window: 1_000_000, spill: false } },
      [`POST ${BASE}/turns`]: { status: 202, body: { turn: 1 } },
    });
    render(wrap(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} />));
    await screen.findByTestId('chat-turn-0');
    await frame();
    const el = screen.getByTestId('chat-transcript') as HTMLDivElement;
    Object.defineProperty(el, 'scrollHeight', { configurable: true, get: () => geom.height });
    Object.defineProperty(el, 'clientHeight', { configurable: true, get: () => geom.client });
    return el;
  }

  it('at the bottom a delta keeps it at the bottom (StrictMode included: the frame handle survives remount)', async () => {
    const el = await mountStreaming();
    scrollTo(el, BOTTOM());
    geom.height = 1400;
    delta('more');
    await frame();
    expect(el.scrollTop).toBe(1200);
    expect(pill()).toBeNull();
    geom.height = 1700;
    delta(' and more');
    await frame();
    expect(el.scrollTop).toBe(1500);
  });

  it('not at the bottom shows the pill, labelled exactly "Jump to latest ↓"', async () => {
    const el = await mountStreaming();
    scrollTo(el, BOTTOM());
    expect(pill()).toBeNull();
    scrollTo(el, 300);
    expect(pill()).not.toBeNull();
    expect(pill()!.textContent).toBe('Jump to latest ↓');
  });

  it('a streamed delta while scrolled up leaves scrollTop where the reader put it', async () => {
    const el = await mountStreaming();
    scrollTo(el, BOTTOM());
    scrollTo(el, 300);
    geom.height = 1400;
    delta('more text below');
    await frame();
    expect(screen.getByTestId('chat-text').textContent).toBe('more text below');
    expect(el.scrollTop).toBe(300);
    expect(pill()).not.toBeNull();
  });

  it('a scroll up between a delta and its frame wins: the queued follow does not pull the reader back', async () => {
    const el = await mountStreaming();
    scrollTo(el, BOTTOM());
    geom.height = 1400;
    delta('queued');
    scrollTo(el, 300);
    await frame();
    expect(el.scrollTop).toBe(300);
    expect(pill()).not.toBeNull();
  });

  it('a follow-scroll that lands after the page grew again is not taken for the reader scrolling up', async () => {
    const el = await mountStreaming();
    scrollTo(el, BOTTOM());
    // The delta grows the page; the scroll event from our earlier write arrives
    // before this frame's write, with the bottom now 400px away.
    geom.height = 1400;
    delta('a');
    fireEvent.scroll(el);
    expect(pill()).toBeNull();
    await frame();
    expect(el.scrollTop).toBe(1200);
    // A smooth scroll's intermediate steps only move down: still following.
    geom.height = 2000;
    scrollTo(el, 1250);
    scrollTo(el, 1500);
    expect(pill()).toBeNull();
    delta('b');
    await frame();
    expect(el.scrollTop).toBe(1800);
  });

  it('a small wheel step up stops following at once, inside the 24px slack, and the next delta does not re-pin', async () => {
    const el = await mountStreaming();
    scrollTo(el, BOTTOM());
    fireEvent.wheel(el, { deltaY: -10 });
    expect(pill()).not.toBeNull();
    scrollTo(el, 790); // the browser's own 10px scroll for that step
    expect(pill()).not.toBeNull();
    geom.height = 1400;
    delta('more');
    await frame();
    expect(el.scrollTop).toBe(790);
  });

  it('a wheel step down does not leave, and a wheel step up with nothing above (scrollTop 0) does not either', async () => {
    const el = await mountStreaming();
    scrollTo(el, BOTTOM());
    fireEvent.wheel(el, { deltaY: 10 });
    expect(pill()).toBeNull();
    geom.height = 200; // the transcript fits: nothing scrolls
    scrollTo(el, 0);
    fireEvent.wheel(el, { deltaY: -10 });
    expect(pill()).toBeNull();
  });

  it('scrolling back down to within the slack resumes following; a small scroll UP inside the slack does not', async () => {
    const el = await mountStreaming();
    scrollTo(el, BOTTOM());
    fireEvent.wheel(el, { deltaY: -10 });
    scrollTo(el, 790);
    scrollTo(el, 785);
    expect(pill()).not.toBeNull(); // still going up, though 15px from the bottom
    scrollTo(el, 790);
    expect(pill()).toBeNull(); // coming down inside the slack re-arms
    geom.height = 1400;
    delta('followed again');
    await frame();
    expect(el.scrollTop).toBe(1200);
  });

  it('clicking the pill goes to the bottom and hides it; with motion allowed it scrolls smoothly', async () => {
    const el = await mountStreaming();
    scrollTo(el, BOTTOM());
    scrollTo(el, 300);
    fireEvent.click(pill()!);
    expect(el.scrollTop).toBe(800);
    expect(pill()).toBeNull();
    // Following again: the next delta is followed.
    geom.height = 1300;
    delta('next');
    await frame();
    expect(el.scrollTop).toBe(1100);

    reducedMotion(false);
    const smooth = vi.fn();
    el.scrollTo = smooth as unknown as typeof el.scrollTo;
    scrollTo(el, 200);
    fireEvent.click(pill()!);
    expect(smooth).toHaveBeenCalledWith({ top: 1100, behavior: 'smooth' });
    expect(pill()).toBeNull();
  });

  it('scroll anchoring is off while following and back on while reading', async () => {
    const el = await mountStreaming();
    scrollTo(el, BOTTOM());
    expect(el.style.overflowAnchor).toBe('none');
    scrollTo(el, 300);
    expect(el.style.overflowAnchor).toBe('auto');
    fireEvent.click(pill()!);
    expect(el.style.overflowAnchor).toBe('none');
  });

  it('growth no render announced (ResizeObserver) re-pins while following and moves nothing while reading', async () => {
    const el = await mountStreaming();
    scrollTo(el, BOTTOM());
    geom.height = 1250; // a fact chip resolved, say: no new delta
    resized();
    await frame();
    expect(el.scrollTop).toBe(1050);
    scrollTo(el, 400);
    geom.height = 1600;
    resized();
    await frame();
    expect(el.scrollTop).toBe(400);
  });

  it('hidden (display:none measures 0), a scroll event changes nothing: a reader stays reading where they were', async () => {
    const el = await mountStreaming();
    scrollTo(el, BOTTOM());
    scrollTo(el, 300);
    geom.height = 0; geom.client = 0;
    scrollTo(el, 0);
    // Shown again: the browser restores the place, and the box resizes.
    geom.height = 1000; geom.client = 200;
    el.scrollTop = 300;
    resized();
    await frame();
    expect(el.scrollTop).toBe(300);
    expect(pill()).not.toBeNull();
  });

  it('a following chat shown again lands at the bottom through the resize, with what streamed while hidden', async () => {
    const el = await mountStreaming();
    scrollTo(el, BOTTOM());
    geom.height = 0; geom.client = 0;
    delta('streamed while hidden');
    await frame();
    expect(el.scrollTop).toBe(800); // no write while hidden
    geom.height = 1500; geom.client = 200;
    resized();
    await frame();
    expect(el.scrollTop).toBe(1300);
    expect(pill()).toBeNull();
  });

  it('sending a message jumps to the bottom and follows again', async () => {
    const el = await mountStreaming(false);
    scrollTo(el, BOTTOM());
    scrollTo(el, 300);
    expect(pill()).not.toBeNull();
    fireEvent.change(screen.getByTestId('chat-input'), { target: { value: 'again' } });
    fireEvent.click(screen.getByTestId('chat-send'));
    expect(el.scrollTop).toBe(800);
    expect(pill()).toBeNull();
  });
});
