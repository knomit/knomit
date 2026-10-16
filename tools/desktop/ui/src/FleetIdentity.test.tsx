import { fireEvent, render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { FleetIdentitySection } from './FleetIdentity.tsx'
import type { FleetIdentity } from './fleet.ts'
import { FP, ROOT_A, enrolled, notEnrolled } from './fleetFixtures.ts'
import wire from './fleetWire.json'

function renderSection(identity: FleetIdentity | null, props: Partial<Parameters<typeof FleetIdentitySection>[0]> = {}) {
  const handlers = {
    onCopyPublicKey: vi.fn().mockResolvedValue(undefined),
    ...props,
  }
  const view = render(<FleetIdentitySection identity={identity} {...handlers} />)
  return { ...handlers, view }
}

describe('fleetWire.json', () => {
  // The other half of Go's TestFleetIdentity_WireKeysMatchTheUIFixture: the
  // fixtures are typed as the TS interfaces, so tsc ties interface to fixture
  // and this ties fixture to the file Go is checked against.
  it('pins the keys the fixtures (and so the TS types) carry', () => {
    expect(Object.keys(enrolled).sort()).toEqual([...wire.identity].sort())
    expect(Object.keys(enrolled.tls).sort()).toEqual([...wire.tls].sort())
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

  it('points at the main window for joining a fleet and offers no install of its own', () => {
    const { view } = renderSection(notEnrolled)
    expect(screen.getByText('Join a fleet')).toBeInTheDocument()
    expect(
      screen.getByText('Create or join a fleet from the main window: Fleet in the sidebar, then the + button.'),
    ).toBeInTheDocument()
    expect(screen.queryByText(/install bundle/i)).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /paste from clipboard/i })).not.toBeInTheDocument()
    expect(view.container.querySelector('input[type="file"]')).toBeNull()
  })
})
