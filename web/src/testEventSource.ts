// Fake EventSource for tests. jsdom has none, and the code under test
// constructs one directly, so tests install this global and drive events by
// hand.
//
// Shared because there are now two independent streams to test — App's branch
// stream and the client-sessions change stream — and a second copy would be
// two definitions of "what a stream does" that could drift apart.
export class FakeEventSource {
  static instances: FakeEventSource[] = [];
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;

  url: string;
  readyState = FakeEventSource.OPEN;
  closeCount = 0;
  closedByClient = false;
  private listeners = new Map<string, Set<(e: unknown) => void>>();

  /**
   * Whether a new stream announces itself the way the real server does:
   * `open`, then `ready`, as soon as listeners are attached.
   *
   * Defaults to TRUE because that is what the server actually does, and a fake
   * that stayed silent on connect hid a real bug — the client-sessions hook
   * treated the connect-time `ready` as a change and every mount paid for two
   * list reads, invisibly, because no test ever delivered one.
   *
   * Tests that drive the connect sequence by hand (the log stream's, which
   * assert on backlog/ready ordering) set this false and say so.
   */
  static emitReadyOnConnect = true;

  constructor(url: string) {
    this.url = url;
    FakeEventSource.instances.push(this);
    if (FakeEventSource.emitReadyOnConnect) {
      // A microtask, not synchronous: the caller has not attached its
      // listeners yet at construction time, exactly as a real connection has
      // not completed yet.
      queueMicrotask(() => {
        this.emit('open');
        this.emit('ready', {});
      });
    }
  }

  addEventListener(type: string, fn: (e: unknown) => void) {
    let set = this.listeners.get(type);
    if (!set) { set = new Set(); this.listeners.set(type, set); }
    set.add(fn);
  }

  removeEventListener(type: string, fn: (e: unknown) => void) {
    this.listeners.get(type)?.delete(fn);
  }

  close() { this.closeCount += 1; this.closedByClient = true; this.readyState = FakeEventSource.CLOSED; }

  /**
   * Deliver an event to every listener registered for `type`. A stream the
   * client closed delivers nothing more, mirroring the real EventSource —
   * this is what makes "the old subscription is really torn down" observable.
   */
  emit(type: string, data?: unknown) {
    if (this.closedByClient) return;
    const payload = { type, data: data === undefined ? '' : JSON.stringify(data) };
    for (const fn of [...(this.listeners.get(type) ?? [])]) fn(payload);
  }
}

/**
 * Streams whose URL contains `substr`, oldest first.
 *
 * Select by URL, never by index into `instances`. The app holds SEVERAL streams
 * at once — the branch stream, the server-wide index stream, sessions, logs —
 * and which one is `instances[0]` depends on mount order, so an index-based
 * test asserts on whichever stream happened to open first and breaks the day a
 * new one is added. It did: adding the index stream moved every App stream by
 * one.
 */
export function streamsMatching(substr: string): FakeEventSource[] {
  return FakeEventSource.instances.filter(es => es.url.includes(substr));
}

/** The most recent stream whose URL contains `substr`, or undefined. */
export function latestStream(substr: string): FakeEventSource | undefined {
  const all = streamsMatching(substr);
  return all[all.length - 1];
}

/** Install the fake as the global EventSource and clear recorded instances. */
export function installFakeEventSource() {
  FakeEventSource.instances = [];
  FakeEventSource.emitReadyOnConnect = true;
  (globalThis as unknown as { EventSource: unknown }).EventSource = FakeEventSource;
}

/**
 * Hand back the baseline test-setup.ts installs for EVERY test: this same
 * FakeEventSource class, with its default ready-on-connect behaviour. It used
 * to DELETE the global, which left nothing for the setup file's cleanup() —
 * which runs after a test file's own afterEach under vitest's "stack" hook
 * order — when the unmount flushed a pending passive effect that constructs an
 * EventSource: "EventSource is not defined" mid-teardown, and the
 * half-torn-down tree leaking into the next test (knomit#270). Restoring the
 * class needs no import of the setup file: the baseline IS this class.
 */
export function uninstallFakeEventSource() {
  FakeEventSource.emitReadyOnConnect = true;
  (globalThis as unknown as { EventSource: unknown }).EventSource = FakeEventSource;
}

/** The URL of the page's one chat events stream. */
export const CHAT_EVENTS_STREAM = '/api/v1/chats/events';

/**
 * The page's chat events stream, seen as one chat's: `emit` tags the payload
 * with `chat` the way the server does (a page-level `ready` carries none).
 */
export function chatStream(chatId = 'chat-a'): { emit: (type: string, data?: object) => void; raw: FakeEventSource } | undefined {
  const raw = latestStream(CHAT_EVENTS_STREAM);
  if (!raw) return undefined;
  return { raw, emit: (type, data) => raw.emit(type, type === 'ready' ? data : { chat: chatId, ...(data ?? {}) }) };
}
