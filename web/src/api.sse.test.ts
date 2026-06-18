import { describe, it, expect } from 'vitest';
import { readSSEStream } from './api';
import type { SSEEvent } from './api';

// Build a fake fetch Response whose body streams the given string chunks
// verbatim, one per read() call. This lets us control exactly where the
// network chunk boundaries fall — the crux of the SSE reassembly bug.
function fakeResponse(chunks: string[]): Response {
  const enc = new TextEncoder();
  let i = 0;
  const reader = {
    read: async () =>
      i < chunks.length
        ? { done: false, value: enc.encode(chunks[i++]) }
        : { done: true, value: undefined },
  };
  return { ok: true, body: { getReader: () => reader } } as unknown as Response;
}

async function collect(chunks: string[]): Promise<SSEEvent[]> {
  const events: SSEEvent[] = [];
  await readSSEStream(fakeResponse(chunks), (e) => events.push(e));
  return events;
}

describe('readSSEStream', () => {
  it('delivers the terminal done event when its line is split across a newline-free middle chunk', async () => {
    // The done event's bytes arrive in three reads; the middle read contains no
    // newline. The buggy reader wiped the buffer on any newline-free chunk,
    // dropping the done event and hanging the connect wizard at "applying".
    const events = await collect([
      'data: {"phase":"replaying"}\n\ndata: {"phase":"do',
      'ne","resu',
      'lt":{"total_facts":568}}\n\n',
    ]);

    expect(events.map((e) => e.phase)).toEqual(['replaying', 'done']);
    expect((events[1] as { result: { total_facts: number } }).result.total_facts).toBe(568);
  });

  it('does not duplicate events that arrive in a single chunk', async () => {
    const events = await collect([
      'data: {"phase":"replaying"}\n\ndata: {"phase":"done","result":{}}\n\n',
    ]);
    expect(events.map((e) => e.phase)).toEqual(['replaying', 'done']);
  });
});
