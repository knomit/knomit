// The Fleet identity section's wire types and wording (knomit#256).
//
// These mirror FleetIdentity, FleetTLS and InstallResult in
// tools/desktop/identity.go field for field. The key lists are pinned from
// BOTH sides against fleetWire.json: Go's TestFleetIdentity_WireKeysMatch-
// TheUIFixture marshals the structs against it, and fleet.test.ts checks the
// fixtures below — typed as these interfaces — against it. A renamed field
// therefore fails a test instead of rendering nothing.
//
// Nothing here carries free text from a bundle: fingerprints, a SAN whose host
// Go has validated as hostname characters, numbers and dates. That is why the
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
  /** The [tls].dir this read, where a bundle is installed. */
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

export interface InstallResult {
  installed: boolean
  /** '' on success, else one of the keys of CLASS_TEXT. */
  class: string
  /** Set for class 'error' only; never bundle text. */
  message: string
  /** "" when no root is installed. Set with the next two for root_unconfirmed, root_differs and confirmation_stale. */
  installedRootFingerprint: string
  bundleRootFingerprint: string
  /** <kind>:<fingerprint>@cert, the principal the bundle's certificate names. */
  bundlePrincipal: string
  identity: FleetIdentity | null
}

/**
 * What each refusal class means to the person holding the bundle. The class
 * is all the UI learns about a bundle-derived refusal — Go never sends the
 * refusal's text — so each sentence says what to do next.
 */
export const CLASS_TEXT: Record<string, string> = {
  malformed:
    'That is not an enrollment bundle. It must hold this instance’s certificate, the fleet root certificate and a revocation list, as the operator’s knomit identity enroll writes it.',
  key_mismatch:
    'This bundle was issued for a different key. Send the operator this instance’s public key (Copy public key) and ask for a new bundle.',
  chain:
    'The certificate in this bundle does not verify against the fleet root it came with: it is expired, revoked, or not issued by that root.',
  crl_invalid: 'The bundle’s revocation list is not signed by its fleet root.',
  crl_rollback:
    'The bundle’s revocation list is older than the one this instance already holds. Ask the operator for a current bundle.',
  error: 'The bundle could not be installed.',
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

/**
 * Whether an install just done needs a restart to take effect: the listener
 * is wanted (an address is configured now) but this process is not
 * listening. A listener that IS running adopts new files by itself.
 */
export function installNeedsRestart(id: FleetIdentity | null): boolean {
  return !!id && id.tls.booted && id.tls.configuredNow !== '' && id.tls.listening === ''
}
