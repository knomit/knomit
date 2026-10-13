import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { SettingsApp } from './SettingsApp.tsx'
import type { Settings } from './SettingsForm.tsx'
import { enrolled, notEnrolled } from './fleetFixtures.ts'
import { ack, baseSettings } from './settingsFixtures.ts'

// The bound-method strings are the whole contract with Go and nothing checks
// them at build time — there is no binding codegen. Mocking Call at this level
// is what lets the names, and the arguments sent with them, be asserted.
const byName = vi.fn()
const setText = vi.fn()
const getText = vi.fn()
vi.mock('@wailsio/runtime', () => ({
  Call: { ByName: (...args: unknown[]) => byName(...args) },
  Clipboard: {
    SetText: (...args: unknown[]) => setText(...args),
    Text: () => getText(),
  },
}))

const loaded: Settings = baseSettings

// Answers GetSettings and resolves everything else, unless a test overrides it.
function respond(overrides: Record<string, unknown> = {}) {
  byName.mockImplementation((name: string) => {
    if (name in overrides) return overrides[name]
    if (name === 'main.NativeService.GetSettings') return Promise.resolve(loaded)
    if (name === 'main.NativeService.GetIdentity') return Promise.resolve(notEnrolled)
    if (name === 'main.NativeService.SaveSettings') return Promise.resolve(ack())
    return Promise.resolve(undefined)
  })
}

// The sidebar: Reveal lives on Logs, the fleet identity on Fleet.
const goTo = async (page: RegExp) =>
  fireEvent.click(await screen.findByRole('button', { name: page }))

const BUNDLE = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"

beforeEach(() => {
  byName.mockReset()
  setText.mockReset().mockResolvedValue(undefined)
  getText.mockReset().mockResolvedValue(BUNDLE)
  respond()
})

describe('SettingsApp', () => {
  it('loads the settings over the bound GetSettings method', async () => {
    render(<SettingsApp />)

    expect(byName).toHaveBeenCalledWith('main.NativeService.GetSettings')
    // The port field carrying the loaded value is the proof the response was
    // used, not merely requested.
    await waitFor(() => expect(screen.getByLabelText(/^port$/i)).toHaveValue('19278'))
  })

  it('reports a load failure instead of rendering an empty form', async () => {
    respond({ 'main.NativeService.GetSettings': Promise.reject(new Error('load config: bad toml')) })
    render(<SettingsApp />)

    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(/load config: bad toml/),
    )
    expect(screen.queryByLabelText(/^port$/i)).toBeNull()
  })

  it('sends the whole settings struct to the bound SaveSettings method', async () => {
    render(<SettingsApp />)
    await screen.findByLabelText(/^port$/i)

    fireEvent.change(screen.getByLabelText(/^port$/i), { target: { value: '20000' } })
    fireEvent.click(screen.getByRole('button', { name: /^save$/i }))

    await waitFor(() =>
      expect(byName).toHaveBeenCalledWith('main.NativeService.SaveSettings', {
        ...loaded,
        port: '20000',
      }),
    )
  })

  it('calls the bound RestartServer method, and leaves "Restarting…" when it resolves', async () => {
    const needing = { ...loaded, server: { ...loaded.server, restartNeeded: ['auth.require'] } }
    const restarted = { ...loaded, server: { ...loaded.server, pid: 5555 } }
    let didRestart = false
    byName.mockImplementation((name: string) => {
      if (name === 'main.NativeService.GetSettings') return Promise.resolve(didRestart ? restarted : needing)
      if (name === 'main.NativeService.GetIdentity') return Promise.resolve(notEnrolled)
      if (name === 'main.NativeService.RestartServer') {
        didRestart = true
        return Promise.resolve(undefined)
      }
      return Promise.resolve(undefined)
    })
    render(<SettingsApp />)

    fireEvent.click(await screen.findByRole('button', { name: /restart server/i }))

    expect(byName).toHaveBeenCalledWith('main.NativeService.RestartServer')
    expect(await screen.findByRole('status')).toHaveTextContent(/server restarted/i)
    expect(screen.queryByRole('button', { name: /restarting/i })).toBeNull()
  })

  it('reads the settings again after a save, for the server state', async () => {
    render(<SettingsApp />)
    await screen.findByLabelText(/^port$/i)
    byName.mockClear()
    fireEvent.click(screen.getByRole('button', { name: /^save$/i }))
    await waitFor(() => expect(byName).toHaveBeenCalledWith('main.NativeService.GetSettings'))
  })

  it('calls the bound RevealLogFile method', async () => {
    render(<SettingsApp />)
    await screen.findByLabelText(/^port$/i)

    await goTo(/^logs$/i)
    fireEvent.click(screen.getByRole('button', { name: /reveal/i }))
    await waitFor(() =>
      expect(byName).toHaveBeenCalledWith('main.NativeService.RevealLogFile'),
    )
  })
})

describe('SettingsApp fleet identity', () => {
  it('loads the section over the bound GetIdentity method', async () => {
    render(<SettingsApp />)
    expect(byName).toHaveBeenCalledWith('main.NativeService.GetIdentity')
    expect(await screen.findByText(/not enrolled/i)).toBeInTheDocument()
  })

  it('copies the line PublicKeyLine returns through the Wails clipboard', async () => {
    respond({ 'main.NativeService.PublicKeyLine': Promise.resolve('ssh-ed25519 AAAA knomit@laptop') })
    render(<SettingsApp />)
    await goTo(/^fleet$/i)
    fireEvent.click(await screen.findByRole('button', { name: /copy public key/i }))
    await waitFor(() => expect(setText).toHaveBeenCalledWith('ssh-ed25519 AAAA knomit@laptop'))
    expect(byName).toHaveBeenCalledWith('main.NativeService.PublicKeyLine')
  })

  it('sends the clipboard bundle to InstallBundle with no confirmation, then the shown pair on replace', async () => {
    const A = 'a'.repeat(64)
    const B = 'b'.repeat(64)
    const install = vi
      .fn()
      .mockResolvedValueOnce({ installed: false, class: 'root_differs', message: '', installedRootFingerprint: A, bundleRootFingerprint: B, bundlePrincipal: '', identity: null })
      .mockResolvedValueOnce({ installed: true, class: '', message: '', installedRootFingerprint: '', bundleRootFingerprint: '', bundlePrincipal: '', identity: enrolled })
    byName.mockImplementation((name: string, ...args: unknown[]) => {
      if (name === 'main.NativeService.GetSettings') return Promise.resolve(loaded)
      if (name === 'main.NativeService.GetIdentity') return Promise.resolve(notEnrolled)
      if (name === 'main.NativeService.InstallBundle') return install(...args)
      return Promise.resolve(undefined)
    })
    render(<SettingsApp />)
    await goTo(/^fleet$/i)
    fireEvent.click(await screen.findByRole('button', { name: /paste from clipboard/i }))
    await screen.findByText(/bundle loaded/i)
    expect(getText).toHaveBeenCalledTimes(1)
    fireEvent.click(screen.getByRole('button', { name: /^install$/i }))
    fireEvent.click(await screen.findByRole('button', { name: /replace fleet root/i }))
    await screen.findByText(/^installed\./i)
    expect(byName).toHaveBeenCalledWith('main.NativeService.InstallBundle', BUNDLE, '', '')
    expect(byName).toHaveBeenLastCalledWith('main.NativeService.InstallBundle', BUNDLE, A, B)
    // The rows now show what was installed.
    expect(screen.getByText(enrolled.san)).toBeInTheDocument()
  })

  it('keeps the settings usable when GetIdentity fails', async () => {
    respond({ 'main.NativeService.GetIdentity': Promise.reject(new Error('load config: bad toml')) })
    render(<SettingsApp />)
    expect(await screen.findByLabelText(/^port$/i)).toHaveValue('19278')
    expect(screen.getByText(/loading…/i)).toBeInTheDocument()
  })
})
