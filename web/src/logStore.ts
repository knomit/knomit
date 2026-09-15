import { apiUrl } from './api';

/**
 * How many lines the view keeps. The server's ring is the archive — this is
 * only what is on screen — and an unbounded array behind a `<pre>` is how a log
 * viewer left open overnight ends up eating a gigabyte.
 */
export const MAX_LINES = 5000;

// Module-level rather than React state, as in the desktop viewer this is ported
// from. Here the reason is narrower: the component opens the stream itself, so
// nothing can arrive before it mounts. What the module scope still buys is a
// scrollback that survives a re-render and a single subscription shape that
// useSyncExternalStore can read.
let lines: string[] = [];
// Lines received since the last clear, which is NOT lines.length: the
// scrollback is capped and drops the oldest past it, so on any busy log the
// length stops rising and pins at the cap. Anything measuring arrivals off that
// length silently stops counting at the same moment — see the "N new lines"
// pill, which is what this exists for.
let received = 0;
const listeners = new Set<() => void>();

function notify() {
  for (const listener of listeners) listener();
}

/** Subscribes to store changes. The returned function unsubscribes. */
export function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
}

/**
 * The current scrollback. The array identity changes only when the contents
 * do, which is what useSyncExternalStore requires of a snapshot: returning a
 * fresh copy each call would re-render forever.
 */
export function getLines(): string[] {
  return lines;
}

/** Lines received since the last clear, counted rather than measured. */
export function getReceived(): number {
  return received;
}

function capped(next: string[]): string[] {
  return next.length > MAX_LINES ? next.slice(next.length - MAX_LINES) : next;
}

/** Adds a batch of lines, dropping the oldest beyond MAX_LINES. */
export function appendLines(batch: string[]): void {
  if (batch.length === 0) return;
  received += batch.length;
  lines = capped(lines.concat(batch));
  notify();
}

/**
 * Replaces the scrollback wholesale — what a RECONNECT does. The server's ring
 * is the truth on a fresh connection, and re-appending it would duplicate
 * every line the viewer already had.
 */
export function replaceLines(batch: string[]): void {
  lines = capped(batch.slice());
  received = lines.length;
  notify();
}

/**
 * Empties the view. The server's ring and the subscription are untouched.
 *
 * The arrival count resets with it: it counts what is behind the CURRENT view,
 * and a clear is the user saying they are done with everything before now.
 */
export function clearLines(): void {
  if (lines.length === 0 && received === 0) return;
  lines = [];
  received = 0;
  notify();
}

/**
 * A server-reported gap, rendered as a row of its own.
 *
 * Deliberately NOT in the `<RFC3339> <LVL> <message>` shape: the viewer's
 * parser declines it, which means the level filter treats it as unrankable and
 * therefore always shows it. A marker that could be filtered out of view would
 * defeat its own purpose — the whole point is that a gap is never invisible.
 */
function droppedMarker(count: number): string {
  return `— ${count} ${count === 1 ? 'line' : 'lines'} dropped (the server was producing them faster than this view could read) —`;
}

/**
 * Subscribes to GET /api/v1/logs/events. The returned function closes it.
 *
 * Reached through apiUrl, i.e. the same API base every other call uses and the
 * one /config.js injects in the desktop app — the log stream is part of the
 * API, not a second transport.
 *
 * The backlog is BUFFERED until `ready` rather than appended as it arrives,
 * then REPLACES the scrollback. Both halves matter:
 *
 * Buffered, because the server replays its ring before it says `ready` — so
 * appending each line as it arrives and reconciling afterwards would already
 * have duplicated the whole replay, and clearing on `ready` would discard it.
 *
 * Replaces rather than appends, because every connect gets the same treatment
 * and the server's ring is the authority at that moment. A first connect finds
 * an empty store, so the two are identical there. A reconnect and a REMOUNT —
 * the user switching Manage tabs away and back, which closes this stream and
 * opens a new one against the same module-level store — would each otherwise
 * append a backlog the store already held, doubling every line.
 *
 * The cost is that a "Clear view" is undone by a later reconnect, and that a
 * scrollback grown past the server's ring depth shrinks back to it. Both are
 * the honest answer: the server can only vouch for what it kept.
 */
export function connectLogStream(): () => void {
  const es = new EventSource(apiUrl('/api/v1/logs/events'));
  // Per-connection state.
  let backlog: string[] = [];
  let inBacklog = true;

  // Fires for the first connection AND for every transparent EventSource
  // reconnect, which is what makes it the reliable place to re-enter the
  // backlog phase.
  const onOpen = () => { backlog = []; inBacklog = true; };

  const onLine = (e: unknown) => {
    const line = payload<{ line?: unknown }>(e)?.line;
    if (typeof line !== 'string') return; // malformed: dropped, not trusted
    if (inBacklog) backlog.push(line);
    else appendLines([line]);
  };

  const onReady = () => {
    replaceLines(backlog);
    backlog = [];
    inBacklog = false;
  };

  const onDropped = (e: unknown) => {
    const count = payload<{ count?: unknown }>(e)?.count;
    if (typeof count !== 'number' || count <= 0) return;
    appendLines([droppedMarker(count)]);
  };

  es.addEventListener('open', onOpen);
  es.addEventListener('line', onLine);
  es.addEventListener('ready', onReady);
  es.addEventListener('dropped', onDropped);
  return () => {
    es.removeEventListener('open', onOpen);
    es.removeEventListener('line', onLine);
    es.removeEventListener('ready', onReady);
    es.removeEventListener('dropped', onDropped);
    es.close();
  };
}

/** Parses an SSE event's JSON data, or undefined if it is not usable. */
function payload<T>(e: unknown): T | undefined {
  const data = (e as { data?: unknown } | undefined)?.data;
  if (typeof data !== 'string' || data === '') return undefined;
  try {
    return JSON.parse(data) as T;
  } catch {
    return undefined;
  }
}
