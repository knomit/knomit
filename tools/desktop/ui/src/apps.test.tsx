import { describe, expect, it } from 'vitest'
import { render, screen } from '@testing-library/react'
import { LogsApp } from './LogsApp.tsx'
import { SettingsApp } from './SettingsApp.tsx'

// Smoke coverage: it exists so the two window entry points are proven to render
// at all, which is what tells us a blank desktop window is a routing problem
// rather than a React one. What the Logs window actually DOES lives in
// LogsApp.test.tsx; this only asserts it comes up. Task 6 replaces the settings
// placeholder.
describe('desktop window entry points', () => {
  it('renders the settings placeholder', () => {
    render(<SettingsApp />)
    expect(screen.getByRole('heading', { name: 'Settings' })).toBeInTheDocument()
  })

  it('renders the logs window', () => {
    render(<LogsApp />)
    expect(screen.getByRole('button', { name: /clear/i })).toBeInTheDocument()
  })
})
