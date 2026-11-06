import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, act, cleanup } from '@testing-library/react';
import { ChatFacet } from './ChatFacet';
import { subscribeChatEvents, chatEventStreamCount } from './chatEvents';
import { installFakeEventSource, uninstallFakeEventSource, streamsMatching, latestStream, CHAT_EVENTS_STREAM } from './testEventSource';
import { stubFleetFetch } from './fleetTestFetch';
import type { ChatSnapshot } from './chatApi';

beforeEach(() => { installFakeEventSource(); });
afterEach(() => { cleanup(); uninstallFakeEventSource(); vi.unstubAllGlobals(); });

const flush = () => act(async () => { await Promise.resolve(); });
const pageStreams = () => streamsMatching(CHAT_EVENTS_STREAM);

describe('the page\'s one chat events stream', () => {
  it('opens ONE connection however many chats subscribe, and closes it with the last', async () => {
    const offs = Array.from({ length: 10 }, (_, i) => subscribeChatEvents(`chat-${i}`, () => {}));
    await flush();
    expect(pageStreams()).toHaveLength(1);
    expect(pageStreams()[0].url).toBe('/api/v1/chats/events');
    expect(chatEventStreamCount()).toBe(1);
    offs.slice(0, 9).forEach(off => off());
    await flush();
    expect(pageStreams()[0].closedByClient).toBe(false);
    offs[9]();
    await flush();
    expect(pageStreams()[0].closedByClient).toBe(true);
    expect(chatEventStreamCount()).toBe(0);
  });

  it('routes each event to the chat it is tagged with, and drops events for chats nobody shows', async () => {
    const a = vi.fn();
    const b = vi.fn();
    const offs = [subscribeChatEvents('chat-a', a), subscribeChatEvents('chat-b', b)];
    await flush();
    const es = latestStream(CHAT_EVENTS_STREAM)!;
    act(() => {
      es.emit('text_delta', { chat: 'chat-a', text: 'for a', turn: 0 });
      es.emit('text_delta', { chat: 'chat-b', text: 'for b', turn: 1 });
      es.emit('turn_done', { chat: 'chat-c', turn: 0 });
      es.emit('turn_done', { turn: 0 }); // untagged: nobody's
    });
    const named = (fn: ReturnType<typeof vi.fn>) => fn.mock.calls.filter(c => c[0] !== 'ready').map(c => [c[0], (c[1] as { text?: string }).text]);
    expect(named(a)).toEqual([['text_delta', 'for a']]);
    expect(named(b)).toEqual([['text_delta', 'for b']]);
    offs.forEach(off => off());
  });

  it('a page-level ready (every connect, so every reconnect) reaches every subscriber, and a late subscriber is told too', async () => {
    const a = vi.fn();
    const b = vi.fn();
    const offs = [subscribeChatEvents('chat-a', a), subscribeChatEvents('chat-b', b)];
    await flush();
    expect(a.mock.calls.filter(c => c[0] === 'ready')).toHaveLength(1); // the connect
    act(() => { latestStream(CHAT_EVENTS_STREAM)!.emit('ready', {}); }); // a reconnect
    expect(a.mock.calls.filter(c => c[0] === 'ready')).toHaveLength(2);
    expect(b.mock.calls.filter(c => c[0] === 'ready')).toHaveLength(2);
    const late = vi.fn();
    offs.push(subscribeChatEvents('chat-late', late));
    await flush();
    expect(late.mock.calls.map(c => c[0])).toEqual(['ready']);
    expect(pageStreams()).toHaveLength(1);
    offs.forEach(off => off());
  });

  it('an unsubscribed chat hears nothing more', async () => {
    const a = vi.fn();
    const keep = subscribeChatEvents('chat-b', () => {});
    const off = subscribeChatEvents('chat-a', a);
    await flush();
    off();
    const before = a.mock.calls.length;
    act(() => { latestStream(CHAT_EVENTS_STREAM)!.emit('text_delta', { chat: 'chat-a', text: 'x', turn: 0 }); });
    expect(a.mock.calls).toHaveLength(before);
    keep();
  });
});

describe('chats on the one stream', () => {
  const snap = (id: string, text: string): ChatSnapshot => ({
    id, provider: 'anthropic', model: 'claude-test-x', running: false, grants: [], pending_approvals: [],
    items: [{ kind: 'user', text: 'q' }, { kind: 'assistant', text }],
  } as ChatSnapshot);
  const chat = (id: string) => ({ id, provider: 'anthropic', model: 'claude-test-x' });

  function mountTwo() {
    const fx = stubFleetFetch({
      'GET /api/v1/chats/chat-a': { body: snap('chat-a', 'answer a') },
      'GET /api/v1/chats/chat-b': { body: snap('chat-b', 'answer b') },
    });
    render(<><div data-testid="a"><ChatFacet chat={chat('chat-a')} facet="chat" onGone={vi.fn()} /></div><div data-testid="b"><ChatFacet chat={chat('chat-b')} facet="chat" onGone={vi.fn()} /></div></>);
    return fx;
  }
  const count = (fx: ReturnType<typeof stubFleetFetch>, id: string) => fx.calls.filter(c => c.route === `GET /api/v1/chats/${id}`).length;

  it('two ChatFacets share one connection, and an event reaches only the chat it names', async () => {
    mountTwo();
    await waitFor(() => expect(screen.getAllByTestId('chat-turn-0')).toHaveLength(2));
    expect(pageStreams()).toHaveLength(1);
    act(() => { latestStream(CHAT_EVENTS_STREAM)!.emit('text_delta', { chat: 'chat-b', text: ' and more for b', turn: 0 }); });
    await waitFor(() => expect(screen.getByTestId('b')).toHaveTextContent('and more for b'));
    expect(screen.getByTestId('a')).not.toHaveTextContent('and more for b');
  });

  it('a reconnect reloads every chat on screen once, and no chat on screen is left on stale state', async () => {
    const fx = mountTwo();
    await waitFor(() => expect(screen.getAllByTestId('chat-turn-0')).toHaveLength(2));
    const [a0, b0] = [count(fx, 'chat-a'), count(fx, 'chat-b')];
    fx.set('GET /api/v1/chats/chat-a', { body: snap('chat-a', 'a after reconnect') });
    fx.set('GET /api/v1/chats/chat-b', { body: snap('chat-b', 'b after reconnect') });
    act(() => { latestStream(CHAT_EVENTS_STREAM)!.emit('ready', {}); });
    await waitFor(() => expect(screen.getByTestId('a')).toHaveTextContent('a after reconnect'));
    await waitFor(() => expect(screen.getByTestId('b')).toHaveTextContent('b after reconnect'));
    expect(count(fx, 'chat-a')).toBe(a0 + 1);
    expect(count(fx, 'chat-b')).toBe(b0 + 1);
    expect(pageStreams()).toHaveLength(1);
  });

  it('a turn end tagged with one chat reloads that chat only', async () => {
    const fx = mountTwo();
    await waitFor(() => expect(screen.getAllByTestId('chat-turn-0')).toHaveLength(2));
    const [a0, b0] = [count(fx, 'chat-a'), count(fx, 'chat-b')];
    act(() => { latestStream(CHAT_EVENTS_STREAM)!.emit('turn_done', { chat: 'chat-a', turn: 0 }); });
    await waitFor(() => expect(count(fx, 'chat-a')).toBe(a0 + 1));
    expect(count(fx, 'chat-b')).toBe(b0);
  });
});
