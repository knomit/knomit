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
    expect(fetchMock).toHaveBeenCalledTimes(1);

    act(() => { result.current.remove('chat-a'); });
    expect(result.current.chats.map(c => c.id)).toEqual(['chat-b']);
  });

  describe('a chat created in this page (the create starts its first turn before the page knows the id)', () => {
    const ev = (state: string, title: string, at: string) => ({ chat: 'chat-b', state, turns: 1, title, last_activity: at, started_at: 's' });
    const rowOf = (c: { current: { chats: { id: string }[] } }) => c.current.chats.find(x => x.id === 'chat-b') as ReturnType<typeof useChats>['chats'][number];
    const setup = async (list: unknown[]) => {
      vi.stubGlobal('fetch', vi.fn(async () => listBody(...list)));
      const hook = renderHook(() => useChats());
      await waitFor(() => expect(hook.result.current.loaded).toBe(true));
      await waitFor(() => expect(latestStream(CHAT_EVENTS_STREAM)).toBeTruthy());
      return { ...hook, es: latestStream(CHAT_EVENTS_STREAM)! };
    };

    it('keeps the stream open with no chat listed, and a running chat_state that arrived BEFORE add() is the row\'s state and first-message title', async () => {
      const { result, es } = await setup([]);
      expect(result.current.chats).toEqual([]);
      act(() => { es.emit('chat_state', ev('running', 'Say hi.', '2026-11-06T10:00:01Z')); });
      act(() => { result.current.add(B); });
      expect(rowOf(result)).toMatchObject({ state: 'running', title: 'Say hi.' });
      expect(railState(rowOf(result), undefined, false)).toBe('running');
    });

    it('running then idle between the create and add(): the row ends idle, not running', async () => {
      const { result, es } = await setup([A]);
      act(() => { es.emit('chat_state', ev('running', 'Say hi.', '2026-11-06T10:00:01Z')); });
      act(() => { es.emit('chat_state', ev('idle', 'Say hi.', '2026-11-06T10:00:02Z')); });
      act(() => { result.current.add(B); });
      expect(rowOf(result)).toMatchObject({ state: 'idle', title: 'Say hi.' });
      expect(railState(rowOf(result), 1, false)).toBe('idle');
    });

    it('an event after add() applies: running, then failed', async () => {
      const { result, es } = await setup([A]);
      act(() => { es.emit('chat_state', ev('running', 'Say hi.', '2026-11-06T10:00:01Z')); });
      act(() => { result.current.add(B); });
      act(() => { es.emit('chat_state', ev('error', 'Say hi.', '2026-11-06T10:00:03Z')); });
      expect(railState(rowOf(result), undefined, false)).toBe('failed');
    });

    it('an older GET /chats row does not overwrite a newer event, nor the add() it was cut before', async () => {
      const { result, es } = await setup([A]);
      const older = { id: 'chat-b', provider: 'local', model: 'o1', title: 'o1', state: 'running', turns: 1, last_activity: '2026-11-06T10:00:00Z' };
      let release!: () => void;
      vi.stubGlobal('fetch', vi.fn(() => new Promise<Response>(res => { release = () => res(listBody({ id: 'chat-a', provider: 'anthropic', model: 'claude-test-x' }, older)); })));
      act(() => { window.dispatchEvent(new Event('focus')); }); // a list read starts, cut while chat-b ran
      act(() => { es.emit('chat_state', ev('running', 'Say hi.', '2026-11-06T10:00:01Z')); });
      act(() => { result.current.add(B); }); // added during the read
      act(() => { es.emit('chat_state', ev('idle', 'Say hi.', '2026-11-06T10:00:02Z')); });
      await act(async () => { release(); });
      expect(rowOf(result)).toMatchObject({ state: 'idle', title: 'Say hi.' });
    });
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
