import { Events } from '@wailsio/runtime'
import { LOG_EVENT } from './events.ts'

/**
 * How many lines the window keeps. The file is the archive — this is only what
 * is on screen — and an unbounded array behind a `<pre>` is how a log viewer
 * left open overnight ends up eating a gigabyte.
 */
export const MAX_LINES = 5000

// Module-level rather than React state, and this is load-bearing. Go starts
// emitting the moment the window exists, and its FIRST batch is the file's 64KB
// backlog — the history the user opened the window to read. React's initial
// render (and therefore any useEffect that subscribed) is scheduled, not
// synchronous, so a subscription made from inside a component can easily be
// registered after that batch has already been dispatched, and the backlog
// would be silently dropped. connectLogStream is called from logs.tsx during
// module evaluation instead, which cannot be interleaved with the JavaScript
// Wails injects to deliver an event, so no batch can arrive unheard. The
// component reads whatever accumulated here when it eventually mounts.
let lines: string[] = []
const listeners = new Set<() => void>()

/** Subscribes to store changes. The returned function unsubscribes. */
export function subscribe(listener: () => void): () => void {
  listeners.add(listener)
  return () => {
    listeners.delete(listener)
  }
}

/**
 * The current scrollback. The array identity changes only when the contents
 * do, which is what useSyncExternalStore requires of a snapshot: returning a
 * fresh copy each call would re-render forever.
 */
export function getLines(): string[] {
  return lines
}

/** Adds a batch of lines, dropping the oldest beyond MAX_LINES. */
export function appendLines(batch: string[]): void {
  if (batch.length === 0) return
  const next = lines.concat(batch)
  lines = next.length > MAX_LINES ? next.slice(next.length - MAX_LINES) : next
  for (const listener of listeners) listener()
}

/** Empties the view. The file and the subscription are untouched. */
export function clearLines(): void {
  if (lines.length === 0) return
  lines = []
  for (const listener of listeners) listener()
}

/**
 * Subscribes to the Go side's log batches. Call once, as early as possible —
 * see the note above. The returned function unsubscribes.
 */
export function connectLogStream(): () => void {
  return Events.On(LOG_EVENT, (event) => {
    // Wails sends nil data as an absent field, and a handler that throws takes
    // the whole subscription down with it — after which the window is dead
    // with no visible reason.
    const batch: unknown = event?.data
    if (Array.isArray(batch)) appendLines(batch as string[])
  })
}
