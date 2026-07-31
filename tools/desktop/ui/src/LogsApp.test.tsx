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

  // A toggle button rather than a checkbox now, but still a real toggle: it
  // reports its state through aria-pressed, so this asserts the same thing.
  it('follows by default and can be told not to', () => {
    render(<LogsApp />)
    const follow = screen.getByRole('button', { name: /following/i })
    expect(follow).toHaveAttribute('aria-pressed', 'true')
    fireEvent.click(follow)
    expect(follow).toHaveAttribute('aria-pressed', 'false')
  })

  // The bug this covers: scrolling up to read used to be undone by the next
  // line that arrived, and the only escape was noticing the checkbox.
  it('releases Follow when the user scrolls up, and the pill re-arms it', async () => {
    render(<LogsApp />)
    act(() => appendLines(Array.from({ length: 40 }, (_, i) => `10:00:0${i % 10} INF line ${i}`)))

    // Let the follow effect's own scroll settle first. It guards against
    // mistaking its own scrollTop assignment for the user scrolling away, and
    // that guard clears on the next frame — a real user is always past it.
    await act(async () => {
      await new Promise((r) => requestAnimationFrame(() => r(null)))
    })

    const scroller = document.querySelector('.scroller') as HTMLDivElement
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
})
