// The Fleet identity section's wire types and wording (knomit#256).
//
// These mirror FleetIdentity and FleetTLS in
// tools/desktop/identity.go field for field. The key lists are pinned from
// BOTH sides against fleetWire.json: Go's TestFleetIdentity_WireKeysMatch-
// TheUIFixture marshals the structs against it, and fleet.test.ts checks the
// fixtures below — typed as these interfaces — against it. A renamed field
// therefore fails a test instead of rendering nothing.
//
// Nothing here carries free text from a bundle: fingerprints, the email SAN
// (shown only when Go admits its characters), numbers and dates. That is why the
// section can render every field as plain text.

export interface FleetTLS {
  /** false while the server is still starting: the rest is not known yet. */
  booted: boolean
  /** [tls].addr as this process booted with it; "" = off. */
  configured: string
  /** The bound address; "" when not listening. */
  listening: string
  /** Why configured is not listening: 'no_certificate' | 'addr_in_use' | ''. */
  reason: string
  /** [tls].addr in the config now; differs from configured until a restart. */
  configuredNow: string
}

export interface FleetIdentity {
  /** 'ready', or 'no_key' when Settings opened before boot created the key. */
  state: string
  /** The [tls].dir this read, where a fleet join installs its bundle. */
  dir: string
  keyFingerprint: string
  enrolled: boolean
  /** 'none' | 'ok' | 'unreadable' */
  certificate: string
  principal: string
  san: string
  serial: string
  notAfter: string
  rootFingerprint: string
  crlNumber: string
  crlNextUpdate: string
  crlStale: boolean
  tls: FleetTLS
}

/** The one-line state of the fleet listener, and how loudly to show it. */
export function listenerStatus(tls: FleetTLS): { tone: 'ok' | 'warn' | 'muted'; text: string } {
  const pending =
    tls.booted && tls.configuredNow !== tls.configured
      ? tls.configuredNow
        ? ` Set to ${tls.configuredNow}; applies after a restart.`
        : ' Turned off; applies after a restart.'
      : ''
  if (!tls.booted) return { tone: 'muted', text: 'Starting…' }
  if (tls.listening) return { tone: 'ok', text: `Listening on ${tls.listening} for enrolled peers.${pending}` }
  if (!tls.configured) return { tone: 'muted', text: `Off.${pending}` }
  if (tls.reason === 'no_certificate')
    return {
      tone: 'warn',
      text: `Configured (${tls.configured}), not listening: no certificate installed.${pending}`,
    }
  if (tls.reason === 'addr_in_use')
    return {
      tone: 'warn',
      text: `Configured (${tls.configured}), not listening: the address is held by another program, usually knomit serve on this home.${pending}`,
    }
  return { tone: 'warn', text: `Configured (${tls.configured}), not listening.${pending}` }
}
