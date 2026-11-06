import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { renderHook, waitFor, act } from '@testing-library/react';
import { useChats } from './useChats';
import { railState } from './chatRail';
import { installFakeEventSource, uninstallFakeEventSource, latestStream, CHAT_EVENTS_STREAM } from './testEventSource';

const A = { id: 'chat-a', provider: 'anthropic', model: 'claude-test-x' };
const B = { id: 'chat-b', provider: 'local', model: 'o1' };
const listBody = (...chats: unknown[]) => new Response(JSON.stringify({ count: chats.length, _embedded: { chats } }), { status: 200 });

beforeEach(() => { installFakeEventSource(); });
afterEach(() => { uninstallFakeEventSource(); vi.unstubAllGlobals(); });

describe('useChats', () => {
  it('applies a chat_state from the page stream to the chat\'s row, for every listed chat, and reads the list again on a reconnect', async () => {
    const row = (id: string, state: string, turns: number) => ({ id, provider: 'anthropic', model: 'm', title: 'hello', state, turns, last_activity: '2026-11-06T10:00:00Z' });
    const fetchMock = vi.fn<(url: string) => Promise<Response>>(async () => listBody(row('chat-a', 'idle', 0), row('chat-b', 'idle', 1)));
    vi.stubGlobal('fetch', fetchMock);
    const { result } = renderHook(() => useChats());
    await waitFor(() => expect(result.current.loaded).toBe(true));
    // Nothing here mounts a ChatFacet: the rail itself is subscribed.
    await waitFor(() => expect(latestStream(CHAT_EVENTS_STREAM)).toBeTruthy());
    const es = latestStream(CHAT_EVENTS_STREAM)!;
    act(() => {
      es.emit('chat_state', { chat: 'chat-b', state: 'running', turns: 2, title: 'hello', cost_usd: 0.5, last_activity: '2026-11-06T10:05:00Z', started_at: '2026-11-06T09:00:00Z' });
    });
    const byId = (id: string) => result.current.chats.find(c => c.id === id)!;
    expect(byId('chat-b')).toMatchObject({ state: 'running', turns: 2, cost_usd: 0.5, last_activity: '2026-11-06T10:05:00Z', title: 'hello' });
    expect(byId('chat-a')).toMatchObject({ state: 'idle', turns: 0 });
    // An event for a chat the list does not hold changes nothing.
    act(() => { es.emit('chat_state', { chat: 'chat-z', state: 'error', turns: 9 }); });
    expect(result.current.chats.map(c => c.id).sort()).toEqual(['chat-a', 'chat-b']);
    // The absent cost is absent again.
    act(() => { es.emit('chat_state', { chat: 'chat-b', state: 'idle', turns: 2, title: 'hello' }); });
    expect(byId('chat-b').cost_usd).toBeUndefined();
    // A reconnect (a page-level ready) reads the list again.
    const before = fetchMock.mock.calls.length;
    act(() => { es.emit('ready', {}); });
    await waitFor(() => expect(fetchMock.mock.calls.length).toBeGreaterThan(before));
  });

  it('a chat_state that arrives while a reconnect re-read is in flight is not overwritten by the older list', async () => {
    const row = (state: string, turns: number) => ({ id: 'chat-a', provider: 'anthropic', model: 'm', title: 't', state, turns, last_activity: '2026-11-06T10:00:00Z' });
    let release!: () => void;
    const fetchMock = vi.fn<(url: string) => Promise<Response>>()
      .mockResolvedValueOnce(listBody(row('running', 1)))
      // The reconnect's read: cut while the chat was still running.
      .mockImplementationOnce(() => new Promise<Response>(res => { release = () => res(listBody(row('running', 1))); }));
    vi.stubGlobal('fetch', fetchMock);
    const { result } = renderHook(() => useChats());
    await waitFor(() => expect(result.current.loaded).toBe(true));
    await waitFor(() => expect(latestStream(CHAT_EVENTS_STREAM)).toBeTruthy());
    const es = latestStream(CHAT_EVENTS_STREAM)!;
    act(() => { es.emit('ready', {}); }); // a reconnect: the re-read starts
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    // The turn ends while the read is in flight.
    act(() => { es.emit('chat_state', { chat: 'chat-a', state: 'idle', turns: 1, title: 't', last_activity: '2026-11-06T10:05:00Z', started_at: 's' }); });
    expect(result.current.chats[0].state).toBe('idle');
    // The older answer lands: the newer event still wins.
    await act(async () => { release(); });
    expect(result.current.chats[0]).toMatchObject({ state: 'idle', last_activity: '2026-11-06T10:05:00Z' });
  });

  it('loads GET /chats and add() makes a new chat visible in the same render, with no further fetch', async () => {
    const fetchMock = vi.fn<(url: string) => Promise<Response>>(async () => listBody(A));
    vi.stubGlobal('fetch', fetchMock);
    const { result } = renderHook(() => useChats());
    await waitFor(() => expect(result.current.loaded).toBe(true));
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(String(fetchMock.mock.calls[0][0])).toBe('/api/v1/chats');
    expect(result.current.chats.map(c => c.id)).toEqual(['chat-a']);

    act(() => { result.current.add(B); });
    expect(result.current.chats.map(c => c.id)).toEqual(['chat-a', 'chat-b']);
    // The one further read only fills in the new row's state; it never replaces the list.
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(result.current.chats.map(c => c.id)).toEqual(['chat-a', 'chat-b']);

    act(() => { result.current.remove('chat-a'); });
    expect(result.current.chats.map(c => c.id)).toEqual(['chat-b']);
  });

  it('a chat created in this page shows the server\'s state and first-message title: its first events predate the subscription, so add() reads the row', async () => {
    const server = (id: string, title: string, state: string, turns: number) => ({ id, provider: 'local', model: 'o1', title, state, turns, last_activity: '2026-11-06T10:00:00Z' });
    // The create (with its first message) answers with identity only; by the time
    // the page subscribes, the server's chat is already running with its title.
    const fetchMock = vi.fn<(url: string) => Promise<Response>>()
      .mockResolvedValueOnce(listBody(server('chat-a', 'older chat', 'idle', 1)))
      .mockResolvedValue(listBody(server('chat-a', 'older chat', 'idle', 1), server('chat-b', 'what is a fact?', 'running', 1)));
    vi.stubGlobal('fetch', fetchMock);
    const { result } = renderHook(() => useChats());
    await waitFor(() => expect(result.current.loaded).toBe(true));
    await waitFor(() => expect(latestStream(CHAT_EVENTS_STREAM)).toBeTruthy());
    const es = latestStream(CHAT_EVENTS_STREAM)!;
    const byId = (id: string) => result.current.chats.find(c => c.id === id)!;

    act(() => { result.current.add(B); });
    expect(byId('chat-b').state).toBeUndefined();
    await waitFor(() => expect(byId('chat-b')).toMatchObject({ state: 'running', title: 'what is a fact?' }));
    expect(railState(byId('chat-b'), undefined, false)).toBe('running');

    // Then the turn fails: the live event moves the row to failed.
    act(() => { es.emit('chat_state', { chat: 'chat-b', state: 'error', turns: 1, title: 'what is a fact?', last_activity: '2026-11-06T10:01:00Z', started_at: 's' }); });
    expect(byId('chat-b')).toMatchObject({ state: 'error', title: 'what is a fact?' });
    expect(railState(byId('chat-b'), undefined, false)).toBe('failed');
  });

  it('an add() made while the first GET is in flight survives that GET\'s answer', async () => {
    let release!: () => void;
    vi.stubGlobal('fetch', vi.fn(() => new Promise<Response>(res => { release = () => res(listBody(A)); })));
    const { result } = renderHook(() => useChats());
    act(() => { result.current.add(B); });
    await act(async () => { release(); });
    await waitFor(() => expect(result.current.loaded).toBe(true));
    expect(result.current.chats.map(c => c.id).sort()).toEqual(['chat-a', 'chat-b']);
  });

  it('N3: a chat removed while a list read is in flight stays gone when the read answers', async () => {
    let release!: () => void;
    const fetchMock = vi.fn<(url: string) => Promise<Response>>()
      .mockResolvedValueOnce(listBody(A, B))
      .mockImplementationOnce(() => new Promise<Response>(res => { release = () => res(listBody(A, B)); }));
    vi.stubGlobal('fetch', fetchMock);
    const { result } = renderHook(() => useChats());
    await waitFor(() => expect(result.current.chats.map(c => c.id)).toEqual(['chat-a', 'chat-b']));

    // A second read goes out (focus), cut while chat-b still exists...
    act(() => { window.dispatchEvent(new Event('focus')); });
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    // ...and chat-b ends (chat_end) before it answers.
    act(() => { result.current.remove('chat-b'); });
    expect(result.current.chats.map(c => c.id)).toEqual(['chat-a']);
    await act(async () => { release(); });
    // The late answer still lists chat-b; it must not come back.
    expect(result.current.chats.map(c => c.id)).toEqual(['chat-a']);
  });

  it('reads the list again on window focus: a chat the server dropped leaves, one it gained joins', async () => {
    let body = listBody(A, B);
    const fetchMock = vi.fn<(url: string) => Promise<Response>>(async () => body.clone());
    vi.stubGlobal('fetch', fetchMock);
    const { result } = renderHook(() => useChats());
    await waitFor(() => expect(result.current.chats.map(c => c.id)).toEqual(['chat-a', 'chat-b']));

    body = listBody(B, { id: 'chat-c', provider: 'local', model: 'o2' });
    act(() => { window.dispatchEvent(new Event('focus')); });
    await waitFor(() => expect(result.current.chats.map(c => c.id)).toEqual(['chat-b', 'chat-c']));
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it('does not fetch when disabled, and is not loaded', async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    const { result } = renderHook(() => useChats(false));
    expect(result.current.loaded).toBe(false);
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
