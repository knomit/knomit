import { render, screen, fireEvent, waitFor, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { ALL_NETWORKS_WARNING, FLEET_PORT_WAITING, SettingsForm, type SaveAck, type Settings } from './SettingsForm.tsx'
import { notEnrolled } from './fleetFixtures.ts'
import { ack, attachedServer, baseSettings as base, spawnedServer } from './settingsFixtures.ts'

// Every prop is required, so a helper keeps each test naming only what it cares
// about. Returns the spies so a test can assert on them.
function renderForm(initial: Partial<Settings> = {}, props: Partial<Handlers> = {}) {
  const handlers: Handlers = {
    onSave: vi.fn().mockResolvedValue(ack()),
    onRestart: vi.fn().mockResolvedValue(undefined),
    onRevealLog: vi.fn().mockResolvedValue(undefined),
    onCancel: vi.fn(),
    ...props,
  }
  render(<SettingsForm initial={{ ...base, ...initial }} pollMs={1} {...handlers} />)
  return handlers
}

interface Handlers {
  onSave: (s: Settings) => Promise<SaveAck>
  onRestart: () => Promise<void>
  onRevealLog: () => Promise<void>
  onCancel: () => void
  onRefresh?: () => Promise<Settings>
  restartTimeoutMs?: number
}

const save = () => fireEvent.click(screen.getByRole('button', { name: /^save$/i }))

// The sidebar. Its items are buttons named by the page (plus "has an error"
// when the page holds one), so a name match is what a person would click.
const nav = () => screen.getByRole('navigation', { name: /settings sections/i })
const navItem = (page: RegExp) => within(nav()).getByRole('button', { name: page })
const goTo = (page: RegExp) => fireEvent.click(navItem(page))
const fleetProps = {
  identity: notEnrolled,
  onCopyPublicKey: vi.fn().mockResolvedValue(undefined),
}

describe('SettingsForm sidebar', () => {
  it('lists General, Network, Fleet and Logs, in that order, and opens on General', () => {
    render(
      <SettingsForm
        initial={base}
        onSave={vi.fn()}
        onRestart={vi.fn()}
        onRevealLog={vi.fn()}
        onCancel={vi.fn()}
        fleet={fleetProps}
      />,
    )
    expect(within(nav()).getAllByRole('button').map((b) => b.textContent)).toEqual([
      'General',
      'Network',
      'Fleet',
      'Logs',
    ])
    expect(navItem(/general/i)).toHaveAttribute('aria-current', 'page')
    expect(navItem(/network/i)).not.toHaveAttribute('aria-current')
    expect(screen.getByRole('heading', { level: 2, name: 'General' })).toBeVisible()
  })

  // A missing section is omitted, never shown disabled.
  it('has no Fleet item when the window has no fleet section', () => {
    renderForm()
    expect(within(nav()).getAllByRole('button').map((b) => b.textContent)).toEqual(['General', 'Network', 'Logs'])
  })

  it('shows one page at a time and moves the selection with it', () => {
    renderForm()
    expect(screen.getByLabelText(/start at login/i)).toBeVisible()
    expect(screen.getByLabelText(/^port$/i)).not.toBeVisible()

    goTo(/network/i)
    expect(navItem(/network/i)).toHaveAttribute('aria-current', 'page')
    expect(navItem(/general/i)).not.toHaveAttribute('aria-current')
    expect(screen.getByLabelText(/^port$/i)).toBeVisible()
    expect(screen.getByLabelText(/start at login/i)).not.toBeVisible()
    expect(screen.getByLabelText(/log level/i)).not.toBeVisible()

    goTo(/logs/i)
    expect(screen.getByLabelText(/log level/i)).toBeVisible()
    expect(screen.getByLabelText(/^port$/i)).not.toBeVisible()
  })

  // One transaction across every page: Save sends what was edited on pages
  // that are no longer in view.
  it('keeps edits made on other pages and saves them together', async () => {
    const { onSave } = renderForm()
    goTo(/network/i)
    fireEvent.change(screen.getByLabelText(/^port$/i), { target: { value: '20000' } })
    goTo(/logs/i)
    fireEvent.change(screen.getByLabelText(/log level/i), { target: { value: 'debug' } })
    goTo(/general/i)
    fireEvent.click(screen.getByLabelText(/start at login/i))
    save()
    await waitFor(() => expect(onSave).toHaveBeenCalledTimes(1))
    expect(vi.mocked(onSave).mock.calls[0][0]).toEqual({
      ...base,
      port: '20000',
      logLevel: 'debug',
      startAtLogin: true,
    })
  })

  // The status sits at the foot of the sidebar, on every page.
  it('shows the server state at the foot of the sidebar on every page', () => {
    renderForm({ server: attachedServer })
    for (const p of [/general/i, /network/i, /logs/i]) {
      goTo(p)
      expect(screen.getByText('Running')).toBeVisible()
      expect(screen.getByText('127.0.0.1:19278')).toBeVisible()
      expect(screen.getByText(/· attached/)).toBeVisible()
    }
  })
})

describe('SettingsForm: an error on a page that is not in view', () => {
  it('switches to the page that holds the refused field and marks it in the sidebar', async () => {
    const { onSave } = renderForm()
    goTo(/network/i)
    fireEvent.change(screen.getByLabelText(/^port$/i), { target: { value: '80' } })
    goTo(/logs/i)
    save()

    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('Port must be between 1024 and 65535, got 80.')
    expect(alert).toBeVisible()
    expect(navItem(/network/i)).toHaveAttribute('aria-current', 'page')
    expect(navItem(/network/i)).toHaveAccessibleName('Network has an error')
    expect(navItem(/logs/i)).toHaveAccessibleName('Logs')
    expect(navItem(/general/i)).toHaveAccessibleName('General')
    expect(onSave).not.toHaveBeenCalled()
  })

  it('keeps the mark while you look elsewhere, and drops it once the value is fixed', async () => {
    renderForm()
    goTo(/network/i)
    fireEvent.change(screen.getByLabelText(/fleet listener/i), { target: { value: '0.0.0.0' } })
    save()
    await screen.findByRole('alert')

    goTo(/general/i)
    expect(navItem(/network/i)).toHaveAccessibleName('Network has an error')
    expect(screen.queryByRole('alert')).toBeNull()

    goTo(/network/i)
    fireEvent.change(screen.getByLabelText(/fleet listener/i), { target: { value: '0.0.0.0:19279' } })
    expect(navItem(/network/i)).toHaveAccessibleName('Network')
  })

  // Two pages hold errors and the one in view is the LATER of them: a Save
  // that always jumped to the first error page would land on Network.
  it('stays on the page in view when that page holds an error, and still marks the other', async () => {
    renderForm({ logLevel: 'verbose' })
    goTo(/network/i)
    fireEvent.change(screen.getByLabelText(/^port$/i), { target: { value: '80' } })
    goTo(/logs/i)
    save()
    expect(await screen.findByRole('alert')).toHaveTextContent(/Log level must be one of .*got "verbose"/)
    expect(navItem(/logs/i)).toHaveAttribute('aria-current', 'page')
    expect(navItem(/logs/i)).toHaveAccessibleName('Logs has an error')
    expect(navItem(/network/i)).not.toHaveAttribute('aria-current')
    expect(navItem(/network/i)).toHaveAccessibleName('Network has an error')
  })
})

describe('SettingsForm', () => {
  // Strengthened from the brief, which only asserted `.port`. A form that sent
  // ONLY the edited field would have passed that: SaveSettings takes the whole
  // struct and applySettings writes every key from it, so a dropped logLevel
  // reaches validateSettings as "" and the save is rejected outright
  // (settings.go: "log level must not be empty"). Assert the entire payload.
  it('saves the edited values, and everything else unchanged', async () => {
    const { onSave } = renderForm()

    fireEvent.change(screen.getByLabelText(/^port$/i), { target: { value: '20000' } })
    save()

    await waitFor(() => expect(onSave).toHaveBeenCalledTimes(1))
    expect(vi.mocked(onSave).mock.calls[0][0]).toEqual({ ...base, port: '20000' })
  })

  // envOverrides returns a NIL slice when no variable is set (settings.go), and
  // encoding/json renders a nil slice as `null`, not `[]`. That is the common
  // case — no env vars — so a form that calls .includes() on it straight from
  // the wire throws on mount and the whole Settings window comes up blank.
  it('survives overriddenByEnv arriving as null from Go', () => {
    renderForm({ overriddenByEnv: null as unknown as string[] })
    expect(screen.getByLabelText(/^port$/i)).toBeEnabled()
  })

  // KNOMIT_PORT beats knomit.toml, so writing the file would change nothing.
  // Letting the user edit the field anyway is the failure mode this prevents.
  //
  // READ-ONLY, not disabled. The value is perfectly valid and worth being able
  // to read and copy — it is simply owned by the environment. Disabling greys
  // it out and reads as "unavailable", which is a different and wrong claim.
  // A <select> has no readOnly, so those still take `disabled`; the chip is
  // what carries the meaning on all three.
  it('makes a field the environment overrides read-only and names the variable', () => {
    renderForm({ overriddenByEnv: ['KNOMIT_PORT'] })

    const port = screen.getByLabelText(/^port$/i)
    expect(port).toHaveAttribute('readonly')
    expect(port).toBeEnabled()
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
      overriddenByEnv: ['KNOMIT_PORT', 'KNOMIT_LOG_LEVEL', 'KNOMIT_LOG_FORMAT', 'KNOMIT_HTTP'],
    })
    expect(screen.getByLabelText(/start at login/i)).toBeEnabled()
  })

  it('confirms a plain save', async () => {
    renderForm()
    fireEvent.change(screen.getByLabelText(/log level/i), { target: { value: 'debug' } })
    save()

    expect(await screen.findByRole('status')).toHaveTextContent(/saved/i)
    expect(screen.queryByRole('button', { name: /restart server/i })).toBeNull()
  })

  // A rejected SaveSettings wrote nothing (applySettings validates before it
  // touches either backend), so the server was not told either.
  it('shows a rejected save as the error it is, with no restart offered', async () => {
    renderForm({}, { onSave: vi.fn().mockRejectedValue(new Error('port must be between 1024 and 65535')) })

    fireEvent.change(screen.getByLabelText(/^port$/i), { target: { value: '20000' } })
    save()

    await waitFor(() =>
      expect(screen.getByRole('alert')).toHaveTextContent(/port must be between/i),
    )
    expect(screen.queryByRole('button', { name: /restart server/i })).toBeNull()
  })

  it('shows the effective port when it differs from the configured one', () => {
    renderForm({ port: '19278', effectivePort: 54321, server: { ...spawnedServer, httpUrl: '' } })
    expect(screen.getByText(/54321/)).toBeInTheDocument()
    expect(screen.getByText(/configured port was busy/i)).toBeInTheDocument()
  })

  // The negative halves. Without them a form that always printed effectivePort
  // passed the assertion above, and would tell every user their configured port
  // "was busy".
  it('says nothing about the effective port when it matches', () => {
    renderForm({ port: '19278', effectivePort: 19278 })
    expect(screen.queryByText(/busy/i)).toBeNull()
  })

  it('says nothing about the effective port while the server is still booting', () => {
    // No server.json yet (settings.go).
    renderForm({ port: '19278', effectivePort: 0, server: { ...spawnedServer, running: false, httpUrl: '' } })
    expect(screen.queryByText(/busy/i)).toBeNull()
    expect(screen.getByText(/starting/i)).toBeInTheDocument()
  })

  it('shows a running server with no HTTP port as socket-only, not as booting', () => {
    renderForm({ httpScope: 'off', effectivePort: 0, server: { ...spawnedServer, httpUrl: '' } })
    expect(screen.getByText('Running')).toBeInTheDocument()
    expect(screen.getByText(/local socket only/i)).toBeInTheDocument()
  })

  // Client-side validation mirrors validateSettings in settings.go. Go would
  // reject these too, but only after a round trip that shows a raw Go error;
  // more importantly a blocked save leaves knomit.toml untouched with no chance
  // of a partial apply (the autostart toggle is written before the file).
  it('refuses a port outside 1024-65535 without calling onSave', async () => {
    const { onSave } = renderForm()

    fireEvent.change(screen.getByLabelText(/^port$/i), { target: { value: '80' } })
    save()

    expect(await screen.findByRole('alert')).toHaveTextContent(/1024/)
    expect(onSave).not.toHaveBeenCalled()
  })

  it('refuses a non-numeric port without calling onSave', async () => {
    const { onSave } = renderForm()

    fireEvent.change(screen.getByLabelText(/^port$/i), { target: { value: 'nineteen' } })
    save()

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

  // Manage → Logs shows a bounded ring of recent lines; the history, and every
  // rotated file, lives on disk — so the path has to be reachable, not just
  // printed.
  it('reveals the log file', async () => {
    const { onRevealLog } = renderForm()
    goTo(/logs/i)
    fireEvent.click(screen.getByRole('button', { name: /reveal/i }))
    await waitFor(() => expect(onRevealLog).toHaveBeenCalledTimes(1))
  })

  // Home-collapsed so a long path cannot widen a fixed-size window. The full
  // value stays on `title`, which is what this asserts — the displayed text is
  // a rendering choice, the title is the promise that nothing was lost.
  it('shows where the config and the log file live, home-collapsed', () => {
    renderForm()

    expect(screen.getByTitle(base.configPath)).toBeInTheDocument()
    expect(screen.getByTitle(base.logFilePath)).toBeInTheDocument()

    expect(screen.getByText('~/.knomit/knomit.toml')).toBeInTheDocument()
    expect(screen.queryByText(base.configPath)).toBeNull()
  })

  // The Windows data root is %LOCALAPPDATA%\knomit\home, which is NOT inside
  // the user's home directory, so it collapses to the variable name rather
  // than to `~`.
  it('collapses a Windows config path to %LOCALAPPDATA%', () => {
    const configPath = 'C:\\Users\\u\\AppData\\Local\\knomit\\home\\knomit.toml'
    renderForm({ configPath })

    expect(screen.getByTitle(configPath)).toBeInTheDocument()
    expect(screen.getByText('%LOCALAPPDATA%\\knomit\\home\\knomit.toml')).toBeInTheDocument()
    expect(screen.queryByText(configPath)).toBeNull()
  })

  // resolveLogFile (logging.go) returns "" when there is no `[log] file` and no
  // resolvable logs directory. There is no path, so there is no button.
  it('offers no Reveal when there is no log file at all', () => {
    const { onRevealLog } = renderForm({ logFilePath: '' })

    expect(screen.queryByRole('button', { name: /reveal/i })).toBeNull()
    expect(onRevealLog).not.toHaveBeenCalled()
    expect(screen.getByText(/set/i)).toHaveTextContent('[log] file')
  })
})

describe('SettingsForm HTTP port scope', () => {
  const radio = (name: RegExp) => screen.getByRole('radio', { name })
  // The scope lives on the Network page.
  const renderNet = (...args: Parameters<typeof renderForm>) => {
    const h = renderForm(...args)
    goTo(/network/i)
    return h
  }

  it('offers the three scopes and checks the one the config means', () => {
    renderNet({ httpScope: 'off' })
    const group = screen.getByRole('radiogroup')
    expect(within(group).getAllByRole('radio').map((r) => r.closest('label')?.textContent)).toEqual([
      'Off',
      'This computer only',
      'All networks',
    ])
    expect(radio(/^off$/i)).toBeChecked()
    expect(radio(/this computer only/i)).not.toBeChecked()
    expect(radio(/all networks/i)).not.toBeChecked()
  })

  // The selected segment is styled by a class from state, not by :has(),
  // which WebKit before Safari 15.4 (macOS 11's) does not support.
  it('marks the selected segment with is-on, and moves it with the choice', () => {
    renderNet({ httpScope: 'off' })
    const opt = (name: RegExp) => radio(name).closest('label')
    expect(opt(/^off$/i)).toHaveClass('is-on')
    expect(opt(/this computer only/i)).not.toHaveClass('is-on')
    expect(opt(/all networks/i)).not.toHaveClass('is-on')
    fireEvent.click(radio(/all networks/i))
    expect(opt(/all networks/i)).toHaveClass('is-on')
    expect(opt(/^off$/i)).not.toHaveClass('is-on')
  })

  it('marks no segment when the config binds some other host', () => {
    renderNet({ httpScope: 'other' })
    for (const r of screen.getAllByRole('radio')) expect(r.closest('label')).not.toHaveClass('is-on')
  })

  it.each([
    ['off', /No TCP port is open/i],
    ['local', /Only programs on this computer can connect/i],
    ['all', /Anyone who can reach this computer can READ/],
  ] as const)('%s shows its own copy', (scope, text) => {
    renderNet({ httpScope: scope })
    expect(screen.getByText(text)).toBeInTheDocument()
    // ...and only its own: the other scopes' copy is not on screen.
    const others = (['off', 'local', 'all'] as const).filter((x) => x !== scope)
    const copy = { off: /No TCP port is open/i, local: /Only programs on this computer/i, all: /Anyone who can reach/ }
    for (const o of others) expect(screen.queryByText(copy[o])).toBeNull()
  })

  // The consequence of "All networks", verbatim from the design (Q5). A reworded
  // warning is a different promise.
  it('states the All networks consequence verbatim', () => {
    renderNet({ httpScope: 'all' })
    expect(ALL_NETWORKS_WARNING).toBe(
      'Anyone who can reach this computer can READ every knowledge base and clone it, without signing in. ' +
        'They cannot change anything. To let other machines work with this one securely, enrol it in a fleet ' +
        'and use the Fleet port.',
    )
    expect(screen.getByText(ALL_NETWORKS_WARNING)).toBeInTheDocument()
  })

  it('shows the warning as soon as All networks is chosen, before saving', () => {
    renderNet({ httpScope: 'local' })
    expect(screen.queryByText(ALL_NETWORKS_WARNING)).toBeNull()
    fireEvent.click(radio(/all networks/i))
    expect(screen.getByText(ALL_NETWORKS_WARNING)).toBeInTheDocument()
  })

  it('sends the chosen scope with the rest of the settings', async () => {
    const { onSave } = renderNet({ httpScope: 'off' })
    fireEvent.click(radio(/this computer only/i))
    save()
    await waitFor(() => expect(onSave).toHaveBeenCalledTimes(1))
    expect(vi.mocked(onSave).mock.calls[0][0]).toEqual({ ...base, httpScope: 'local' })
  })

  it('leaves a host Settings does not edit alone: nothing checked, and it says so', () => {
    renderNet({ httpScope: 'other' })
    for (const r of screen.getAllByRole('radio')) expect(r).not.toBeChecked()
    expect(screen.getByText(/binds a specific address/i)).toBeInTheDocument()
  })

  it.each(['KNOMIT_HTTP', 'KNOMIT_HOST'])('is read-only and names the variable when %s owns it', (v) => {
    renderNet({ overriddenByEnv: [v] })
    for (const r of screen.getAllByRole('radio')) expect(r).toBeDisabled()
    expect(screen.getByText(new RegExp(v))).toBeVisible()
  })

  // The same amber "env" chip the Port and Log level rows carry, on the scope
  // row itself, and the choice cannot move: a save sends the scope it loaded.
  it('carries the env chip on the scope row and keeps the loaded scope through a save', async () => {
    const { onSave } = renderNet({ overriddenByEnv: ['KNOMIT_HTTP'], httpScope: 'local' })
    const group = screen.getByRole('radiogroup', { name: /http port/i })
    const row = group.closest('.row') as HTMLElement
    expect(within(row).getByText('env')).toHaveClass('chip')
    expect(within(row).getByText(/KNOMIT_HTTP/).closest('p')).toHaveClass('env')
    // The Port row has no chip of its own: only KNOMIT_HTTP is set.
    const portRow = screen.getByLabelText(/^port$/i).closest('.row') as HTMLElement
    expect(within(portRow).queryByText('env')).toBeNull()

    // Disabled, so no click can move it (jsdom toggles a disabled radio's
    // `checked` on a synthetic click, which a webview does not, so the
    // assertion is on the disabled state and on what Save sends).
    for (const r of within(group).getAllByRole('radio')) expect(r).toBeDisabled()
    expect(radio(/this computer only/i)).toBeChecked()
    save()
    await waitFor(() => expect(onSave).toHaveBeenCalledTimes(1))
    expect(vi.mocked(onSave).mock.calls[0][0].httpScope).toBe('local')
  })
})

describe('SettingsForm after a save: what became of the running server', () => {
  const pending = async (a: SaveAck) => {
    renderForm({}, { onSave: vi.fn().mockResolvedValue(a) })
    fireEvent.change(screen.getByLabelText(/^port$/i), { target: { value: '20000' } })
    save()
  }

  it('applied: says so and names where it listens', async () => {
    await pending(ack({ httpUrl: 'http://127.0.0.1:20000' }))
    expect(await screen.findByRole('status')).toHaveTextContent(/saved and applied.*127\.0\.0\.1:20000/i)
    expect(screen.queryByRole('button', { name: /restart server/i })).toBeNull()
  })

  it('applied with the port off: says it is off', async () => {
    renderForm({ httpScope: 'off' }, { onSave: vi.fn().mockResolvedValue(ack({ httpUrl: '' })) })
    save()
    expect(await screen.findByRole('status')).toHaveTextContent(/HTTP port is off/i)
  })

  it('restart_needed: lists the keys and offers the restart, nothing else', async () => {
    await pending(ack({ outcome: 'restart_needed', restartNeeded: ['auth.require', 'oauth.issuer'] }))
    const bar = await screen.findByText(/auth\.require, oauth\.issuer/)
    expect(bar).toHaveTextContent(/take effect after a restart of the server/i)
    expect(screen.getByRole('button', { name: /restart server/i })).toBeEnabled()
    // Not the old claim that a port change itself needs a restart.
    expect(screen.queryByText(/MCP clients/i)).toBeNull()
  })

  it.each([
    ['listener_failed', 'reloaded, but not every listener is as configured (http: addr_in_use)', /port could not be opened.*addr_in_use/i],
    ['refused', 'toml: line 1: bad', /could not load it and kept its running settings.*bad/i],
    ['timeout', 'reload requested, but the server did not acknowledge within 5s', /did not confirm.*5s/i],
    ['unavailable', 'does not listen for reload: restart it', /could not be told.*restart it/i],
  ] as const)('%s: a save that reached the file but not the server is an error line, not "Saved."', async (outcome, error, text) => {
    await pending(ack({ outcome, error }))
    expect(await screen.findByRole('alert')).toHaveTextContent(text)
    expect(screen.queryByRole('status')).toBeNull()
  })

  // A fresh instance sets the Fleet port BEFORE it is in a fleet: the
  // listener waits for the certificate the create or join installs, which
  // starts it. That is not an error (2026-10-17 chicken-and-egg report).
  // SABOTAGE: drop the no_certificate branch from ackLine -> no note -> red.
  it('applied with the Fleet port waiting for a certificate: a status note, not an error', async () => {
    renderForm({ tlsAddr: '0.0.0.0:19279' }, { onSave: vi.fn().mockResolvedValue(ack({ tlsReason: 'no_certificate' })) })
    save()
    const line = await screen.findByRole('status')
    expect(line).toHaveTextContent(/saved and applied/i)
    expect(line).toHaveTextContent(FLEET_PORT_WAITING('0.0.0.0:19279'))
    // The user's approved wording (PR #545), verbatim.
    expect(line).toHaveTextContent('Fleet port 0.0.0.0:19279 is set; it opens when this instance creates or joins a fleet.')
    expect(screen.queryByRole('alert')).toBeNull()
  })

  // An attached server from before the reload client counted no_certificate
  // as waiting still answers listener_failed for it alone: the same note.
  // SABOTAGE: drop the listener_failed no_certificate case -> alert -> red.
  it('listener_failed for the waiting Fleet port alone (an older server): the same note, not an error', async () => {
    renderForm({ tlsAddr: '0.0.0.0:19279' }, {
      onSave: vi.fn().mockResolvedValue(ack({
        outcome: 'listener_failed', tlsReason: 'no_certificate',
        error: 'reloaded, but not every listener is as configured (tls: no_certificate); the previous listener keeps serving where there was one',
      })),
    })
    save()
    expect(await screen.findByRole('status')).toHaveTextContent(FLEET_PORT_WAITING('0.0.0.0:19279'))
    expect(screen.queryByRole('alert')).toBeNull()
  })

  // Positive control: a held HTTP port beside the waiting Fleet port is
  // still the error it is.
  it('listener_failed with a held HTTP port beside the waiting Fleet port: still an error', async () => {
    renderForm({ tlsAddr: '0.0.0.0:19279' }, {
      onSave: vi.fn().mockResolvedValue(ack({
        outcome: 'listener_failed', tlsReason: 'no_certificate', httpReason: 'addr_in_use',
        error: 'reloaded, but not every listener is as configured (http: addr_in_use, tls: no_certificate)',
      })),
    })
    save()
    expect(await screen.findByRole('alert')).toHaveTextContent(/port could not be opened.*addr_in_use/i)
  })

  it('not_running: confirms, and says it applies when the server starts', async () => {
    await pending(ack({ outcome: 'not_running' }))
    expect(await screen.findByRole('status')).toHaveTextContent(/apply when it starts/i)
  })

  it('refreshes the server state after a save', async () => {
    const onRefresh = vi.fn().mockResolvedValue({
      ...base,
      effectivePort: 20000,
      server: { ...spawnedServer, httpUrl: 'http://127.0.0.1:20000', generation: 2 },
    })
    renderForm({}, { onSave: vi.fn().mockResolvedValue(ack()), onRefresh })
    fireEvent.change(screen.getByLabelText(/^port$/i), { target: { value: '20000' } })
    save()
    await waitFor(() => expect(onRefresh).toHaveBeenCalled())
    expect(await screen.findByText('127.0.0.1:20000')).toBeInTheDocument()
  })

  it('shows a restart-needed notice that the window opened with', () => {
    renderForm({ server: { ...spawnedServer, restartNeeded: ['auth.require'] } })
    expect(screen.getByText(/auth\.require/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /restart server/i })).toBeEnabled()
  })
})

describe('SettingsForm attach mode', () => {
  const NOTE = /started outside the app \(pid 777\); restart is not available, changes apply on reload/i

  it('says the server was started outside the app, with its pid', () => {
    renderForm({ server: attachedServer })
    expect(screen.getByText(NOTE)).toBeInTheDocument()
  })

  it('says nothing about it for a server the app started', () => {
    renderForm()
    expect(screen.queryByText(/started outside the app/i)).toBeNull()
  })

  it('shows both versions, and warns only when major.minor differ', () => {
    renderForm({ server: { ...attachedServer, version: '0.4.9.old', desktopVersion: '0.5.3.new', versionSkew: true } })
    expect(screen.getByText('0.4.9.old')).toBeInTheDocument()
    expect(screen.getByText('0.5.3.new')).toBeInTheDocument()
    expect(screen.getByText(/different versions \(0\.4 and 0\.5\)/i)).toBeInTheDocument()
  })

  it('shows both versions without a warning when only the patch differs', () => {
    renderForm({ server: { ...attachedServer, version: '0.5.1.a', desktopVersion: '0.5.3.b', versionSkew: false } })
    expect(screen.getByText('0.5.1.a')).toBeInTheDocument()
    expect(screen.queryByText(/different versions/i)).toBeNull()
  })

  it('offers the restart notice but disables the button, naming the pid', () => {
    renderForm({ server: { ...attachedServer, restartNeeded: ['auth.require'] } })
    expect(screen.getByRole('button', { name: /restart server/i })).toBeDisabled()
    expect(screen.getAllByText(/restart it where it was started/i).length).toBeGreaterThan(0)
  })

  it('still saves and reloads: only the restart is unavailable', async () => {
    const { onSave } = renderForm({ server: attachedServer })
    fireEvent.click(screen.getByLabelText(/all networks/i))
    save()
    await waitFor(() => expect(onSave).toHaveBeenCalledTimes(1))
  })
})

describe('SettingsForm restart', () => {
  const needing = { server: { ...spawnedServer, restartNeeded: ['auth.require'] } }
  const restartBtn = () => screen.getByRole('button', { name: /restart server|restarting/i })

  it('leaves "Restarting…" on success and reports it', async () => {
    const { onRestart } = renderForm(needing)
    fireEvent.click(restartBtn())
    expect(onRestart).toHaveBeenCalledTimes(1)
    expect(screen.getByRole('button', { name: /restarting/i })).toBeDisabled()

    expect(await screen.findByRole('status')).toHaveTextContent(/server restarted/i)
    expect(screen.queryByRole('button', { name: /restarting/i })).toBeNull()
  })

  it('stops offering the restart once it succeeded: the new server has no keys waiting', async () => {
    const onRefresh = vi.fn().mockResolvedValue({
      ...base,
      server: { ...spawnedServer, pid: 5555, restartNeeded: [] },
    })
    renderForm(needing, { onRefresh })
    fireEvent.click(restartBtn())
    await screen.findByRole('status')
    expect(onRefresh).toHaveBeenCalled()
    expect(screen.queryByRole('button', { name: /restart server/i })).toBeNull()
  })

  it('waits for the new server to name a new pid before it settles', async () => {
    const stale = { ...base, server: { ...spawnedServer } }
    const fresh = { ...base, server: { ...spawnedServer, pid: 5555 } }
    const onRefresh = vi.fn().mockResolvedValueOnce(stale).mockResolvedValueOnce(stale).mockResolvedValue(fresh)
    renderForm(needing, { onRefresh })
    fireEvent.click(restartBtn())
    await screen.findByRole('status')
    expect(onRefresh.mock.calls.length).toBeGreaterThanOrEqual(3)
  })

  it('surfaces a failure and puts the button back', async () => {
    const onRestart = vi.fn().mockRejectedValue(new Error('the knomit server was started outside the app (pid 777)'))
    renderForm(needing, { onRestart })
    fireEvent.click(restartBtn())
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent(/pid 777/))
    expect(screen.getByRole('button', { name: /restart server/i })).toBeEnabled()
  })

  // A restart that never answers must not leave the button on "Restarting…"
  // for ever: that was the bug this replaces.
  it('times out with a message and puts the button back', async () => {
    const onRestart = vi.fn().mockReturnValue(new Promise<void>(() => {}))
    renderForm(needing, { onRestart, restartTimeoutMs: 30 })
    fireEvent.click(restartBtn())
    expect(screen.getByRole('button', { name: /restarting/i })).toBeDisabled()
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent(/did not come back within/i))
    expect(screen.getByRole('button', { name: /restart server/i })).toBeEnabled()
  })
})

describe('SettingsForm fleet listener ([tls].addr)', () => {
  it('saves the address with everything else', async () => {
    const { onSave } = renderForm()
    fireEvent.change(screen.getByLabelText(/fleet listener/i), { target: { value: '0.0.0.0:19279' } })
    save()
    await waitFor(() => expect(onSave).toHaveBeenCalledTimes(1))
    expect(vi.mocked(onSave).mock.calls[0][0]).toEqual({ ...base, tlsAddr: '0.0.0.0:19279' })
  })

  it('is read-only and says why when KNOMIT_TLS_ADDR owns it', () => {
    renderForm({ overriddenByEnv: ['KNOMIT_TLS_ADDR'], tlsAddr: '0.0.0.0:20001' })
    expect(screen.getByLabelText(/fleet listener/i)).toHaveAttribute('readonly')
    expect(screen.getByText(/KNOMIT_TLS_ADDR/)).toBeInTheDocument()
    expect(screen.getByLabelText(/^port$/i)).not.toHaveAttribute('readonly')
  })

  it.each([
    ['0.0.0.0', /host:port/],
    ['0.0.0.0:https', /host:port/],
    ['0.0.0.0:443', /between 1024 and 65535/],
    ['0.0.0.0:19278', /cannot share port 19278/],
    ['https://0.0.0.0:19279', /host:port/],
  ])('refuses %s before the round trip, on the field', async (addr, text) => {
    const { onSave } = renderForm()
    fireEvent.change(screen.getByLabelText(/fleet listener/i), { target: { value: addr } })
    save()
    expect(await screen.findByRole('alert')).toHaveTextContent(text)
    expect(screen.getByLabelText(/fleet listener/i)).toHaveAttribute('aria-invalid', 'true')
    expect(onSave).not.toHaveBeenCalled()
  })

  it.each(['', '0.0.0.0:19279', ':19279', '[::]:19279', 'knomit.lan:20000'])('accepts %j', async (addr) => {
    const { onSave } = renderForm({ tlsAddr: '0.0.0.0:21000' })
    fireEvent.change(screen.getByLabelText(/fleet listener/i), { target: { value: addr } })
    save()
    await waitFor(() => expect(onSave).toHaveBeenCalledTimes(1))
  })

  // A reload applies the fleet listener now: a changed address is a save, not a
  // restart.
  it('applies an address change by reload: no restart is offered', async () => {
    renderForm()
    fireEvent.change(screen.getByLabelText(/fleet listener/i), { target: { value: '0.0.0.0:19279' } })
    save()
    expect(await screen.findByRole('status')).toHaveTextContent(/saved and applied/i)
    expect(screen.queryByRole('button', { name: /restart server/i })).toBeNull()
  })

  it('names the Fleet port in its hint', () => {
    renderForm()
    expect(screen.getByText(/fleet port, host:port/i)).toBeInTheDocument()
  })
})
