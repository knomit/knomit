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
})
