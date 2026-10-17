import { render, screen, fireEvent, waitFor, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { EXTERNAL_ADDRESS_COPY, SettingsForm, parseExternalAddresses, type SaveAck, type Settings } from './SettingsForm.tsx'
import { ack, attachedServer, baseSettings as base, spawnedServer } from './settingsFixtures.ts'

// The External address row (PR #545 option a): Settings → Network → Fleet
// edits external_addresses, which a reload does not apply, so a save that the
// server answers with external_addresses in restart_needed restarts the
// server it spawned, through the window's own restart.

const nav = () => screen.getByRole('navigation', { name: /settings sections/i })
const goTo = (page: RegExp) => fireEvent.click(within(nav()).getByRole('button', { name: page }))
const save = () => fireEvent.click(screen.getByRole('button', { name: /^save$/i }))
const field = () => screen.getByLabelText(EXTERNAL_ADDRESS_COPY.label) as HTMLInputElement

function mount(initial: Partial<Settings>, onSave: (s: Settings) => Promise<SaveAck>, extra: {
  onRestart?: () => Promise<void>; onRefresh?: () => Promise<Settings>
} = {}) {
  const onRestart = extra.onRestart ?? vi.fn().mockResolvedValue(undefined)
  render(
    <SettingsForm initial={{ ...base, ...initial }} pollMs={1} onSave={onSave} onRestart={onRestart}
      onRevealLog={vi.fn()} onCancel={vi.fn()} onRefresh={extra.onRefresh} />,
  )
  goTo(/network/i)
  return { onRestart }
}

describe('SettingsForm External address', () => {
  it('parses commas and spaces into a list', () => {
    expect(parseExternalAddresses(' https://a.example:19279, https://b.example  https://c.example ,')).toEqual([
      'https://a.example:19279', 'https://b.example', 'https://c.example',
    ])
    expect(parseExternalAddresses('')).toEqual([])
  })

  // SABOTAGE: send initial.externalAddresses instead of the edited list -> red.
  it('shows the saved list and saves what was typed, as a list', async () => {
    const onSave = vi.fn().mockResolvedValue(ack())
    mount({ externalAddresses: ['https://old.example'] }, onSave)
    expect(field()).toHaveValue('https://old.example')
    fireEvent.change(field(), { target: { value: 'https://kb-01.example:19279, https://kb-01.lan:19279' } })
    save()
    await waitFor(() => expect(onSave).toHaveBeenCalled())
    expect(onSave.mock.calls[0][0].externalAddresses).toEqual(['https://kb-01.example:19279', 'https://kb-01.lan:19279'])
  })

  // SABOTAGE: drop the externalAddresses check from validate -> onSave runs -> red.
  it.each(['http://kb.example:19279', 'https://kb.example/git', 'kb.example:19279', 'https://u@kb.example'])(
    'refuses %s inline, on its field, and saves nothing', async (bad) => {
      const onSave = vi.fn().mockResolvedValue(ack())
      mount({}, onSave)
      fireEvent.change(field(), { target: { value: `https://ok.example, ${bad}` } })
      save()
      expect(await screen.findByText(EXTERNAL_ADDRESS_COPY.invalid(bad))).toBeInTheDocument()
      expect(field()).toHaveAttribute('aria-invalid', 'true')
      expect(onSave).not.toHaveBeenCalled()
    })

  // SABOTAGE: drop the restart after the save -> onRestart is never called -> red.
  it('restarts the server it spawned when the save leaves external_addresses waiting for a restart', async () => {
    const onSave = vi.fn().mockResolvedValue(ack({ outcome: 'restart_needed', restartNeeded: ['external_addresses'] }))
    const onRefresh = vi.fn().mockResolvedValue({ ...base, server: { ...spawnedServer, pid: 5151 } })
    const { onRestart } = mount({}, onSave, { onRefresh })
    fireEvent.change(field(), { target: { value: 'https://kb-01.example:19279' } })
    save()
    await waitFor(() => expect(onRestart).toHaveBeenCalledTimes(1))
    expect(await screen.findByRole('status')).toHaveTextContent('Server restarted.')
  })

  // Positive controls: a save that leaves nothing for a restart, and an
  // attached server (which the app may not restart), restart nothing.
  it('restarts nothing when the reload applied everything', async () => {
    const { onRestart } = mount({}, vi.fn().mockResolvedValue(ack()))
    fireEvent.change(field(), { target: { value: 'https://kb-01.example:19279' } })
    save()
    await screen.findByRole('status')
    expect(onRestart).not.toHaveBeenCalled()
  })

  it('an attached server is not restarted: the bar says where to restart it', async () => {
    const onSave = vi.fn().mockResolvedValue(ack({ outcome: 'restart_needed', restartNeeded: ['external_addresses'] }))
    const { onRestart } = mount({ server: attachedServer }, onSave)
    fireEvent.change(field(), { target: { value: 'https://kb-01.example:19279' } })
    save()
    expect(await screen.findByText(/external_addresses/)).toHaveTextContent('restart it where it was started')
    expect(onRestart).not.toHaveBeenCalled()
  })
})
