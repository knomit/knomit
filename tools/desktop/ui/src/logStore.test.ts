import { afterEach, describe, expect, it, vi } from 'vitest'

// The store subscribes through @wailsio/runtime, which only exists inside a
// webview. Mock the one function used so the ordering guarantee this module is
// built around can be asserted in jsdom.
type LogHandler = (event: { data?: unknown }) => void

const unsubscribe = vi.fn()
const on = vi.fn<(name: string, handler: LogHandler) => () => void>(() => unsubscribe)
// Indirect on purpose: the factory is hoisted above `on`'s initialisation, so
// it may only REFERENCE it from inside a function body that runs later.
vi.mock('@wailsio/runtime', () => ({
  Events: { On: (name: string, handler: LogHandler) => on(name, handler) },
}))

import { LOG_EVENT } from './events.ts'
import {
  MAX_LINES,
  appendLines,
  clearLines,
  connectLogStream,
  getLines,
  subscribe,
} from './logStore.ts'

afterEach(() => {
  clearLines()
  on.mockClear()
  unsubscribe.mockClear()
})

describe('logStore', () => {
  it('accumulates batches in arrival order', () => {
    appendLines(['a', 'b'])
    appendLines(['c'])
    expect(getLines()).toEqual(['a', 'b', 'c'])
  })

  it('notifies subscribers and stops after unsubscribe', () => {
    const listener = vi.fn()
    const stop = subscribe(listener)
    appendLines(['a'])
    expect(listener).toHaveBeenCalledTimes(1)
    stop()
    appendLines(['b'])
    expect(listener).toHaveBeenCalledTimes(1)
  })

  // useSyncExternalStore compares snapshots by identity and re-reads on every
  // render, so an unchanged store MUST hand back the very same array or React
  // loops forever re-rendering.
  it('returns a stable snapshot until it changes', () => {
    appendLines(['a'])
    const first = getLines()
    expect(getLines()).toBe(first)
    appendLines([])
    expect(getLines()).toBe(first)
    appendLines(['b'])
    expect(getLines()).not.toBe(first)
  })

  it('caps the scrollback at MAX_LINES, dropping the oldest', () => {
    appendLines(Array.from({ length: MAX_LINES + 10 }, (_, i) => `line ${i}`))
    const lines = getLines()
    expect(lines).toHaveLength(MAX_LINES)
    expect(lines[0]).toBe('line 10')
    expect(lines[lines.length - 1]).toBe(`line ${MAX_LINES + 9}`)
  })

  // The whole point of the module-level store: the first batch is the file's
  // backlog and Go emits it as soon as the window exists, which is before React
  // has mounted anything. Lines that land before the first render must still be
  // there when it happens.
  it('keeps lines that arrive before anything subscribes', () => {
    appendLines(['early'])
    const listener = vi.fn()
    subscribe(listener)
    expect(getLines()).toEqual(['early'])
    expect(listener).not.toHaveBeenCalled()
  })

  it('feeds the wails event payload into the store and returns its unsubscribe', () => {
    const stop = connectLogStream()
    expect(on).toHaveBeenCalledWith(LOG_EVENT, expect.any(Function))

    const handler = on.mock.calls[0]![1]
    handler({ data: ['10:55:40 INF tray up'] })
    expect(getLines()).toEqual(['10:55:40 INF tray up'])

    // An event with no payload is not a crash: Emit sends nil data when the
    // batch is empty, and a thrown handler would kill the subscription.
    handler({ data: undefined })
    expect(getLines()).toEqual(['10:55:40 INF tray up'])

    stop()
    expect(unsubscribe).toHaveBeenCalled()
  })
})
