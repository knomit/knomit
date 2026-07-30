import { afterEach, describe, expect, it } from 'vitest'
import { act, fireEvent, render, screen } from '@testing-library/react'
import { LogsApp } from './LogsApp.tsx'
import { appendLines, clearLines } from './logStore.ts'

afterEach(() => {
  clearLines()
})

describe('LogsApp', () => {
  it('shows lines that arrived before it mounted', () => {
    appendLines(['10:55:40 INF tray up'])
    render(<LogsApp />)
    expect(screen.getByText(/tray up/)).toBeInTheDocument()
  })

  it('appends lines that arrive while it is mounted', () => {
    render(<LogsApp />)
    act(() => {
      appendLines(['10:55:47 WRN synthesis disabled'])
    })
    expect(screen.getByText(/synthesis disabled/)).toBeInTheDocument()
  })

  it('narrows to one level and back', () => {
    appendLines(['10:55:40 INF tray up', '10:55:47 WRN synthesis disabled'])
    render(<LogsApp />)

    fireEvent.change(screen.getByLabelText(/level/i), { target: { value: 'WRN' } })
    expect(screen.queryByText(/tray up/)).toBeNull()
    expect(screen.getByText(/synthesis disabled/)).toBeInTheDocument()

    fireEvent.change(screen.getByLabelText(/level/i), { target: { value: '' } })
    expect(screen.getByText(/tray up/)).toBeInTheDocument()
  })

  // Clear empties the view, not the file: the next batch keeps arriving on the
  // same subscription.
  it('clears the view without stopping the stream', () => {
    appendLines(['10:55:40 INF tray up'])
    render(<LogsApp />)

    fireEvent.click(screen.getByRole('button', { name: /clear/i }))
    expect(screen.queryByText(/tray up/)).toBeNull()

    act(() => {
      appendLines(['10:55:47 WRN synthesis disabled'])
    })
    expect(screen.getByText(/synthesis disabled/)).toBeInTheDocument()
  })

  it('follows by default and can be told not to', () => {
    render(<LogsApp />)
    const follow = screen.getByLabelText(/follow/i)
    expect(follow).toBeChecked()
    fireEvent.click(follow)
    expect(follow).not.toBeChecked()
  })
})
