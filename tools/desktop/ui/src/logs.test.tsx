import { describe, expect, it, vi } from 'vitest'

// The ordering inside logs.tsx is the single most load-bearing line in the
// Logs window, and it looks like clutter: moving connectLogStream() into a
// useEffect inside LogsApp is the obvious tidy-up. It would also silently drop
// the backlog — Go emits the file's first 64KB as soon as the window exists,
// Wails queues that JavaScript until the runtime reports ready, and React's
// first render (and therefore any effect) is scheduled rather than synchronous,
// so the batch can be dispatched to an empty listener set.
//
// The race itself is not unit-testable. The ORDER is: with createRoot mocked,
// an effect never runs, so a subscription moved into a component leaves `calls`
// as ['render'] and this fails.
const calls: string[] = []

vi.mock('@wailsio/runtime', () => ({
  Events: {
    On: () => {
      calls.push('on')
      return () => {}
    },
  },
}))

vi.mock('react-dom/client', () => ({
  createRoot: () => ({
    render: () => {
      calls.push('render')
    },
  }),
}))

describe('logs entry point', () => {
  it('subscribes to the log event before React renders', async () => {
    document.body.innerHTML = '<div id="root"></div>'
    await import('./logs.tsx')
    expect(calls).toEqual(['on', 'render'])
  })
})
