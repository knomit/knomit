import { act, render, screen, fireEvent, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { SettingsForm, type Settings } from './SettingsForm.tsx'

const base: Settings = {
  port: '19278',
  logLevel: 'info',
  logFormat: 'console',
  startAtLogin: false,
  effectivePort: 19278,
  configPath: '/home/u/.knomit/knomit.toml',
  logFilePath: '/home/u/Library/Logs/knomit/knomit-desktop.log',
  overriddenByEnv: [],
}

// Every prop is required, so a helper keeps each test naming only what it cares
// about. Returns the spies so a test can assert on them.
function renderForm(initial: Partial<Settings> = {}, props: Partial<Handlers> = {}) {
  const handlers: Handlers = {
    onSave: vi.fn().mockResolvedValue(undefined),
    onRestart: vi.fn().mockReturnValue(new Promise<void>(() => {})),
    onRevealLog: vi.fn().mockResolvedValue(undefined),
    ...props,
  }
  render(<SettingsForm initial={{ ...base, ...initial }} {...handlers} />)
  return handlers
}

interface Handlers {
  onSave: (s: Settings) => Promise<void>
  onRestart: () => Promise<void>
  onRevealLog: () => Promise<void>
}

describe('SettingsForm', () => {
  // Strengthened from the brief, which only asserted `.port`. A form that sent
  // ONLY the edited field would have passed that: SaveSettings takes the whole
  // struct and applySettings writes every key from it, so a dropped logLevel
  // reaches validateSettings as "" and the save is rejected outright
  // (settings.go: "log level must not be empty"). Assert the entire payload.
  it('saves the edited values, and everything else unchanged', async () => {
    const { onSave } = renderForm()

    fireEvent.change(screen.getByLabelText(/port/i), { target: { value: '20000' } })
    fireEvent.click(screen.getByRole('button', { name: /^save$/i }))

    await waitFor(() => expect(onSave).toHaveBeenCalledTimes(1))
    expect(vi.mocked(onSave).mock.calls[0][0]).toEqual({ ...base, port: '20000' })
  })

  // envOverrides returns a NIL slice when no variable is set (settings.go), and
  // encoding/json renders a nil slice as `null`, not `[]`. That is the common
  // case — no env vars — so a form that calls .includes() on it straight from
  // the wire throws on mount and the whole Settings window comes up blank.
  it('survives overriddenByEnv arriving as null from Go', () => {
    renderForm({ overriddenByEnv: null as unknown as string[] })
    expect(screen.getByLabelText(/port/i)).toBeEnabled()
  })

  // KNOMIT_PORT beats knomit.toml, so writing the file would change nothing.
  // Letting the user edit the field anyway is the failure mode this prevents.
  //
  // Strengthened from the brief: it also pins that the OTHER fields stay
  // editable. Without that, a form that disabled everything whenever any
  // override existed — or that was permanently read-only — passed.
  it('disables a field the environment overrides and names the variable', () => {
    renderForm({ overriddenByEnv: ['KNOMIT_PORT'] })

    expect(screen.getByLabelText(/port/i)).toBeDisabled()
    expect(screen.getByText(/KNOMIT_PORT/)).toBeInTheDocument()

    expect(screen.getByLabelText(/log level/i)).toBeEnabled()
    expect(screen.getByLabelText(/log format/i)).toBeEnabled()
    expect(screen.queryByText(/KNOMIT_LOG_LEVEL/)).toBeNull()
  })

  // There is no env override for start-at-login: it is an OS login item, not a
  // config key, so envKeys (settings.go) has no entry for it. A form that
  // invented one would disable a control the user can always change.
  it('leaves start at login editable even when everything else is overridden', () => {
    renderForm({
      overriddenByEnv: ['KNOMIT_PORT', 'KNOMIT_LOG_LEVEL', 'KNOMIT_LOG_FORMAT'],
    })
    expect(screen.getByLabelText(/start at login/i)).toBeEnabled()
  })

  // A port change strands every connected MCP bridge; the user is told before
  // it happens, not after they notice their tools stopped working.
  it('offers a restart only after a port change', async () => {
    const { onSave } = renderForm()

    fireEvent.change(screen.getByLabelText(/log level/i), { target: { value: 'debug' } })
    fireEvent.click(screen.getByRole('button', { name: /^save$/i }))
    await waitFor(() => expect(onSave).toHaveBeenCalled())
    expect(screen.queryByRole('button', { name: /restart now/i })).toBeNull()

    fireEvent.change(screen.getByLabelText(/port/i), { target: { value: '20000' } })
    fireEvent.click(screen.getByRole('button', { name: /^save$/i }))
    await waitFor(() =>
      expect(screen.getByRole('button', { name: /restart now/i })).toBeInTheDocument(),
    )
    expect(screen.getByText(/MCP clients/i)).toBeInTheDocument()
  })

  // A rejected SaveSettings wrote nothing (applySettings validates before it
  // touches either backend), so the running port is still the old one. Offering
  // a restart here would restart the app into no change at all and blame the
  // user's port edit for it.
  it('does not offer a restart when the save fails', async () => {
    renderForm({}, { onSave: vi.fn().mockRejectedValue(new Error('port must be between 1024 and 65535')) })

    fireEvent.change(screen.getByLabelText(/port/i), { target: { value: '20000' } })
    fireEvent.click(screen.getByRole('button', { name: /^save$/i }))

    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(/port must be between/i),
    )
    expect(screen.queryByRole('button', { name: /restart now/i })).toBeNull()
  })

  it('shows the effective port when it differs from the configured one', () => {
    renderForm({ port: '19278', effectivePort: 54321 })
    expect(screen.getByText(/54321/)).toBeInTheDocument()
  })

  // The negative halves the brief omitted. Without them a form that always
  // printed effectivePort passed the assertion above, and would tell every
  // user their configured port "was unavailable".
  it('says nothing about the effective port when it matches', () => {
    renderForm({ port: '19278', effectivePort: 19278 })
    expect(screen.queryByText(/unavailable/i)).toBeNull()
  })

  it('says nothing about the effective port while the server is still booting', () => {
    // EffectivePort is 0 until the boot goroutine reports it (settings.go).
    renderForm({ port: '19278', effectivePort: 0 })
    expect(screen.queryByText(/unavailable/i)).toBeNull()
  })

  // Client-side validation mirrors validateSettings in settings.go. Go would
  // reject these too, but only after a round trip that shows a raw Go error;
  // more importantly a blocked save leaves knomit.toml untouched with no chance
  // of a partial apply (the autostart toggle is written before the file).
  it('refuses a port outside 1024-65535 without calling onSave', async () => {
    const { onSave } = renderForm()

    fireEvent.change(screen.getByLabelText(/port/i), { target: { value: '80' } })
    fireEvent.click(screen.getByRole('button', { name: /^save$/i }))

    expect(await screen.findByRole('alert')).toHaveTextContent(/1024/)
    expect(onSave).not.toHaveBeenCalled()
  })

  it('refuses a non-numeric port without calling onSave', async () => {
    const { onSave } = renderForm()

    fireEvent.change(screen.getByLabelText(/port/i), { target: { value: 'nineteen' } })
    fireEvent.click(screen.getByRole('button', { name: /^save$/i }))

    expect(await screen.findByRole('alert')).toHaveTextContent(/number/i)
    expect(onSave).not.toHaveBeenCalled()
  })

  // The log level is a <select> over the five zerolog levels precisely so the
  // empty value validateSettings rejects can never be produced here.
  it('never offers an empty log level', () => {
    renderForm()
    const options = Array.from(
      screen.getByLabelText(/log level/i).querySelectorAll('option'),
    ).map((o) => o.getAttribute('value'))
    expect(options).toEqual(['trace', 'debug', 'info', 'warn', 'error'])
  })

  // THE load-bearing one. RestartApp never returns: wapp.Quit() is
  // InvokeSync(destroy) -> NSApp terminate, so the process is gone before the
  // transport writes a response and the promise never settles. Confirmed
  // empirically in Task 9. The button must therefore commit to a terminal
  // "Restarting…" state on click rather than waiting for a resolution that is
  // never coming.
  it('enters a terminal restarting state and does not wait for a resolution', async () => {
    const onRestart = vi.fn().mockReturnValue(new Promise<void>(() => {}))
    renderForm({}, { onRestart })

    fireEvent.change(screen.getByLabelText(/port/i), { target: { value: '20000' } })
    fireEvent.click(screen.getByRole('button', { name: /^save$/i }))
    fireEvent.click(await screen.findByRole('button', { name: /restart now/i }))

    expect(onRestart).toHaveBeenCalledTimes(1)
    const button = screen.getByRole('button', { name: /restarting/i })
    expect(button).toBeDisabled()

    // Flush every microtask the promise could have scheduled AND the React
    // work those would have queued. The state must be unchanged afterwards.
    await act(async () => {})
    expect(screen.getByRole('button', { name: /restarting/i })).toBeDisabled()
    expect(screen.queryByRole('button', { name: /restart now/i })).toBeNull()
  })

  // Without this the "terminal state" test above cannot fail against the
  // mutation it exists to prevent: with a never-settling promise, `await
  // onRestart(); setRestarting(false)` and "never clear it" are observationally
  // identical. Forcing a resolution separates them. Resolution is not a signal
  // of anything here — RestartApp cannot report success, only failure — so
  // treating it as "restart finished, put the button back" is wrong even though
  // today's transport never delivers it.
  it('stays in the restarting state even if the promise does resolve', async () => {
    const onRestart = vi.fn().mockResolvedValue(undefined)
    renderForm({}, { onRestart })

    fireEvent.change(screen.getByLabelText(/port/i), { target: { value: '20000' } })
    fireEvent.click(screen.getByRole('button', { name: /^save$/i }))
    fireEvent.click(await screen.findByRole('button', { name: /restart now/i }))

    await act(async () => {})
    expect(screen.getByRole('button', { name: /restarting/i })).toBeDisabled()
    expect(screen.queryByRole('button', { name: /restart now/i })).toBeNull()
  })

  // A port change that reached the file still needs a restart, whatever the
  // user does next. A form that recomputed the offer on every save attempt
  // would silently retract it here, and the user would go on running against
  // the old port with no warning left on screen.
  it('keeps the restart offer when a later save fails', async () => {
    const onSave = vi
      .fn()
      .mockResolvedValueOnce(undefined)
      .mockRejectedValueOnce(new Error('write knomit.toml: permission denied'))
    renderForm({}, { onSave })

    fireEvent.change(screen.getByLabelText(/port/i), { target: { value: '20000' } })
    fireEvent.click(screen.getByRole('button', { name: /^save$/i }))
    await screen.findByRole('button', { name: /restart now/i })

    fireEvent.change(screen.getByLabelText(/log level/i), { target: { value: 'debug' } })
    fireEvent.click(screen.getByRole('button', { name: /^save$/i }))

    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(/permission denied/),
    )
    expect(screen.getByRole('button', { name: /restart now/i })).toBeInTheDocument()
  })

  // ...and a blocked-before-the-wire save must not retract it either.
  it('keeps the restart offer when a later save is refused by validation', async () => {
    const { onSave } = renderForm()

    fireEvent.change(screen.getByLabelText(/port/i), { target: { value: '20000' } })
    fireEvent.click(screen.getByRole('button', { name: /^save$/i }))
    await screen.findByRole('button', { name: /restart now/i })

    fireEvent.change(screen.getByLabelText(/port/i), { target: { value: '80' } })
    fireEvent.click(screen.getByRole('button', { name: /^save$/i }))

    expect(await screen.findByRole('alert')).toHaveTextContent(/1024/)
    expect(onSave).toHaveBeenCalledTimes(1)
    expect(screen.getByRole('button', { name: /restart now/i })).toBeInTheDocument()
  })

  // A rejection IS meaningful, and is the normal outcome in a dev build:
  // relaunchTarget -> bundlePathFor needs a real .app and fails 100% of the
  // time outside one. The app is still alive, so the button has to come back.
  it('surfaces a restart failure and re-enables the button', async () => {
    const onRestart = vi.fn().mockRejectedValue(new Error('resolve relaunch target: not a bundle'))
    renderForm({}, { onRestart })

    fireEvent.change(screen.getByLabelText(/port/i), { target: { value: '20000' } })
    fireEvent.click(screen.getByRole('button', { name: /^save$/i }))
    fireEvent.click(await screen.findByRole('button', { name: /restart now/i }))

    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(/not a bundle/),
    )
    expect(screen.getByRole('button', { name: /restart now/i })).toBeEnabled()
  })

  // The Logs window tails a bounded window of the file; the history lives on
  // disk, so the path has to be reachable, not just printed.
  it('reveals the log file', async () => {
    const { onRevealLog } = renderForm()
    fireEvent.click(screen.getByRole('button', { name: /reveal/i }))
    await waitFor(() => expect(onRevealLog).toHaveBeenCalledTimes(1))
  })

  it('shows where the config and the log file live', () => {
    renderForm()
    expect(screen.getByText(base.configPath)).toBeInTheDocument()
    expect(screen.getByText(base.logFilePath)).toBeInTheDocument()
  })
})
