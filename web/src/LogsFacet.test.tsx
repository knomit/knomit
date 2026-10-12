import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { act, fireEvent, render, screen, within } from '@testing-library/react'
import { LogsFacet } from './LogsFacet'
import { MAX_LINES, appendLines, clearLines } from './logStore'
import { FakeEventSource } from './testEventSource'

beforeEach(() => {
  // The page seeds its scrollback through the store directly; the fake's
  // connect announcement would arrive as an empty backlog and replace it.
  // See testEventSource.ts.
  FakeEventSource.emitReadyOnConnect = false
})

afterEach(() => {
  FakeEventSource.emitReadyOnConnect = true
  clearLines()
})

describe('LogsFacet', () => {
  it('shows lines that arrived before it mounted', () => {
    appendLines(['10:55:40 INF tray up'])
    render(<LogsFacet />)
    expect(screen.getByText(/tray up/)).toBeInTheDocument()
  })

  it('appends lines that arrive while it is mounted', () => {
    render(<LogsFacet />)
    act(() => {
      appendLines(['10:55:47 WRN synthesis disabled'])
    })
    expect(screen.getByText(/synthesis disabled/)).toBeInTheDocument()
  })

  it('narrows to one level and back', () => {
    appendLines(['10:55:40 INF tray up', '10:55:47 WRN synthesis disabled'])
    render(<LogsFacet />)

    const level = screen.getByRole('radiogroup', { name: /level/i })
    fireEvent.click(within(level).getByRole('radio', { name: /^warn$/i }))
    expect(within(level).getByRole('radio', { name: /^warn$/i })).toHaveAttribute('aria-checked', 'true')
    expect(screen.queryByText(/tray up/)).toBeNull()
    expect(screen.getByText(/synthesis disabled/)).toBeInTheDocument()

    fireEvent.click(within(level).getByRole('radio', { name: /^all$/i }))
    expect(within(level).getByRole('radio', { name: /^warn$/i })).toHaveAttribute('aria-checked', 'false')
    expect(screen.getByText(/tray up/)).toBeInTheDocument()
  })

  // Clear empties the view, not the file: the next batch keeps arriving on the
  // same subscription.
  it('clears the view without stopping the stream', () => {
    appendLines(['10:55:40 INF tray up'])
    render(<LogsFacet />)

    fireEvent.click(screen.getByRole('button', { name: /clear/i }))
    expect(screen.queryByText(/tray up/)).toBeNull()

    act(() => {
      appendLines(['10:55:47 WRN synthesis disabled'])
    })
    expect(screen.getByText(/synthesis disabled/)).toBeInTheDocument()
  })

  // A toggle button rather than a checkbox now, but still a real toggle: it
  // reports its state through aria-pressed, so this asserts the same thing.
  it('follows by default and can be told not to', () => {
    render(<LogsFacet />)
    const follow = screen.getByRole('button', { name: /following/i })
    expect(follow).toHaveAttribute('aria-pressed', 'true')
    fireEvent.click(follow)
    expect(follow).toHaveAttribute('aria-pressed', 'false')
  })

  it('says what is being shown, and of how many', () => {
    render(<LogsFacet />)
    act(() => appendLines(['10:00:01 INF one', '10:00:02 WRN two']))
    expect(screen.getByText(/showing/)).toHaveTextContent(/All levels/)
    expect(screen.getByText(/showing/)).toHaveTextContent(/of 2/)
  })

  // The cap is silent otherwise: logStore drops the oldest lines past
  // MAX_LINES, and a log viewer that quietly discards history misleads about
  // what it is showing.
  // Explicit timeout: this test and the one below render a FULL scrollback —
  // 5000 rows, several nodes each — and jsdom is slow enough at that to blow
  // the 5s default on a loaded machine. The cap is the thing under test, so
  // rendering fewer lines would not test it.
  it('admits when the buffer cap has started dropping lines', () => {
    render(<LogsFacet />)
    expect(screen.queryByText(/oldest lines dropped/)).toBeNull()
    act(() =>
      appendLines(Array.from({ length: MAX_LINES + 10 }, (_, i) => `10:00:00 INF line ${i}`)),
    )
    expect(screen.getByText(/oldest lines dropped/)).toBeInTheDocument()
  }, 30_000)

  // The bug this covers: scrolling up to read used to be undone by the next
  // line that arrived, and the only escape was noticing the checkbox.
  it('releases Follow when the user scrolls up, and the pill re-arms it', async () => {
    render(<LogsFacet />)
    act(() => appendLines(Array.from({ length: 40 }, (_, i) => `10:00:0${i % 10} INF line ${i}`)))

    // Let the follow effect's own scroll settle first. It guards against
    // mistaking its own scrollTop assignment for the user scrolling away, and
    // that guard clears on the next frame — a real user is always past it.
    await act(async () => {
      await new Promise((r) => requestAnimationFrame(() => r(null)))
    })

    const scroller = screen.getByTestId('logs-scroller') as HTMLDivElement
    // jsdom has no layout, so the geometry the handler reads is stubbed.
    Object.defineProperty(scroller, 'scrollHeight', { value: 1000, configurable: true })
    Object.defineProperty(scroller, 'clientHeight', { value: 200, configurable: true })
    scroller.scrollTop = 0
    fireEvent.scroll(scroller)

    const follow = screen.getByRole('button', { name: /following/i })
    expect(follow).toHaveAttribute('aria-pressed', 'false')

    act(() => appendLines(['10:00:99 INF later line']))
    const pill = await screen.findByRole('button', { name: /new line/i })
    fireEvent.click(pill)
    expect(follow).toHaveAttribute('aria-pressed', 'true')
  })

  // The pill used to mark its release point with lines.length, which stops
  // rising once the scrollback hits MAX_LINES. From that moment `behind` was
  // permanently 0: on a log busy enough to fill the buffer — the only kind
  // where being dragged to the bottom is a nuisance — the pill silently never
  // appeared again, and scrolling up became a one-way trip.
  it('still counts new lines after the scrollback cap is reached', async () => {
    render(<LogsFacet />)
    act(() => appendLines(Array.from({ length: MAX_LINES }, (_, i) => `10:00:00 INF line ${i}`)))
    await act(async () => {
      await new Promise((r) => requestAnimationFrame(() => r(null)))
    })

    const scroller = screen.getByTestId('logs-scroller') as HTMLDivElement
    Object.defineProperty(scroller, 'scrollHeight', { value: 1000, configurable: true })
    Object.defineProperty(scroller, 'clientHeight', { value: 200, configurable: true })
    scroller.scrollTop = 0
    fireEvent.scroll(scroller)
    expect(screen.getByRole('button', { name: /following/i })).toHaveAttribute(
      'aria-pressed',
      'false',
    )

    // The buffer is already full, so these evict as many as they add and
    // lines.length does not move.
    act(() => appendLines(['10:00:01 INF a', '10:00:02 INF b']))
    expect(await screen.findByRole('button', { name: /2 new lines/i })).toBeInTheDocument()
  }, 30_000)

  // The server tab already says "Logs", so a heading that says it again is pure
  // repetition. Scoped to the page's own subtree.
  it('renders no heading of its own — the server tab already names the page', () => {
    render(<LogsFacet />)
    const page = screen.getByTestId('logs-facet')
    expect(page.querySelector('h1, h2, h3')).toBeNull()
  })

  // "live" is what says this is a stream rather than a dump of a file. With
  // the card gone it rides the status line.
  it('keeps the live hint, on the status line', () => {
    render(<LogsFacet />)
    expect(screen.getByTestId('logs-status')).toHaveTextContent('this server, live')
  })

  // The controls are the facet toolbar, the shell's second level, not a row
  // inside a card.
  it('puts every control in the facet toolbar', () => {
    render(<LogsFacet />)
    const bar = screen.getByTestId('facet-toolbar')
    expect(within(bar).getByRole('radiogroup', { name: /level/i })).toBeInTheDocument()
    expect(within(bar).getByRole('searchbox', { name: /search log lines/i })).toBeInTheDocument()
    expect(within(bar).getByRole('button', { name: /following/i })).toBeInTheDocument()
    expect(within(bar).getByRole('button', { name: /copy/i })).toBeInTheDocument()
    expect(within(bar).getByRole('button', { name: /clear view/i })).toBeInTheDocument()
    // The log is not inside the toolbar.
    expect(within(bar).queryByTestId('logs-scroller')).toBeNull()
  })

  // Asserts the POSITIVE, not just the absence of the old cap: the body, not
  // the viewport, has to be what bounds the log.
  it('sizes the log from the body, not a viewport fraction', () => {
    render(<LogsFacet />)
    const scroller = screen.getByTestId('logs-scroller')
    expect(scroller.style.flex).toBe('1 1 0%')
    expect(scroller.style.minHeight).toBe('0px')
    expect(scroller.style.overflowY).toBe('auto')
    expect(scroller.style.maxHeight).toBe('')
  })

  // There is no card any more, but the gotcha it was about still applies to
  // every flex column between the facet body and the log: left at the flex
  // default (min-height:auto) each floors at its min-content height, which
  // includes the WHOLE scrollback, and the page grows past the window with
  // the status line off screen (kb/gotchas/web/ui/manage-page/
  // flex-min-content-propagation). So the root and the log's frame both
  // carry minHeight 0 and take the body's height with flex, and neither has
  // an overflow of its own.
  it('pins the page and the log frame to the body instead of to the log they contain', () => {
    render(<LogsFacet />)
    const root = screen.getByTestId('logs-facet')
    expect(root.style.flex).toBe('1 1 0%')
    expect(root.style.minHeight).toBe('0px')
    expect(root.style.overflow).toBe('hidden')
    const frame = screen.getByTestId('logs-scroller').parentElement as HTMLDivElement
    expect(frame.style.flex).toBe('1 1 0%')
    expect(frame.style.minHeight).toBe('0px')
    expect(frame.style.overflowY).toBe('')
  })
})
