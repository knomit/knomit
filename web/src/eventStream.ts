// openEventStream is the one place the app constructs a server-sent-event
// connection. Everywhere else asks for a stream by URL and gets back the subset
// of EventSource that the app, streamOutage.ts and the vitest double already
// share.
//
// Default: `new EventSource(url)` — the browser build, the tests (the global is
// the FakeEventSource double) and the desktop on any transport that can carry a
// plain GET stream.
//
// Desktop with `window.__KNOMIT_EVENT_TRANSPORT__ === 'wails-stream'` (injected
// by /config.js): a Wails Stream to the desktop process, which itself holds the
// SSE subscription to the server over the local socket / pipe and forwards each
// frame. No listener, no port, and it works with the Windows response writer,
// which buffers a plain streaming response until the handler returns.

export interface EventSourceLike {
  readonly url: string;
  readonly readyState: number;
  onopen: ((e: unknown) => void) | null;
  onmessage: ((e: MessageEventLike) => void) | null;
  onerror: ((e: unknown) => void) | null;
  addEventListener(type: string, fn: (e: MessageEvent) => void): void;
  removeEventListener(type: string, fn: (e: MessageEvent) => void): void;
  close(): void;
}

export interface MessageEventLike {
  type: string;
  data: string;
  lastEventId: string;
}

/** The Wails stream name the desktop registers (tools/desktop/spike_streams.go). */
export const WAILS_SSE_STREAM = 'knomit-sse';

// The subset of the Wails runtime's WailsSocket this adapter uses.
export interface WailsSocketLike {
  readyState: number;
  binaryType: string;
  onopen: ((e: unknown) => void) | null;
  onmessage: ((e: { data: unknown }) => void) | null;
  onclose: ((e: unknown) => void) | null;
  onerror: ((e: unknown) => void) | null;
  send(data: string | ArrayBuffer | Uint8Array): void;
  close(): void;
}

export type WailsStreamFactory = (name: string) => WailsSocketLike;

// One frame from the desktop. `t` discriminates: an SSE event, or a control
// notice about the upstream leg that EventSource would surface as open/error.
export type DesktopFrame =
  | { t: 'event'; event?: string; data: string; id?: string }
  | { t: 'open' }
  | { t: 'error'; closed?: boolean };

const CONNECTING = 0;
const OPEN = 1;
const CLOSED = 2;

type Listener = (e: MessageEvent) => void;

/**
 * An EventSource look-alike over a Wails stream. `connect` is injectable so the
 * event mapping is unit-testable with a fake stream.
 */
export class WailsEventSource implements EventSourceLike {
  readonly url: string;
  readyState = CONNECTING;
  onopen: ((e: unknown) => void) | null = null;
  onmessage: ((e: MessageEventLike) => void) | null = null;
  onerror: ((e: unknown) => void) | null = null;

  private listeners = new Map<string, Set<Listener>>();
  private sock: WailsSocketLike;
  private lastEventId = '';
  private decoder = new TextDecoder();

  constructor(url: string, connect: WailsStreamFactory, base?: string) {
    this.url = url;
    // The desktop wants the API path, not an origin: it dials the local
    // socket itself.
    const u = new URL(url, base ?? (typeof location !== 'undefined' ? location.href : 'http://localhost/'));
    const path = u.pathname + u.search;
    this.sock = connect(WAILS_SSE_STREAM);
    this.sock.binaryType = 'arraybuffer';
    this.sock.onopen = () => {
      if (this.readyState === CLOSED) return;
      this.sock.send(JSON.stringify({ path }));
    };
    this.sock.onmessage = (ev) => this.onFrame(ev.data);
    this.sock.onclose = () => {
      if (this.readyState === CLOSED) return;
      // The Wails stream itself ended (page reload, window closing, app
      // shutdown): nothing will reconnect it, which is EventSource's CLOSED.
      this.readyState = CLOSED;
      this.dispatch('error', { type: 'error' });
    };
    this.sock.onerror = () => { /* surfaced through onclose */ };
  }

  addEventListener(type: string, fn: Listener): void {
    let set = this.listeners.get(type);
    if (!set) { set = new Set(); this.listeners.set(type, set); }
    set.add(fn);
  }

  removeEventListener(type: string, fn: Listener): void {
    this.listeners.get(type)?.delete(fn);
  }

  close(): void {
    if (this.readyState === CLOSED) return;
    this.readyState = CLOSED;
    this.listeners.clear();
    this.onopen = this.onmessage = this.onerror = null;
    try { this.sock.close(); } catch { /* already gone */ }
  }

  private onFrame(raw: unknown): void {
    if (this.readyState === CLOSED) return;
    let f: DesktopFrame;
    try {
      const text = typeof raw === 'string' ? raw : this.decoder.decode(raw as ArrayBuffer);
      f = JSON.parse(text) as DesktopFrame;
    } catch {
      return; // an unreadable frame is dropped, as a malformed SSE line is
    }
    if (f.t === 'open') {
      this.readyState = OPEN;
      this.dispatch('open', { type: 'open' });
    } else if (f.t === 'error') {
      // The desktop's upstream leg dropped and it is retrying: EventSource's
      // CONNECTING-with-error. `closed` is the desktop giving up.
      this.readyState = f.closed ? CLOSED : CONNECTING;
      this.dispatch('error', { type: 'error' });
    } else if (f.t === 'event') {
      if (f.id) this.lastEventId = f.id;
      const type = f.event || 'message';
      this.dispatch(type, { type, data: f.data, lastEventId: this.lastEventId });
    }
  }

  private dispatch(type: string, e: MessageEventLike | { type: string }): void {
    if (type === 'open') this.onopen?.(e);
    else if (type === 'error') this.onerror?.(e);
    else if (type === 'message') this.onmessage?.(e as MessageEventLike);
    for (const fn of [...(this.listeners.get(type) ?? [])]) (fn as (e: unknown) => void)(e);
  }
}

const RUNTIME_URL = '/wails/runtime.js';
type RuntimeModule = { Stream: WailsStreamFactory };
let runtime: RuntimeModule | null = null;
let runtimeLoading: Promise<RuntimeModule> | null = null;

function loadRuntime(): Promise<RuntimeModule> {
  // /wails/runtime.js is served by the Wails framework itself and is an ES
  // module exporting Stream; importing it by URL keeps @wailsio/runtime out of
  // the shared browser bundle (the same trade externalLinks.ts makes).
  runtimeLoading ??= import(/* @vite-ignore */ RUNTIME_URL).then((m) => {
    runtime = m as RuntimeModule;
    return runtime;
  });
  return runtimeLoading;
}

/** Kick off the runtime import early so the first stream does not wait on it. */
export function preloadWailsRuntime(): void {
  if (wailsStreamsEnabled()) void loadRuntime();
}

function wailsStreamsEnabled(): boolean {
  return typeof window !== 'undefined' &&
    (window as Window & { __KNOMIT_EVENT_TRANSPORT__?: string }).__KNOMIT_EVENT_TRANSPORT__ === 'wails-stream';
}

export function openEventStream(url: string): EventSourceLike {
  if (!wailsStreamsEnabled()) return new EventSource(url) as unknown as EventSourceLike;
  if (runtime) return new WailsEventSource(url, runtime.Stream);
  // The runtime module has not loaded yet: hand back the adapter now (callers
  // attach listeners synchronously) and connect once it arrives.
  let inner: WailsEventSource | null = null;
  const pending = new Map<string, Set<Listener>>();
  const shim = {
    url,
    get readyState() { return inner ? inner.readyState : CONNECTING; },
    onopen: null as ((e: unknown) => void) | null,
    onmessage: null as ((e: MessageEventLike) => void) | null,
    onerror: null as ((e: unknown) => void) | null,
    addEventListener(type: string, fn: Listener) {
      if (inner) { inner.addEventListener(type, fn); return; }
      let s = pending.get(type);
      if (!s) { s = new Set(); pending.set(type, s); }
      s.add(fn);
    },
    removeEventListener(type: string, fn: Listener) {
      if (inner) inner.removeEventListener(type, fn); else pending.get(type)?.delete(fn);
    },
    close() { closed = true; inner?.close(); },
  };
  let closed = false;
  void loadRuntime().then((rt) => {
    if (closed) return;
    inner = new WailsEventSource(url, rt.Stream);
    inner.onopen = (e) => shim.onopen?.(e);
    inner.onmessage = (e) => shim.onmessage?.(e);
    inner.onerror = (e) => shim.onerror?.(e);
    for (const [t, set] of pending) for (const fn of set) inner.addEventListener(t, fn);
    pending.clear();
  });
  return shim;
}
