import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import { LogView } from './LogView.tsx'

describe('LogView', () => {
  it('renders each line', () => {
    render(<LogView lines={['10:55:40 INF tray up', '10:55:47 WRN synthesis disabled']} />)
    expect(screen.getByText(/tray up/)).toBeInTheDocument()
    expect(screen.getByText(/synthesis disabled/)).toBeInTheDocument()
  })

  // The file is console-formatted text, so the level is a token in the line
  // rather than a structured field — the filter reads that token. It is a
  // FLOOR, not an equality test: picking a level asks for that level and
  // everything louder.
  it('shows the chosen level and drops quieter ones', () => {
    render(
      <LogView
        lines={['10:55:40 INF tray up', '10:55:47 WRN synthesis disabled']}
        level="WRN"
      />,
    )
    expect(screen.queryByText(/tray up/)).toBeNull()
    expect(screen.getByText(/synthesis disabled/)).toBeInTheDocument()
  })

  // The bug this replaced: an equality test meant choosing Warn HID every
  // error, so a user filtering for trouble was shown strictly less of it than
  // "All" showed. The most confidently wrong a filter can be.
  it('does not hide errors when filtering for warnings', () => {
    render(
      <LogView
        lines={[
          '10:55:40 INF tray up',
          '10:55:47 WRN synthesis disabled',
          '10:55:49 ERR reconcile failed',
          '10:55:50 FTL out of disk',
        ]}
        level="WRN"
      />,
    )
    expect(screen.getByText(/synthesis disabled/)).toBeInTheDocument()
    expect(screen.getByText(/reconcile failed/)).toBeInTheDocument()
    expect(screen.getByText(/out of disk/)).toBeInTheDocument()
    expect(screen.queryByText(/tray up/)).toBeNull()
  })

  // A line we could not parse has no rank, so no threshold can exclude it.
  // Dropping a line we failed to read is worse than showing one the filter did
  // not ask for — silence is indistinguishable from "nothing happened".
  it('never filters out a line it cannot rank', () => {
    render(
      <LogView
        lines={['10:55:40 INF tray up', 'no log file at /var/log/knomit.log yet']}
        level="ERR"
      />,
    )
    expect(screen.getByText(/no log file at/)).toBeInTheDocument()
  })

  // Asserted on rendered text rather than with getByText: highlighting splits
  // the message across <mark> boundaries, so a matcher scoped to one node
  // cannot see a phrase that spans the match.
  it('narrows to lines matching the search', () => {
    const { container } = render(
      <LogView
        lines={['10:55:40 INF tray up', '10:55:47 INF reconcile ok repo=core']}
        query="reconcile"
      />,
    )
    const shown = [...container.querySelectorAll('.logline')].map((n) => n.textContent)
    expect(shown).toHaveLength(1)
    expect(shown[0]).toContain('reconcile ok repo=core')
  })

  it('highlights the match inside the message', () => {
    const { container } = render(
      <LogView lines={['10:55:47 INF reconcile ok repo=core']} query="conc" />,
    )
    const mark = container.querySelector('mark')
    expect(mark).not.toBeNull()
    expect(mark).toHaveTextContent('conc')
  })

  it('matches case-insensitively', () => {
    const { container } = render(
      <LogView lines={['10:55:47 INF Reconcile OK']} query="reconcile" />,
    )
    const shown = [...container.querySelectorAll('.logline')].map((n) => n.textContent)
    expect(shown).toHaveLength(1)
    // The original casing survives — the query is how it was FOUND, not how it
    // is displayed.
    expect(shown[0]).toContain('Reconcile OK')
  })

  // The filter is a substring, not a pattern. A user typing an unbalanced
  // bracket is searching, not writing a regex, and must not be met with a
  // thrown SyntaxError.
  it('does not throw on a query containing regex metacharacters', () => {
    const { container } = render(
      <LogView lines={['10:55:47 INF parse failed at ( char 3']} query="at (" />,
    )
    const shown = [...container.querySelectorAll('.logline')].map((n) => n.textContent)
    expect(shown).toHaveLength(1)
    expect(shown[0]).toContain('parse failed at ( char 3')
  })

  // A blank window is the one state that cannot explain itself. It looks the
  // same whether nothing has been logged yet or the window is tailing a file
  // nothing writes to, which is exactly the confusion a Logs window exists to
  // remove.
  it('explains an empty view instead of rendering nothing', () => {
    render(<LogView lines={[]} />)
    expect(screen.getByText(/Waiting for log output/)).toBeInTheDocument()
  })

  // Distinguishable from "nothing has arrived": the user set this filter, and
  // telling them so is the difference between a bug and a control they can undo.
  it('says when the filter is what is hiding everything', () => {
    render(<LogView lines={['10:55:40 INF tray up']} level="ERR" />)
    expect(screen.getByText(/ERR or above/)).toBeInTheDocument()
    expect(screen.queryByText(/Waiting for log output/)).toBeNull()
  })

  // A line whose MESSAGE happens to contain the token must not be mistaken for
  // a line logged at that level: the token is only a level when it sits in the
  // level column, which is where the leading timestamp puts it.
  it('does not match the level token inside a message', () => {
    render(<LogView lines={['10:55:40 INF checked for ERR strings']} level="ERR" />)
    expect(screen.queryByText(/checked for/)).toBeNull()
  })

  // `log.format = "json"` is a first-class choice in the Settings dialog, so the
  // file the window tails is not always console-shaped. These lines are captured
  // VERBATIM from internal/logging with Format: "json" — not hand-written — so
  // they carry zerolog's real key order and its real reserved-field names.
  describe('json-format lines', () => {
    const JSON_LINES = [
      '{"level":"debug","time":"2026-07-31T15:20:53-04:00","message":"tick"}',
      '{"level":"info","api":"http://127.0.0.1:19278","port":19278,"time":"2026-07-31T15:20:53-04:00","message":"knomit-desktop server up (API-only)"}',
      '{"level":"warn","time":"2026-07-31T15:20:53-04:00","message":"selfupdate"}',
      '{"level":"error","error":"EOF","time":"2026-07-31T15:20:53-04:00","message":"boom"}',
    ]

    // The regression this fixes. Every JSON line used to parse to `undefined`,
    // which atOrAbove reads as "unrankable, therefore always show" — so the
    // Level menu did not empty the pane, it silently stopped filtering, and a
    // control that appears to work is not one anybody re-examines.
    it('applies the severity floor instead of showing everything', () => {
      const { container } = render(<LogView lines={JSON_LINES} level="WRN" />)
      const shown = [...container.querySelectorAll('.logline')].map((n) => n.textContent)
      expect(shown).toHaveLength(2)
      expect(shown.join('\n')).toContain('selfupdate')
      expect(shown.join('\n')).toContain('boom')
      expect(shown.join('\n')).not.toContain('tick')
    })

    // The level column has to speak the same three-letter vocabulary as the
    // console format, or the filter and the rendered token disagree on screen.
    it('renders zerolog level names as console tokens', () => {
      const { container } = render(<LogView lines={JSON_LINES} />)
      const levels = [...container.querySelectorAll('.logline')].map((n) =>
        n.getAttribute('data-level'),
      )
      expect(levels).toEqual(['DBG', 'INF', 'WRN', 'ERR'])
    })

    // Structured fields become the same dimmed `key=value` tail the console
    // format produces, so splitTail and the rest of the render path need no
    // second code path.
    it('flattens structured fields into the key=value tail', () => {
      const { container } = render(<LogView lines={[JSON_LINES[1]]} />)
      const tail = container.querySelector('.tail')
      expect(tail).toHaveTextContent('api=http://127.0.0.1:19278')
      expect(tail).toHaveTextContent('port=19278')
      // Reserved keys belong to the columns, not the tail.
      expect(tail).not.toHaveTextContent('level=')
      expect(tail).not.toHaveTextContent('time=')
    })

    // The RFC3339 stamp recedes to a clock time exactly as it does for the
    // console format — the date is recovered by the day divider and the title.
    it('shows the clock time and keeps the full stamp on hover', () => {
      const { container } = render(<LogView lines={[JSON_LINES[0]]} />)
      const ts = container.querySelector('.ts')
      expect(ts).toHaveTextContent('15:20:53')
      expect(ts).toHaveAttribute('title', '2026-07-31T15:20:53-04:00')
    })

    // A half-flushed line is the ordinary case at the tail of a file being
    // written to. It must render raw rather than throw, and stay unfiltered.
    it('renders a truncated json line raw rather than throwing', () => {
      const partial = '{"level":"error","time":"2026-07-31T15:20:53-04:00","mess'
      const { container } = render(<LogView lines={[partial]} level="ERR" />)
      const shown = [...container.querySelectorAll('.logline')].map((n) => n.textContent)
      expect(shown).toEqual([partial])
    })

    // Search still narrows what the threshold allowed, over the reconstructed
    // message rather than the raw JSON — otherwise a user searching for a field
    // name would match key text the window never shows them.
    it('searches the rendered message, not the raw record', () => {
      const { container } = render(<LogView lines={JSON_LINES} query="api=" />)
      const shown = [...container.querySelectorAll('.logline')].map((n) => n.textContent)
      expect(shown).toHaveLength(1)
      expect(shown[0]).toContain('knomit-desktop server up')
    })
  })
})
