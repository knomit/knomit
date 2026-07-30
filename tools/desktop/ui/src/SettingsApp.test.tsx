import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { SettingsApp } from './SettingsApp.tsx'
import type { Settings } from './SettingsForm.tsx'

// The bound-method strings are the whole contract with Go and nothing checks
// them at build time — there is no binding codegen. Mocking Call at this level
// is what lets the names, and the arguments sent with them, be asserted.
const byName = vi.fn()
vi.mock('@wailsio/runtime', () => ({
  Call: { ByName: (...args: unknown[]) => byName(...args) },
}))

const loaded: Settings = {
  port: '19278',
  logLevel: 'info',
  logFormat: 'console',
  startAtLogin: false,
  effectivePort: 19278,
  configPath: '/home/u/.knomit/knomit.toml',
  logFilePath: '/home/u/Library/Logs/knomit/knomit-desktop.log',
  overriddenByEnv: [],
}

// Answers GetSettings and resolves everything else, unless a test overrides it.
function respond(overrides: Record<string, unknown> = {}) {
  byName.mockImplementation((name: string) => {
    if (name in overrides) return overrides[name]
    if (name === 'main.NativeService.GetSettings') return Promise.resolve(loaded)
    return Promise.resolve(undefined)
  })
}

beforeEach(() => {
  byName.mockReset()
  respond()
})

describe('SettingsApp', () => {
  it('loads the settings over the bound GetSettings method', async () => {
    render(<SettingsApp />)

    expect(byName).toHaveBeenCalledWith('main.NativeService.GetSettings')
    // The port field carrying the loaded value is the proof the response was
    // used, not merely requested.
    await waitFor(() => expect(screen.getByLabelText(/port/i)).toHaveValue('19278'))
  })

  it('reports a load failure instead of rendering an empty form', async () => {
    respond({ 'main.NativeService.GetSettings': Promise.reject(new Error('load config: bad toml')) })
    render(<SettingsApp />)

    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(/load config: bad toml/),
    )
    expect(screen.queryByLabelText(/port/i)).toBeNull()
  })

  it('sends the whole settings struct to the bound SaveSettings method', async () => {
    render(<SettingsApp />)
    await screen.findByLabelText(/port/i)

    fireEvent.change(screen.getByLabelText(/port/i), { target: { value: '20000' } })
    fireEvent.click(screen.getByRole('button', { name: /^save$/i }))

    await waitFor(() =>
      expect(byName).toHaveBeenCalledWith('main.NativeService.SaveSettings', {
        ...loaded,
        port: '20000',
      }),
    )
  })

  it('calls the bound RestartApp method after a port change', async () => {
    // Never settles, exactly as the real one does not — see SettingsForm.
    respond({ 'main.NativeService.RestartApp': new Promise<void>(() => {}) })
    render(<SettingsApp />)
    await screen.findByLabelText(/port/i)

    fireEvent.change(screen.getByLabelText(/port/i), { target: { value: '20000' } })
    fireEvent.click(screen.getByRole('button', { name: /^save$/i }))
    fireEvent.click(await screen.findByRole('button', { name: /restart now/i }))

    expect(byName).toHaveBeenCalledWith('main.NativeService.RestartApp')
    expect(await screen.findByRole('button', { name: /restarting/i })).toBeDisabled()
  })

  it('calls the bound RevealLogFile method', async () => {
    render(<SettingsApp />)
    await screen.findByLabelText(/port/i)

    fireEvent.click(screen.getByRole('button', { name: /reveal/i }))
    await waitFor(() =>
      expect(byName).toHaveBeenCalledWith('main.NativeService.RevealLogFile'),
    )
  })
})
