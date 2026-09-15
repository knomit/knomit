import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { SettingsApp } from './SettingsApp.tsx'

// SettingsApp reaches Go on mount. Left unmocked, Call.ByName would try to post
// to /wails/runtime from jsdom; a pending promise is the honest stand-in for
// "the window came up and is waiting on the service".
vi.mock('@wailsio/runtime', () => ({
  Call: { ByName: () => new Promise(() => {}) },
}))

// Smoke coverage: it exists so the window entry point is proven to render at
// all, which is what tells us a blank desktop window is a routing problem
// rather than a React one. What the window actually DOES lives in
// SettingsApp.test.tsx / SettingsForm.test.tsx; this only asserts it comes up.
describe('desktop window entry points', () => {
  it('renders the settings window', () => {
    render(<SettingsApp />)
    expect(screen.getByRole('heading', { name: 'Settings' })).toBeInTheDocument()
  })
})
