import { describe, it, expect } from 'vitest';
import { WailsEventSource, WAILS_SSE_STREAM, openEventStream } from './eventStream';
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
  name = '';
  send(d: string | ArrayBuffer | Uint8Array) { this.sent.push(String(d)); }
  close() { this.closed += 1; }
  push(frame: unknown) {
    const bytes = new TextEncoder().encode(JSON.stringify(frame));
    this.onmessage?.({ data: bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength) });
  }
}

function make(url = 'wails://localhost/api/v1/repo-events?x=1') {
  const sock = new FakeSocket();
  const es = new WailsEventSource(url, (name) => { sock.name = name; return sock; }, 'wails://localhost/');
  return { sock, es };
}

describe('WailsEventSource', () => {
  it('opens the named stream and sends only the API path and query once the socket opens', () => {
    const { sock } = make();
    expect(sock.name).toBe(WAILS_SSE_STREAM);
    expect(sock.sent).toEqual([]);
    sock.onopen?.({});
    expect(sock.sent).toEqual([JSON.stringify({ path: '/api/v1/repo-events?x=1' })]);
  });

  it('maps open, named events, default message events and lastEventId', () => {
    const { sock, es } = make();
    const seen: string[] = [];
    es.onopen = () => seen.push('open');
    es.onmessage = (e) => seen.push(`message:${e.data}:${e.lastEventId}`);
    es.addEventListener('index', ((e: MessageEvent) => seen.push(`index:${e.data}:${e.lastEventId}`)) as never);
    expect(es.readyState).toBe(0);
    sock.push({ t: 'open' });
    expect(es.readyState).toBe(1);
    sock.push({ t: 'event', event: 'index', data: '{"repo":"a"}', id: '7' });
    sock.push({ t: 'event', data: 'plain' });
    expect(seen).toEqual(['open', 'index:{"repo":"a"}:7', 'message:plain:7']);
  });

  it('reports an upstream drop as error with readyState CONNECTING, and open again on recovery', () => {
    const { sock, es } = make();
    const seen: string[] = [];
    es.onerror = () => seen.push(`error:${es.readyState}`);
    es.onopen = () => seen.push('open');
    sock.push({ t: 'open' });
    sock.push({ t: 'error' });
    sock.push({ t: 'open' });
    expect(seen).toEqual(['open', 'error:0', 'open']);
  });

  it('reports a closed Wails stream as error with readyState CLOSED, and a client close() as silent', () => {
    const a = make();
    let errs = 0;
    a.es.onerror = () => { errs += 1; };
    a.sock.onclose?.({});
    expect(a.es.readyState).toBe(2);
    expect(errs).toBe(1);

    const b = make();
    let n = 0;
    b.es.addEventListener('index', (() => { n += 1; }) as never);
    b.es.close();
    expect(b.sock.closed).toBe(1);
    b.sock.push({ t: 'event', event: 'index', data: 'x' });
    expect(n).toBe(0);
    expect(b.es.readyState).toBe(2);
  });

  it('drops a malformed frame instead of throwing', () => {
    const { sock, es } = make();
    let n = 0;
    es.onmessage = () => { n += 1; };
    sock.onmessage?.({ data: new TextEncoder().encode('not json').buffer });
    expect(n).toBe(0);
  });
});

describe('openEventStream', () => {
  it('uses the global EventSource when the desktop flag is absent', () => {
    const es = openEventStream('/api/v1/repo-events');
    expect(es.url).toBe('/api/v1/repo-events');
  });
});
