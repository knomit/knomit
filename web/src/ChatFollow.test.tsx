import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, act, fireEvent, cleanup } from '@testing-library/react';
import { ChatFacet } from './ChatFacet';
import { installFakeEventSource, uninstallFakeEventSource, chatStream } from './testEventSource';
import { stubFleetFetch } from './fleetTestFetch';
import type { ChatSnapshot, ChatViewItem } from './chatApi';
import { CHAT_JUMP_LATEST } from './chatCopy';

// Follow or read: at the bottom the transcript follows the stream; scrolled up,
// nothing moves it, and "Jump to latest ↓" (or End) brings it back. jsdom has no
// layout, so the transcript's geometry is stubbed: scrollHeight and
// clientHeight are read from `geom`, and scrollTop is a plain stored number.

const CHAT = { id: 'chat-a', provider: 'anthropic', model: 'claude-test-x', name: 'Haiku 5.5' };
const BASE = '/api/v1/chats/chat-a';
const snap = (items: ChatViewItem[], over: Partial<ChatSnapshot> = {}): ChatSnapshot =>
  ({ ...CHAT, running: false, grants: [], pending_approvals: [], items, ...over });

const geom = { height: 1000, client: 200 };
const BOTTOM = () => geom.height - geom.client;

let rerenderWith: (onScreen: boolean) => void = () => {};

async function mountStreaming(running = true) {
  stubFleetFetch({
    [`GET ${BASE}`]: { body: snap([{ kind: 'user', text: 'q' }], { running }) },
    [`GET ${BASE}/context`]: { body: { window: 1_000_000, spill: false } },
    [`POST ${BASE}/turns`]: { status: 202, body: { turn: 1 } },
  });
  const onGone = vi.fn();
  const { rerender } = render(<ChatFacet chat={CHAT} facet="chat" onGone={onGone} />);
  rerenderWith = (onScreen: boolean) => rerender(<ChatFacet chat={CHAT} facet="chat" onGone={onGone} onScreen={onScreen} />);
  await screen.findByTestId('chat-turn-0');
  await frame();
  const el = screen.getByTestId('chat-transcript') as HTMLDivElement;
  Object.defineProperty(el, 'scrollHeight', { configurable: true, get: () => geom.height });
  Object.defineProperty(el, 'clientHeight', { configurable: true, get: () => geom.client });
  return el;
}

/** Let the follow's one-per-frame write run. */
const frame = () => act(async () => { await new Promise(r => requestAnimationFrame(() => r(null))); });
const scrollTo = (el: HTMLElement, top: number) => { el.scrollTop = top; fireEvent.scroll(el); };
const delta = (text: string) => act(() => { chatStream('chat-a')!.emit('text_delta', { text, turn: 0 }); });
const pill = () => screen.queryByRole('button', { name: CHAT_JUMP_LATEST });
const reducedMotion = (on: boolean) => vi.stubGlobal('matchMedia', vi.fn((q: string) => ({ matches: on && q.includes('reduce'), media: q, addEventListener() {}, removeEventListener() {} })));

beforeEach(() => { installFakeEventSource(); geom.height = 1000; geom.client = 200; reducedMotion(true); });
afterEach(() => { cleanup(); uninstallFakeEventSource(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

describe('the transcript follows at the bottom and stays put when scrolled up', () => {
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

  it('at the bottom a delta keeps it at the bottom', async () => {
    const el = await mountStreaming();
    scrollTo(el, BOTTOM());
    geom.height = 1400;
    delta('more');
    await frame();
    expect(el.scrollTop).toBe(1200);
    expect(pill()).toBeNull();
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

  it('End in the composer does nothing; End with focus elsewhere jumps to the latest', async () => {
    const el = await mountStreaming();
    scrollTo(el, BOTTOM());
    scrollTo(el, 300);
    const box = screen.getByTestId('chat-input');
    box.focus();
    const inBox = fireEvent.keyDown(box, { key: 'End' });
    expect(inBox).toBe(true); // not prevented: the composer keeps its own End
    expect(el.scrollTop).toBe(300);
    expect(pill()).not.toBeNull();

    box.blur();
    const elsewhere = fireEvent.keyDown(document.body, { key: 'End' });
    expect(elsewhere).toBe(false);
    expect(el.scrollTop).toBe(800);
    expect(pill()).toBeNull();
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

  it('a chat that comes back on screen starts at the bottom, following', async () => {
    const el = await mountStreaming();
    scrollTo(el, BOTTOM());
    scrollTo(el, 300);
    expect(pill()).not.toBeNull();
    rerenderWith(false);
    rerenderWith(true);
    expect(el.scrollTop).toBe(800);
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
