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
  // rather than a structured field — filtering is a match on that token.
  it('filters by the console level token', () => {
    render(
      <LogView
        lines={['10:55:40 INF tray up', '10:55:47 WRN synthesis disabled']}
        level="WRN"
      />,
    )
    expect(screen.queryByText(/tray up/)).toBeNull()
    expect(screen.getByText(/synthesis disabled/)).toBeInTheDocument()
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
    expect(screen.getByText(/filter is set to ERR/)).toBeInTheDocument()
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
