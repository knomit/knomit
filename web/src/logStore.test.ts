import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { FakeEventSource, installFakeEventSource } from './testEventSource';
import {
  MAX_LINES,
  appendLines,
  clearLines,
  connectLogStream,
  getLines,
  getReceived,
  subscribe,
} from './logStore';

beforeEach(() => {
  installFakeEventSource();
  // These tests drive the connect sequence by hand — backlog frames, then
  // `ready`, then live lines — because the ORDER is what they are about. The
  // fake's own connect announcement would interleave a second, empty backlog
  // with theirs.
  FakeEventSource.emitReadyOnConnect = false;
});
afterEach(() => { clearLines(); vi.clearAllMocks(); });

describe('logStore', () => {
  it('accumulates batches in arrival order', () => {
    appendLines(['a', 'b']);
    appendLines(['c']);
    expect(getLines()).toEqual(['a', 'b', 'c']);
  });

  it('notifies subscribers and stops after unsubscribe', () => {
    const listener = vi.fn();
    const stop = subscribe(listener);
    appendLines(['a']);
    expect(listener).toHaveBeenCalledTimes(1);
    stop();
    appendLines(['b']);
    expect(listener).toHaveBeenCalledTimes(1);
  });

  // useSyncExternalStore compares snapshots by identity and re-reads on every
  // render, so an unchanged store MUST hand back the very same array or React
  // loops forever re-rendering.
  it('returns a stable snapshot until it changes', () => {
    appendLines(['a']);
    const first = getLines();
    expect(getLines()).toBe(first);
    appendLines([]);
    expect(getLines()).toBe(first);
    appendLines(['b']);
    expect(getLines()).not.toBe(first);
  });

  it('caps the scrollback at MAX_LINES, dropping the oldest', () => {
    appendLines(Array.from({ length: MAX_LINES + 10 }, (_, i) => `line ${i}`));
    const lines = getLines();
    expect(lines).toHaveLength(MAX_LINES);
    expect(lines[0]).toBe('line 10');
    expect(lines[lines.length - 1]).toBe(`line ${MAX_LINES + 9}`);
  });

  // The count exists precisely because lines.length cannot serve: past the cap
  // it stops rising, so anything measuring arrivals off it silently stops
  // counting at the same moment.
  it('keeps counting arrivals after the scrollback cap stops the length rising', () => {
    appendLines(Array.from({ length: MAX_LINES }, (_, i) => `line ${i}`));
    expect(getReceived()).toBe(MAX_LINES);
    appendLines(['one more', 'and another']);
    expect(getLines()).toHaveLength(MAX_LINES);
    expect(getReceived()).toBe(MAX_LINES + 2);
  });

  it('resets the arrival count when the view is cleared', () => {
    appendLines(['a', 'b']);
    expect(getReceived()).toBe(2);
    clearLines();
    expect(getReceived()).toBe(0);
    appendLines(['c']);
    expect(getReceived()).toBe(1);
  });
});

/** The stream opened by connectLogStream, plus a helper to drive a connection. */
function stream(): FakeEventSource {
  expect(FakeEventSource.instances).toHaveLength(1);
  return FakeEventSource.instances[0];
}

describe('logStore over SSE', () => {
  it('opens the API stream and replays the backlog before ready', () => {
    const stop = connectLogStream();
    const es = stream();
    expect(es.url).toContain('/api/v1/logs/events');

    es.emit('open');
    es.emit('line', { line: 'a' });
    es.emit('line', { line: 'b' });
    es.emit('ready', { retained: 2, max: 2000 });
    expect(getLines()).toEqual(['a', 'b']);

    es.emit('line', { line: 'c' });
    expect(getLines()).toEqual(['a', 'b', 'c']);
    stop();
    expect(es.closeCount).toBe(1);
  });

  // THE reconnect case, and the reason the backlog is buffered rather than
  // appended as it arrives. The server sends the backlog BEFORE `ready`, so a
  // client that appended each line and only then learned it was a reconnect
  // would already have duplicated the whole replay — and a client that cleared
  // on `ready` would throw away the very backlog it just received.
  it('replaces the scrollback on a reconnect instead of duplicating it', () => {
    connectLogStream();
    const es = stream();
    es.emit('open');
    es.emit('line', { line: 'a' });
    es.emit('ready', { retained: 1, max: 2000 });
    es.emit('line', { line: 'b' });
    expect(getLines()).toEqual(['a', 'b']);

    // The connection drops and EventSource reconnects by itself; the server
    // replays its ring, which still holds both lines.
    es.emit('open');
    es.emit('line', { line: 'a' });
    es.emit('line', { line: 'b' });
    es.emit('ready', { retained: 2, max: 2000 });
    expect(getLines()).toEqual(['a', 'b']);

    es.emit('line', { line: 'c' });
    expect(getLines()).toEqual(['a', 'b', 'c']);
  });

  // Switching to another Manage tab unmounts the page and closes the stream;
  // coming back opens a new one. The store is module-level and still holds the
  // old scrollback, and the server replays the same ring — so a connect that
  // APPENDED its backlog would double every line each time the user looked
  // away and back.
  it('does not duplicate the backlog when the page is reopened', () => {
    const stop = connectLogStream();
    const first = stream();
    first.emit('open');
    first.emit('line', { line: 'a' });
    first.emit('line', { line: 'b' });
    first.emit('ready', { retained: 2, max: 2000 });
    expect(getLines()).toEqual(['a', 'b']);
    stop();

    // Reopened: a second EventSource, the same ring.
    connectLogStream();
    const second = FakeEventSource.instances[1];
    second.emit('open');
    second.emit('line', { line: 'a' });
    second.emit('line', { line: 'b' });
    second.emit('ready', { retained: 2, max: 2000 });
    expect(getLines()).toEqual(['a', 'b']);
  });

  // A gap the server admits to must be visible in the scrollback. A viewer
  // that resumed quietly would present a discontinuous log as a continuous one.
  it('marks a server-side drop as a visible row', () => {
    connectLogStream();
    const es = stream();
    es.emit('open');
    es.emit('ready', { retained: 0, max: 2000 });
    es.emit('line', { line: 'a' });
    es.emit('dropped', { count: 7 });
    es.emit('line', { line: 'b' });

    const lines = getLines();
    expect(lines).toHaveLength(3);
    expect(lines[1]).toContain('7');
    expect(lines[1].toLowerCase()).toContain('dropped');
    // Unparseable by the level filter, which is what keeps it always visible:
    // a marker hidden by the current level would defeat its own purpose.
    expect(lines[1].startsWith('2')).toBe(false);
  });

  it('ignores a malformed payload rather than dying on it', () => {
    connectLogStream();
    const es = stream();
    es.emit('open');
    es.emit('ready', { retained: 0, max: 2000 });
    es.emit('line', { nonsense: true });
    es.emit('line', { line: 'good' });
    expect(getLines()).toEqual(['good']);
  });
});
