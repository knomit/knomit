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

  // A line whose MESSAGE happens to contain the token must not be mistaken for
  // a line logged at that level: the token is only a level when it sits in the
  // level column, which is where the leading timestamp puts it.
  it('does not match the level token inside a message', () => {
    render(<LogView lines={['10:55:40 INF checked for ERR strings']} level="ERR" />)
    expect(screen.queryByText(/checked for/)).toBeNull()
  })
})
