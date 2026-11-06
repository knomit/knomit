import { describe, it, expect, vi, afterEach } from 'vitest';
import { renderHook, waitFor, act } from '@testing-library/react';
import { useChats } from './useChats';
import { notifyChatActivity } from './chatRail';

const A = { id: 'chat-a', provider: 'anthropic', model: 'claude-test-x' };
const B = { id: 'chat-b', provider: 'local', model: 'o1' };
const listBody = (...chats: unknown[]) => new Response(JSON.stringify({ count: chats.length, _embedded: { chats } }), { status: 200 });

afterEach(() => { vi.unstubAllGlobals(); });

describe('useChats', () => {
  it('reads the list again, with the row fields, when a chat reports activity', async () => {
    const row = (state: string, turns: number) => ({ ...A, title: 'hello', state, turns, last_activity: '2026-11-06T10:00:00Z', cost_usd: 0.5 });
    const fetchMock = vi.fn<(url: string) => Promise<Response>>()
      .mockResolvedValueOnce(listBody(row('idle', 0)))
      .mockResolvedValueOnce(listBody(row('running', 1)));
    vi.stubGlobal('fetch', fetchMock);
    const { result } = renderHook(() => useChats());
    await waitFor(() => expect(result.current.loaded).toBe(true));
    expect(result.current.chats[0].state).toBe('idle');
    act(() => { notifyChatActivity(); });
    await waitFor(() => expect(result.current.chats[0].state).toBe('running'));
    expect(result.current.chats[0]).toMatchObject({ title: 'hello', turns: 1, cost_usd: 0.5, last_activity: '2026-11-06T10:00:00Z' });
    expect(fetchMock).toHaveBeenCalledTimes(2);
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
