import { describe, it, expect } from 'vitest';
import { renderHook, act, waitFor } from '@testing-library/react';
import { useLensContentRev } from './useLensContentRev';
import { FakeEventSource, latestStream } from './testEventSource';

// The lens content stream's count is the lens list's second head: every bump
// runs the keep-place refresh. A notice bumps it, and so does every REOPEN
// after the first (the server keeps no ids and ends the stream on a lens edit,
// so a reopen may have missed notices). The first open does not: the list has
// just loaded.
describe('useLensContentRev', () => {
  // Sabotage: count the first open as well (rev is 1 before any change), or
  // ignore reopens (a reconnect after a lens edit refreshes nothing).
  it('counts notices and reopens after the first, not the first open', async () => {
    const { result } = renderHook(() => useLensContentRev('eng'));
    const es = latestStream('/api/v1/lenses/eng/events')!;
    expect(es).toBeDefined();
    await act(async () => { await Promise.resolve(); }); // the connect-time open
    expect(result.current).toBe(0);
    act(() => { es.emit('content', { repo: 'beta', id: 'bbbbbbbbbbbb', branch: 'main', head: 'h1' }); });
    expect(result.current).toBe(1);
    act(() => { es.emit('open'); }); // the EventSource reconnected
    expect(result.current).toBe(2);
    act(() => { es.emit('ready', {}); }); // not a kind this stream counts
    expect(result.current).toBe(2);
  });

  // Sabotage: drop the effect's cleanup and the old lens's stream stays open
  // (and keeps bumping a list it no longer belongs to).
  it('closes the stream on a lens switch and opens the new lens\'s; none outside a lens', async () => {
    const { rerender, result } = renderHook(({ lens }) => useLensContentRev(lens), { initialProps: { lens: 'eng' } });
    const a = latestStream('/api/v1/lenses/eng/events')!;
    expect(a.closedByClient).toBe(false);
    rerender({ lens: 'ops' });
    expect(a.closedByClient).toBe(true);
    const b = latestStream('/api/v1/lenses/ops/events')!;
    expect(b).toBeDefined();
    expect(b.closedByClient).toBe(false);
    const before = FakeEventSource.instances.length;
    rerender({ lens: '' });
    expect(b.closedByClient).toBe(true);
    expect(FakeEventSource.instances.length).toBe(before);
    await waitFor(() => expect(result.current).toBeGreaterThanOrEqual(0));
  });
});
