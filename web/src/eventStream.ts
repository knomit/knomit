// openEventStream is the one place the app constructs a server-sent-event
// connection. Everywhere else asks for a stream by URL and gets back the subset
// of EventSource that the app, streamOutage.ts and the vitest double already
// share.
//
// Default: `new EventSource(url)` — the browser build and the tests (where the
// global is the FakeEventSource double).
//
// Desktop, when /config.js sets `window.__KNOMIT_EVENT_TRANSPORT__` to
// 'wails-stream' (see configInjectingHandler in tools/desktop/app.go, next to
// __KNOMIT_BOOTING__): a Wails Stream to the desktop process, which itself holds
// the SSE subscription to the server over the local socket / pipe and forwards
// each event. Wails' Windows response writer buffers a response until the
// handler returns, so a plain streaming GET through the asset handler delivers
// nothing there; a Stream is a held poll plus a POST whose every response
// completes, so it works on all three platforms. The Go side is
// tools/desktop/uistreams.go; DesktopFrame below mirrors its sseFrame.

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

/**
 * What an `error` event carries. `status` is the HTTP status when the server
 * answered the subscription with a refusal (EventSource itself hides it);
 * absent for a plain connection loss.
 */
export interface StreamErrorEvent {
  type: 'error';
  status?: number;
}

/** The Wails stream name the desktop registers (tools/desktop/uistreams.go). */
export const WAILS_SSE_STREAM = 'knomit-sse';

/** The flag /config.js sets on the desktop. */
export const EVENT_TRANSPORT_FLAG = '__KNOMIT_EVENT_TRANSPORT__';
export const WAILS_STREAM_TRANSPORT = 'wails-stream';

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

// One frame from the desktop. `t` discriminates: an SSE event, or a notice about
// the desktop's own upstream leg that EventSource would surface as open/error.
//   open    the server accepted the subscription
//   event   one dispatched SSE event (id = the last id seen on the stream)
//   error   the upstream leg failed. `closed` false: the desktop is retrying.
//           `closed` true: it gave up (the server refused: `status` says why).
export type DesktopFrame =
  | { t: 'event'; event?: string; data: string; id?: string }
  | { t: 'open' }
  | { t: 'ping' }
  | { t: 'error'; closed?: boolean; status?: number };

const CONNECTING = 0;
const OPEN = 1;
const CLOSED = 2;

/** Reconnect schedule for the page-side leg: bounded, like EventSource's retry. */
export const RECONNECT_INITIAL_MS = 500;
export const RECONNECT_MAX_MS = 5000;

/**
 * A stream that has carried no frame for this long is treated as gone and
 * reopened. The desktop pings every stream it holds every 15 s
 * (ssePingInterval in tools/desktop/uistreams.go), so this is three missed
 * pings. Needed because onclose is not reliable here: Wails reaps a stream
 * session whose page has not polled for 10 minutes (a hidden or suspended
 * window), ends its streams on the Go side without a close reaching the page,
 * and the page's next poll quietly gets a new, empty session. Without this the
 * page keeps a stream that is open with nothing behind it until reload, and
 * every chat it starts sees no event and is ended by the server's stream grace.
 */
export const STALE_STREAM_MS = 45_000;

type Listener = (e: MessageEvent) => void;

/**
 * An EventSource look-alike over a Wails stream.
 *
 * Two legs can fail, and both are reconnected so the caller sees what a native
 * EventSource shows it:
 *  - the desktop's leg to the server: the desktop retries and says so with an
 *    `error` frame (readyState CONNECTING), then `open` when it is back;
 *  - the Wails stream itself ending unexpectedly: the page reopens it after a
 *    bounded backoff, resuming with the last event id;
 *  - the Wails stream dying with NO close reaching the page (a reaped session):
 *    a socket that carries no frame, not even the desktop's ping, for
 *    STALE_STREAM_MS is dropped and reopened the same way.
 * A refusal by the server (4xx/5xx) is final, as it is for EventSource: the
 * `error` event carries the status and readyState is CLOSED.
 */
export class WailsEventSource implements EventSourceLike {
  readonly url: string;
  readyState = CONNECTING;
  onopen: ((e: unknown) => void) | null = null;
  onmessage: ((e: MessageEventLike) => void) | null = null;
  onerror: ((e: unknown) => void) | null = null;

  private listeners = new Map<string, Set<Listener>>();
  private sock: WailsSocketLike | null = null;
  private lastEventId = '';
  private decoder = new TextDecoder();
  private path: string;
  private backoff = RECONNECT_INITIAL_MS;
  private timer: ReturnType<typeof setTimeout> | null = null;
  /** When the current socket was opened or last carried a frame. */
  private lastFrameAt = 0;
  private watchdog: ReturnType<typeof setInterval> | null = null;
  private loadStream: () => Promise<WailsStreamFactory>;

  /**
   * `loadStream` resolves the Wails runtime's Stream factory; it is async
   * because the runtime module is imported lazily (see openEventStream) and
   * injectable so the event mapping is testable with a fake stream.
   */
  constructor(url: string, loadStream: () => Promise<WailsStreamFactory>, base?: string) {
    this.url = url;
    this.loadStream = loadStream;
    // The desktop wants the API path, not an origin: it dials the local
    // socket itself.
    const u = new URL(url, base ?? (typeof location !== 'undefined' ? location.href : 'http://localhost/'));
    this.path = u.pathname + u.search;
    void this.connect();
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
    this.release();
  }

  /** Free the socket and every timer: a CLOSED source holds none. */
  private release(): void {
    if (this.timer) { clearTimeout(this.timer); this.timer = null; }
    if (this.watchdog) { clearInterval(this.watchdog); this.watchdog = null; }
    this.dropSocket();
  }

  /** Reopen a socket that has carried nothing for STALE_STREAM_MS: see there. */
  private checkStale(): void {
    if (this.readyState === CLOSED || !this.sock) return;
    if (Date.now() - this.lastFrameAt < STALE_STREAM_MS) return;
    this.dropSocket();
    this.readyState = CONNECTING;
    this.dispatch('error', { type: 'error' });
    this.scheduleReconnect();
  }

  private dropSocket(): void {
    const s = this.sock;
    this.sock = null;
    if (!s) return;
    s.onopen = s.onmessage = s.onclose = s.onerror = null;
    try { s.close(); } catch { /* already gone */ }
  }

  private async connect(): Promise<void> {
    let factory: WailsStreamFactory;
    try {
      factory = await this.loadStream();
    } catch {
      // The runtime module did not load (not on the desktop, or the page is
      // going away): retry on the same schedule rather than spin.
      this.scheduleReconnect();
      return;
    }
    if (this.readyState === CLOSED) return;
    const sock = factory(WAILS_SSE_STREAM);
    this.sock = sock;
    this.lastFrameAt = Date.now();
    this.watchdog ??= setInterval(() => this.checkStale(), STALE_STREAM_MS / 9);
    sock.binaryType = 'arraybuffer';
    sock.onopen = () => {
      const sub: { path: string; last_event_id?: string } = { path: this.path };
      if (this.lastEventId) sub.last_event_id = this.lastEventId;
      sock.send(JSON.stringify(sub));
    };
    sock.onmessage = (ev) => this.onFrame(ev.data);
    sock.onclose = () => {
      if (this.sock !== sock) return;
      this.sock = null;
      if (this.readyState === CLOSED) return;
      // The Wails stream ended under us (the desktop returned from the handler,
      // the transport reset): nothing upstream is retrying it, so reopen.
      this.readyState = CONNECTING;
      this.dispatch('error', { type: 'error' });
      this.scheduleReconnect();
    };
    sock.onerror = () => { /* surfaced through onclose */ };
  }

  private scheduleReconnect(): void {
    if (this.readyState === CLOSED || this.timer) return;
    const delay = this.backoff;
    this.backoff = Math.min(this.backoff * 2, RECONNECT_MAX_MS);
    this.timer = setTimeout(() => {
      this.timer = null;
      if (this.readyState !== CLOSED) void this.connect();
    }, delay);
  }

  private onFrame(raw: unknown): void {
    if (this.readyState === CLOSED) return;
    // Anything the desktop sends, a ping included, proves the stream is held.
    this.lastFrameAt = Date.now();
    let f: DesktopFrame;
    try {
      const text = typeof raw === 'string' ? raw : this.decoder.decode(raw as ArrayBuffer);
      f = JSON.parse(text) as DesktopFrame;
    } catch {
      return; // an unreadable frame is dropped, as a malformed SSE line is
    }
    if (f.t === 'open') {
      this.backoff = RECONNECT_INITIAL_MS;
      this.readyState = OPEN;
      this.dispatch('open', { type: 'open' });
    } else if (f.t === 'error') {
      const err: StreamErrorEvent = { type: 'error' };
      if (f.status) err.status = f.status;
      if (f.closed) {
        // The server refused the subscription: final, as for EventSource.
        this.readyState = CLOSED;
        this.release();
      } else {
        this.readyState = CONNECTING;
      }
      this.dispatch('error', err);
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

// /wails/runtime.js is served by the Wails framework itself and is an ES module
// exporting Stream; importing it by URL keeps @wailsio/runtime out of the shared
// browser bundle (the same trade externalLinks.ts makes).
const RUNTIME_URL = '/wails/runtime.js';
let runtimeLoading: Promise<WailsStreamFactory> | null = null;

function loadWailsStream(): Promise<WailsStreamFactory> {
  runtimeLoading ??= import(/* @vite-ignore */ RUNTIME_URL).then(
    (m: { Stream: WailsStreamFactory }) => m.Stream,
    (err) => { runtimeLoading = null; throw err; }, // a failed import is retried, not cached
  );
  return runtimeLoading;
}

function wailsStreamsEnabled(): boolean {
  return typeof window !== 'undefined' &&
    (window as unknown as Record<string, unknown>)[EVENT_TRANSPORT_FLAG] === WAILS_STREAM_TRANSPORT;
}

/** The status carried by a stream `error` event, as " (HTTP 403)", or "" when there is none. */
export function streamErrorSuffix(e: unknown): string {
  const s = (e as { status?: unknown } | null | undefined)?.status;
  return typeof s === 'number' && s > 0 ? ` (HTTP ${s})` : '';
}

export function openEventStream(url: string): EventSourceLike {
  if (!wailsStreamsEnabled()) return new EventSource(url) as unknown as EventSourceLike;
  return new WailsEventSource(url, loadWailsStream);
}
