import { describe, it, expect, vi, afterEach } from 'vitest';
import { renderHook, waitFor, act } from '@testing-library/react';
import { useChats } from './useChats';

const A = { id: 'chat-a', provider: 'anthropic', model: 'claude-test-x' };
const B = { id: 'chat-b', provider: 'local', model: 'o1' };
const listBody = (...chats: unknown[]) => new Response(JSON.stringify({ count: chats.length, _embedded: { chats } }), { status: 200 });

afterEach(() => { vi.unstubAllGlobals(); });

describe('useChats', () => {
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

  it('does not fetch when disabled, and is not loaded', async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    const { result } = renderHook(() => useChats(false));
    expect(result.current.loaded).toBe(false);
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
