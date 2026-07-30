import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import { LogsApp } from './LogsApp.tsx'
import { SettingsApp } from './SettingsApp.tsx'

// Placeholder coverage: it exists so the two window entry points are proven to
// render at all, which is what tells us a blank desktop window is a routing
// problem rather than a React one. Tasks 6 and 10 replace these bodies.
describe('desktop window entry points', () => {
  it('renders the settings placeholder', () => {
    render(<SettingsApp />)
    expect(screen.getByRole('heading', { name: 'Settings' })).toBeInTheDocument()
  })

  it('renders the logs placeholder', () => {
    render(<LogsApp />)
    expect(screen.getByRole('heading', { name: 'Logs' })).toBeInTheDocument()
  })
})
