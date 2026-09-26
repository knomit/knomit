import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { FleetIdentitySection } from './FleetIdentity.tsx'
import type { FleetIdentity, InstallResult } from './fleet.ts'
import { FP, ROOT_A, ROOT_B, enrolled, notEnrolled } from './fleetFixtures.ts'
import wire from './fleetWire.json'

const BUNDLE =
  '-----BEGIN CERTIFICATE-----\nMIIBleaf\n-----END CERTIFICATE-----\n' +
  '-----BEGIN CERTIFICATE-----\nMIIBroot\n-----END CERTIFICATE-----\n' +
  '-----BEGIN X509 CRL-----\nMIIBcrl\n-----END X509 CRL-----\n'

const ok = (identity: FleetIdentity | null = enrolled): InstallResult => ({
  installed: true,
  class: '',
  message: '',
  installedRootFingerprint: '',
  bundleRootFingerprint: '',
  bundlePrincipal: '',
  identity,
})

const refused = (cls: string, extra: Partial<InstallResult> = {}): InstallResult => ({
  installed: false,
  class: cls,
  message: '',
  installedRootFingerprint: '',
  bundleRootFingerprint: '',
  bundlePrincipal: '',
  identity: null,
  ...extra,
})

function renderSection(identity: FleetIdentity | null, props: Partial<Parameters<typeof FleetIdentitySection>[0]> = {}) {
  const handlers = {
    onCopyPublicKey: vi.fn().mockResolvedValue(undefined),
    onReadClipboard: vi.fn().mockResolvedValue(BUNDLE),
    onInstall: vi.fn().mockResolvedValue(ok()),
    onInstalled: vi.fn(),
    ...props,
  }
  const view = render(<FleetIdentitySection identity={identity} {...handlers} />)
  return { ...handlers, view }
}

async function pasteBundle() {
  fireEvent.click(screen.getByRole('button', { name: /paste from clipboard/i }))
  await screen.findByText(/bundle loaded/i)
}

describe('fleetWire.json', () => {
  // The other half of Go's TestFleetIdentity_WireKeysMatchTheUIFixture: the
  // fixtures are typed as the TS interfaces, so tsc ties interface to fixture
  // and this ties fixture to the file Go is checked against.
  it('pins the keys the fixtures (and so the TS types) carry', () => {
    expect(Object.keys(enrolled).sort()).toEqual([...wire.identity].sort())
    expect(Object.keys(enrolled.tls).sort()).toEqual([...wire.tls].sort())
    expect(Object.keys(ok()).sort()).toEqual([...wire.installResult].sort())
  })
})

describe('FleetIdentitySection states', () => {
  it('says the key is still being created on a first launch', () => {
    renderSection({ ...notEnrolled, state: 'no_key', keyFingerprint: '' })
    expect(screen.getByText(/created once knomit has started/i)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /copy public key/i })).not.toBeInTheDocument()
  })

  it('shows a not-enrolled instance with its fingerprint and no certificate', () => {
    renderSection(notEnrolled)
    expect(screen.getByText(FP.slice(0, 8))).toBeInTheDocument()
    expect(screen.getByText(/not enrolled/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /copy public key/i })).toBeInTheDocument()
  })

  it('shows an enrolled instance: principal, expiry, root and CRL — fingerprints, never names', () => {
    renderSection(enrolled)
    expect(screen.getByText(enrolled.san)).toBeInTheDocument()
    expect(screen.getByText(/2026-12-24/)).toBeInTheDocument()
    expect(screen.getByText(ROOT_A.slice(0, 16), { exact: false })).toBeInTheDocument()
    expect(screen.getByText(/#3/)).toBeInTheDocument()
    expect(screen.queryByText(/overdue/i)).not.toBeInTheDocument()
  })

  it('flags a CRL past its next update', () => {
    renderSection({ ...enrolled, crlStale: true })
    expect(screen.getByText(/overdue/i)).toBeInTheDocument()
  })

  it('reports an unreadable certificate', () => {
    renderSection({ ...enrolled, certificate: 'unreadable', principal: '', san: '' })
    expect(screen.getByText(/certificate is unreadable/i)).toBeInTheDocument()
  })

  it.each([
    [{ booted: false, configured: '', listening: '', reason: '', configuredNow: '' }, /starting/i],
    [{ booted: true, configured: '', listening: '', reason: '', configuredNow: '' }, /^off\.$/i],
    [enrolled.tls, /listening on \[::\]:19279/i],
    [{ booted: true, configured: '0.0.0.0:19279', listening: '', reason: 'no_certificate', configuredNow: '0.0.0.0:19279' }, /not listening: no certificate installed/i],
    [{ booted: true, configured: '0.0.0.0:19279', listening: '', reason: 'addr_in_use', configuredNow: '0.0.0.0:19279' }, /held by another program/i],
    [{ booted: true, configured: '', listening: '', reason: '', configuredNow: '0.0.0.0:19279' }, /applies after a restart/i],
  ])('states the listener: %o', (tls, text) => {
    renderSection({ ...enrolled, tls })
    expect(screen.getByTestId('fleet-listener')).toHaveTextContent(text)
  })
})

describe('FleetIdentitySection actions', () => {
  it('copies the public key through the handler and says so', async () => {
    const { onCopyPublicKey } = renderSection(notEnrolled)
    fireEvent.click(screen.getByRole('button', { name: /copy public key/i }))
    await screen.findByText(/copied/i)
    expect(onCopyPublicKey).toHaveBeenCalledTimes(1)
  })

  it('reports a copy that failed', async () => {
    renderSection(notEnrolled, { onCopyPublicKey: vi.fn().mockRejectedValue(new Error('no key yet')) })
    fireEvent.click(screen.getByRole('button', { name: /copy public key/i }))
    expect(await screen.findByRole('alert')).toHaveTextContent('no key yet')
  })

  it('installs nothing until a bundle is loaded, and never renders the bundle', async () => {
    const { onInstall, view } = renderSection(notEnrolled)
    expect(screen.getByRole('button', { name: /^install$/i })).toBeDisabled()
    await pasteBundle()
    expect(view.container.textContent).not.toMatch(/BEGIN|MIIB/)
    fireEvent.click(screen.getByRole('button', { name: /^install$/i }))
    await waitFor(() => expect(onInstall).toHaveBeenCalledTimes(1))
    expect(onInstall).toHaveBeenCalledWith(BUNDLE, '', '')
  })

  it('says an empty clipboard is empty instead of loading it', async () => {
    const { onInstall } = renderSection(notEnrolled, { onReadClipboard: vi.fn().mockResolvedValue('  ') })
    fireEvent.click(screen.getByRole('button', { name: /paste from clipboard/i }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/clipboard is empty/i)
    expect(screen.getByRole('button', { name: /^install$/i })).toBeDisabled()
    expect(onInstall).not.toHaveBeenCalled()
  })

  it('loads a bundle from a chosen file, naming the file and not its contents', async () => {
    const { onInstall, view } = renderSection(notEnrolled)
    const file = new File([BUNDLE], 'laptop.pem', { type: 'application/x-pem-file' })
    fireEvent.change(screen.getByLabelText(/choose file/i), { target: { files: [file] } })
    await screen.findByText(/laptop\.pem/)
    expect(view.container.textContent).not.toMatch(/BEGIN|MIIB/)
    fireEvent.click(screen.getByRole('button', { name: /^install$/i }))
    await waitFor(() => expect(onInstall).toHaveBeenCalledWith(BUNDLE, '', ''))
  })

  it('on success says so, drops the bundle and hands the new identity up', async () => {
    const { onInstalled } = renderSection(notEnrolled)
    await pasteBundle()
    fireEvent.click(screen.getByRole('button', { name: /^install$/i }))
    await screen.findByText(/^installed\./i)
    expect(onInstalled).toHaveBeenCalledWith(enrolled)
    expect(screen.queryByText(/bundle loaded/i)).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^install$/i })).toBeDisabled()
  })

  it.each(['malformed', 'key_mismatch', 'chain', 'crl_invalid', 'crl_rollback'])(
    'maps the %s class to its sentence, and keeps the bundle for another try',
    async (cls) => {
      const { onInstalled } = renderSection(enrolled, { onInstall: vi.fn().mockResolvedValue(refused(cls)) })
      await pasteBundle()
      fireEvent.click(screen.getByRole('button', { name: /^install$/i }))
      const alert = await screen.findByRole('alert')
      const { CLASS_TEXT } = await import('./fleet.ts')
      expect(alert).toHaveTextContent(CLASS_TEXT[cls].slice(0, 30))
      expect(onInstalled).not.toHaveBeenCalled()
      expect(screen.getByText(/bundle loaded/i)).toBeInTheDocument()
    },
  )

  it('shows the message only for the error class', async () => {
    renderSection(enrolled, {
      onInstall: vi.fn().mockResolvedValue(refused('error', { message: 'write /x/pki/root.crt: disk full' })),
    })
    await pasteBundle()
    fireEvent.click(screen.getByRole('button', { name: /^install$/i }))
    expect(await screen.findByRole('alert')).toHaveTextContent('disk full')
  })

  it('reports a rejected call (e.g. not the Settings window) as an error', async () => {
    renderSection(enrolled, { onInstall: vi.fn().mockRejectedValue(new Error('this action is available only from the Settings window')) })
    await pasteBundle()
    fireEvent.click(screen.getByRole('button', { name: /^install$/i }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/only from the settings window/i)
  })
})

describe('FleetIdentitySection replace-root confirmation', () => {
  const differs = refused('root_differs', { installedRootFingerprint: ROOT_A, bundleRootFingerprint: ROOT_B })

  // The confirmation is a separate, explicit control that names BOTH roots,
  // and the replace is only ever a second call carrying the pair that was
  // shown — never a default, never the first call.
  it('asks before replacing, naming both roots, and sends back exactly that pair', async () => {
    const onInstall = vi.fn().mockResolvedValueOnce(differs).mockResolvedValueOnce(ok())
    const { onInstalled } = renderSection(enrolled, { onInstall })
    await pasteBundle()
    fireEvent.click(screen.getByRole('button', { name: /^install$/i }))

    const confirm = await screen.findByRole('group', { name: /replace the fleet root/i })
    expect(confirm).toHaveTextContent(ROOT_A)
    expect(confirm).toHaveTextContent(ROOT_B)
    // Nothing more was sent while the question is on screen.
    expect(onInstall).toHaveBeenCalledTimes(1)
    expect(onInstall).toHaveBeenLastCalledWith(BUNDLE, '', '')

    fireEvent.click(screen.getByRole('button', { name: /replace fleet root/i }))
    await waitFor(() => expect(onInstall).toHaveBeenCalledTimes(2))
    expect(onInstall).toHaveBeenLastCalledWith(BUNDLE, ROOT_A, ROOT_B)
    await screen.findByText(/^installed\./i)
    expect(onInstalled).toHaveBeenCalledWith(enrolled)
    expect(screen.queryByRole('group', { name: /replace the fleet root/i })).not.toBeInTheDocument()
  })

  it('keeping the current fleet sends nothing and closes the question', async () => {
    const onInstall = vi.fn().mockResolvedValue(differs)
    renderSection(enrolled, { onInstall })
    await pasteBundle()
    fireEvent.click(screen.getByRole('button', { name: /^install$/i }))
    await screen.findByRole('group', { name: /replace the fleet root/i })
    fireEvent.click(screen.getByRole('button', { name: /keep the current fleet/i }))
    expect(screen.queryByRole('group', { name: /replace the fleet root/i })).not.toBeInTheDocument()
    expect(onInstall).toHaveBeenCalledTimes(1)
  })

  it('a stale confirmation is asked again with the roots as they are now', async () => {
    const ROOT_C = '3333333333333333333333333333333333333333333333333333333333333333'
    const onInstall = vi
      .fn()
      .mockResolvedValueOnce(differs)
      .mockResolvedValueOnce(refused('confirmation_stale', { installedRootFingerprint: ROOT_C, bundleRootFingerprint: ROOT_B }))
      .mockResolvedValueOnce(ok())
    renderSection(enrolled, { onInstall })
    await pasteBundle()
    fireEvent.click(screen.getByRole('button', { name: /^install$/i }))
    await screen.findByRole('group', { name: /replace the fleet root/i })
    fireEvent.click(screen.getByRole('button', { name: /replace fleet root/i }))

    const again = await screen.findByText(/changed since/i)
    expect(again).toBeInTheDocument()
    const confirm = screen.getByRole('group', { name: /replace the fleet root/i })
    expect(confirm).toHaveTextContent(ROOT_C)
    expect(confirm).not.toHaveTextContent(ROOT_A)

    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: /replace fleet root/i }))
    })
    expect(onInstall).toHaveBeenLastCalledWith(BUNDLE, ROOT_C, ROOT_B)
  })
})

describe('FleetIdentitySection first-install confirmation (knomit#299)', () => {
  const PRINCIPAL = `instance:${FP}@cert`
  const unconfirmed = refused('root_unconfirmed', { bundleRootFingerprint: ROOT_B, bundlePrincipal: PRINCIPAL })

  // A first install shows the root and the principal and asks the user to
  // check the root against the operator's value; only "Join this fleet"
  // sends ("", that root) back.
  it('shows the fleet root and the principal, and joins only on an explicit confirmation', async () => {
    const onInstall = vi.fn().mockResolvedValueOnce(unconfirmed).mockResolvedValueOnce(ok())
    const { onInstalled } = renderSection(notEnrolled, { onInstall })
    await pasteBundle()
    fireEvent.click(screen.getByRole('button', { name: /^install$/i }))

    const confirm = await screen.findByRole('group', { name: /confirm the fleet root/i })
    expect(confirm).toHaveTextContent(ROOT_B)
    expect(confirm).toHaveTextContent(PRINCIPAL)
    expect(confirm).toHaveTextContent(/fleet operator gave you/i)
    expect(confirm).not.toHaveTextContent(/current root/i)
    expect(onInstall).toHaveBeenCalledTimes(1)

    fireEvent.click(screen.getByRole('button', { name: /join this fleet/i }))
    await waitFor(() => expect(onInstall).toHaveBeenCalledTimes(2))
    expect(onInstall).toHaveBeenLastCalledWith(BUNDLE, '', ROOT_B)
    await screen.findByText(/^installed\./i)
    expect(onInstalled).toHaveBeenCalledWith(enrolled)
  })

  it('cancelling sends nothing and closes the question', async () => {
    const onInstall = vi.fn().mockResolvedValue(unconfirmed)
    renderSection(notEnrolled, { onInstall })
    await pasteBundle()
    fireEvent.click(screen.getByRole('button', { name: /^install$/i }))
    await screen.findByRole('group', { name: /confirm the fleet root/i })
    fireEvent.click(screen.getByRole('button', { name: /^cancel$/i }))
    expect(screen.queryByRole('group', { name: /confirm the fleet root/i })).not.toBeInTheDocument()
    expect(onInstall).toHaveBeenCalledTimes(1)
  })

  // Another process enrolled this home between the question and the answer:
  // the confirmation comes back stale naming the root now installed, and the
  // question becomes a replace.
  it('a root installed underneath turns the question into a replace', async () => {
    const onInstall = vi
      .fn()
      .mockResolvedValueOnce(unconfirmed)
      .mockResolvedValueOnce(
        refused('confirmation_stale', { installedRootFingerprint: ROOT_A, bundleRootFingerprint: ROOT_B, bundlePrincipal: PRINCIPAL }),
      )
    renderSection(notEnrolled, { onInstall })
    await pasteBundle()
    fireEvent.click(screen.getByRole('button', { name: /^install$/i }))
    await screen.findByRole('group', { name: /confirm the fleet root/i })
    fireEvent.click(screen.getByRole('button', { name: /join this fleet/i }))

    await screen.findByText(/changed since/i)
    const confirm = screen.getByRole('group', { name: /replace the fleet root/i })
    expect(confirm).toHaveTextContent(ROOT_A)
    expect(confirm).toHaveTextContent(ROOT_B)
  })

  // Another program installed the bundle's OWN root between the question and
  // the answer: nothing is replaced, so the copy must not say "different
  // fleet" over two identical roots, nor "changed since you were asked".
  it('a stale answer whose two roots are the same is offered as a renewal, not a replace', async () => {
    const onInstall = vi
      .fn()
      .mockResolvedValueOnce(unconfirmed)
      .mockResolvedValueOnce(
        refused('confirmation_stale', { installedRootFingerprint: ROOT_B, bundleRootFingerprint: ROOT_B, bundlePrincipal: PRINCIPAL }),
      )
      .mockResolvedValueOnce(ok())
    renderSection(notEnrolled, { onInstall })
    await pasteBundle()
    fireEvent.click(screen.getByRole('button', { name: /^install$/i }))
    await screen.findByRole('group', { name: /confirm the fleet root/i })
    fireEvent.click(screen.getByRole('button', { name: /join this fleet/i }))

    const confirm = await screen.findByText(/another program installed/i)
    expect(confirm).toBeInTheDocument()
    expect(screen.queryByText(/different fleet/i)).not.toBeInTheDocument()
    expect(screen.queryByText(/changed since/i)).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /replace fleet root/i })).not.toBeInTheDocument()
    await act(async () => {
      fireEvent.click(within(screen.getByRole('group', { name: /confirm the fleet root/i })).getByRole('button', { name: /^install$/i }))
    })
    expect(onInstall).toHaveBeenLastCalledWith(BUNDLE, ROOT_B, ROOT_B)
  })
})
