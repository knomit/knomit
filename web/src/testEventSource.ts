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

/** Install the fake as the global EventSource and clear recorded instances. */
export function installFakeEventSource() {
  FakeEventSource.instances = [];
  FakeEventSource.emitReadyOnConnect = true;
  (globalThis as unknown as { EventSource: unknown }).EventSource = FakeEventSource;
}

/** Remove it again, so a file that does not install it sees jsdom's absence. */
export function uninstallFakeEventSource() {
  delete (globalThis as unknown as { EventSource?: unknown }).EventSource;
}
