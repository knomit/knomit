import { describe, it, expect, vi, afterEach } from 'vitest';
import {
  WailsEventSource, WAILS_SSE_STREAM, openEventStream, streamErrorSuffix,
  RECONNECT_INITIAL_MS, RECONNECT_MAX_MS, EVENT_TRANSPORT_FLAG, WAILS_STREAM_TRANSPORT,
} from './eventStream';
import type { WailsSocketLike } from './eventStream';

class FakeSocket implements WailsSocketLike {
  readyState = 0;
  binaryType = 'blob';
  onopen: ((e: unknown) => void) | null = null;
  onmessage: ((e: { data: unknown }) => void) | null = null;
  onclose: ((e: unknown) => void) | null = null;
  onerror: ((e: unknown) => void) | null = null;
  sent: string[] = [];
  closed = 0;
  send(d: string | ArrayBuffer | Uint8Array) { this.sent.push(String(d)); }
  close() { this.closed += 1; }
  push(frame: unknown) {
    const bytes = new TextEncoder().encode(JSON.stringify(frame));
    this.onmessage?.({ data: bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength) });
  }
}

/** A Wails Stream factory that records every stream the adapter opens. */
function harness() {
  const socks: FakeSocket[] = [];
  const names: string[] = [];
  const factory = (name: string) => {
    const s = new FakeSocket();
    socks.push(s);
    names.push(name);
    return s;
  };
  return { socks, names, load: () => Promise.resolve(factory) };
}

const flush = () => vi.advanceTimersByTimeAsync(0);

async function make(url = 'wails://localhost/api/v1/repo-events?x=1') {
  const h = harness();
  const es = new WailsEventSource(url, h.load, 'wails://localhost/');
  await flush(); // the adapter opens its stream once the runtime resolves
  return { ...h, es, sock: () => h.socks[h.socks.length - 1] };
}

afterEach(() => {
  vi.useRealTimers();
  delete (window as unknown as Record<string, unknown>)[EVENT_TRANSPORT_FLAG];
});

describe('WailsEventSource', () => {
  it('opens the one named stream and sends only the API path and query once the socket opens', async () => {
    vi.useFakeTimers();
    const { names, sock, socks } = await make();
    expect(socks).toHaveLength(1);
    expect(names).toEqual([WAILS_SSE_STREAM]);
    expect(sock().binaryType).toBe('arraybuffer');
    expect(sock().sent).toEqual([]);
    sock().onopen?.({});
    expect(sock().sent).toEqual([JSON.stringify({ path: '/api/v1/repo-events?x=1' })]);
  });

  it('maps open, named events, default message events and lastEventId', async () => {
    vi.useFakeTimers();
    const { sock, es } = await make();
    const seen: string[] = [];
    es.onopen = () => seen.push('open');
    es.onmessage = (e) => seen.push(`message:${e.data}:${e.lastEventId}`);
    es.addEventListener('index', ((e: MessageEvent) => seen.push(`index:${e.data}:${e.lastEventId}`)) as never);
    expect(es.readyState).toBe(0);
    sock().push({ t: 'open' });
    expect(es.readyState).toBe(1);
    sock().push({ t: 'event', event: 'index', data: '{"repo":"a"}', id: '7' });
    sock().push({ t: 'event', data: 'plain' });
    expect(seen).toEqual(['open', 'index:{"repo":"a"}:7', 'message:plain:7']);
  });

  it('delivers named events to every listener, not only the last, and stops after removeEventListener', async () => {
    vi.useFakeTimers();
    const { sock, es } = await make();
    let a = 0, b = 0;
    const fa = (() => { a += 1; }) as never;
    const fb = (() => { b += 1; }) as never;
    es.addEventListener('ready', fa);
    es.addEventListener('ready', fb);
    sock().push({ t: 'event', event: 'ready', data: '{}' });
    es.removeEventListener('ready', fa);
    sock().push({ t: 'event', event: 'ready', data: '{}' });
    expect([a, b]).toEqual([1, 2]);
  });

  it('reports an upstream drop as error with readyState CONNECTING, and open again on recovery', async () => {
    vi.useFakeTimers();
    const { sock, es } = await make();
    const seen: string[] = [];
    es.onerror = () => seen.push(`error:${es.readyState}`);
    es.onopen = () => seen.push('open');
    sock().push({ t: 'open' });
    sock().push({ t: 'error' });
    sock().push({ t: 'open' });
    expect(seen).toEqual(['open', 'error:0', 'open']);
  });

  it('surfaces a server refusal as a final error event carrying the status', async () => {
    vi.useFakeTimers();
    const { sock, es, socks } = await make();
    const errs: { status?: number }[] = [];
    es.addEventListener('error', ((e: { status?: number }) => errs.push(e)) as never);
    sock().push({ t: 'error', closed: true, status: 403 });
    expect(es.readyState).toBe(2);
    expect(errs).toEqual([{ type: 'error', status: 403 }]);
    expect(streamErrorSuffix(errs[0])).toBe(' (HTTP 403)');
    expect(streamErrorSuffix({ type: 'error' })).toBe('');
    // Final: no reopen, however long we wait.
    await vi.advanceTimersByTimeAsync(RECONNECT_MAX_MS * 3);
    expect(socks).toHaveLength(1);
  });

  it('reopens the Wails stream with backoff when it ends, resuming with the last event id', async () => {
    vi.useFakeTimers();
    const { es, socks, sock } = await make();
    const seen: string[] = [];
    es.onerror = () => seen.push(`error:${es.readyState}`);
    es.onopen = () => seen.push('open');
    sock().onopen?.({});
    sock().push({ t: 'open' });
    sock().push({ t: 'event', event: 'index', data: 'x', id: '12' });

    sock().onclose?.({}); // the stream ended under the page
    expect(seen).toEqual(['open', 'error:0']);
    expect(socks).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(RECONNECT_INITIAL_MS - 1);
    expect(socks).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(socks).toHaveLength(2);

    sock().onopen?.({});
    expect(sock().sent).toEqual([JSON.stringify({ path: '/api/v1/repo-events?x=1', last_event_id: '12' })]);
    sock().push({ t: 'open' });
    expect(es.readyState).toBe(1);
    expect(seen).toEqual(['open', 'error:0', 'open']);
  });

  it('doubles the reopen delay while the stream keeps failing, caps it, and resets after an open', async () => {
    vi.useFakeTimers();
    const { es, socks, sock } = await make();
    es.onerror = () => {};
    const gaps: number[] = [];
    let want = RECONNECT_INITIAL_MS;
    for (let i = 0; i < 6; i++) {
      sock().onclose?.({});
      await vi.advanceTimersByTimeAsync(want - 1);
      expect(socks).toHaveLength(i + 1);
      await vi.advanceTimersByTimeAsync(1);
      expect(socks).toHaveLength(i + 2);
      gaps.push(want);
      want = Math.min(want * 2, RECONNECT_MAX_MS);
    }
    expect(gaps).toEqual([500, 1000, 2000, 4000, 5000, 5000]);
    // A successful open resets the schedule.
    sock().push({ t: 'open' });
    sock().onclose?.({});
    await vi.advanceTimersByTimeAsync(RECONNECT_INITIAL_MS);
    expect(socks).toHaveLength(8);
  });

  it('close() is silent: closes the stream, stops reopening, drops later frames', async () => {
    vi.useFakeTimers();
    const { es, socks, sock } = await make();
    let n = 0;
    es.addEventListener('index', (() => { n += 1; }) as never);
    es.onerror = () => { n += 100; };
    const s = sock();
    es.close();
    expect(s.closed).toBe(1);
    expect(es.readyState).toBe(2);
    s.push({ t: 'event', event: 'index', data: 'x' });
    s.onclose?.({});
    await vi.advanceTimersByTimeAsync(RECONNECT_MAX_MS * 2);
    expect(n).toBe(0);
    expect(socks).toHaveLength(1);
  });

  it('close() before the runtime has loaded never opens a stream', async () => {
    vi.useFakeTimers();
    const socks: FakeSocket[] = [];
    let resolve!: (f: (n: string) => WailsSocketLike) => void;
    const es = new WailsEventSource('/api/v1/logs/events', () => new Promise((r) => { resolve = r; }), 'wails://localhost/');
    es.close();
    resolve((n) => { const s = new FakeSocket(); socks.push(s); void n; return s; });
    await flush();
    expect(socks).toHaveLength(0);
  });

  it('retries when the runtime module fails to load', async () => {
    vi.useFakeTimers();
    const socks: FakeSocket[] = [];
    let calls = 0;
    const load = () => {
      calls += 1;
      if (calls === 1) return Promise.reject(new Error('import failed'));
      return Promise.resolve((n: string) => { void n; const s = new FakeSocket(); socks.push(s); return s; });
    };
    new WailsEventSource('/api/v1/logs/events', load, 'wails://localhost/');
    await flush();
    expect(socks).toHaveLength(0);
    await vi.advanceTimersByTimeAsync(RECONNECT_INITIAL_MS);
    expect(socks).toHaveLength(1);
  });

  it('drops a malformed frame instead of throwing', async () => {
    vi.useFakeTimers();
    const { sock, es } = await make();
    let n = 0;
    es.onmessage = () => { n += 1; };
    sock().onmessage?.({ data: new TextEncoder().encode('not json').buffer });
    expect(n).toBe(0);
    sock().push({ t: 'event', data: 'ok' });
    expect(n).toBe(1); // positive control: the same source still delivers
  });
});

describe('openEventStream', () => {
  it('uses the global EventSource when the desktop flag is absent', () => {
    const es = openEventStream('/api/v1/repo-events');
    expect(es).not.toBeInstanceOf(WailsEventSource);
    expect(es.url).toContain('/api/v1/repo-events');
  });

  it('uses the Wails adapter only when /config.js set the flag to wails-stream', () => {
    (window as unknown as Record<string, unknown>)[EVENT_TRANSPORT_FLAG] = WAILS_STREAM_TRANSPORT;
    const es = openEventStream('wails://localhost/api/v1/repo-events');
    expect(es).toBeInstanceOf(WailsEventSource);
    es.close();
    (window as unknown as Record<string, unknown>)[EVENT_TRANSPORT_FLAG] = 'something-else';
    expect(openEventStream('/api/v1/repo-events')).not.toBeInstanceOf(WailsEventSource);
  });
});
