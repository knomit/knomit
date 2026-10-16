import { useEffect, useState } from 'react'
import { listenerStatus, type FleetIdentity } from './fleet.ts'
import { CopyIcon } from './icons.tsx'
import { Row } from './Row.tsx'

interface Props {
  /** null while GetIdentity is in flight. */
  identity: FleetIdentity | null
  /** PublicKeyLine, then onto the clipboard. Rejects with a reason to show. */
  onCopyPublicKey: () => Promise<void>
}

type Outcome = { kind: 'error'; text: string } | null

function message(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}

/** The date part of an RFC 3339 timestamp: the day is what a person plans by. */
const day = (ts: string) => ts.slice(0, 10)

/**
 * Settings → Fleet identity (knomit#256): this instance's key, enrolment and
 * fleet listener. Copy the public key for the operator. Joining a fleet is
 * not done here: it is the main window's Fleet → New fleet → Join a fleet.
 */
export function FleetIdentitySection({ identity, onCopyPublicKey }: Props) {
  const [outcome, setOutcome] = useState<Outcome>(null)
  const [copied, setCopied] = useState(false)

  // "Copied." reports an event; it must expire, like the form's "Saved.".
  useEffect(() => {
    if (!copied) return
    const t = setTimeout(() => setCopied(false), 2600)
    return () => clearTimeout(t)
  }, [copied])

  async function copy() {
    setOutcome(null)
    try {
      await onCopyPublicKey()
      setCopied(true)
    } catch (e) {
      setOutcome({ kind: 'error', text: message(e) })
    }
  }

  if (!identity) {
    return (
      <section className="fleet" aria-labelledby="fleet-h">
        <h3 id="fleet-h" className="gh">
          Fleet identity
        </h3>
        <p className="fleet-note">Loading…</p>
      </section>
    )
  }

  const listener = listenerStatus(identity.tls)
  const noKey = identity.state === 'no_key'

  return (
    <section className="fleet" aria-labelledby="fleet-h">
      <h3 id="fleet-h" className="gh">
        Fleet identity
      </h3>
      <div className="card">
        <Row
          label="This instance"
          control={
            noKey ? (
              <span className="fleet-note">The key is created once Knomit has started.</span>
            ) : (
              <>
                <code className="fp" title={identity.keyFingerprint}>
                  {identity.keyFingerprint.slice(0, 8)}
                </code>
                <button type="button" className="linkbtn" onClick={copy}>
                  <CopyIcon color="currentColor" size={12} />
                  Copy public key
                </button>
                {copied && (
                  <span role="status" className="fleet-ok">
                    Copied.
                  </span>
                )}
              </>
            )
          }
        />

        <Row
          label="Enrollment"
          sub={identity.certificate === 'ok' ? `until ${day(identity.notAfter)}` : undefined}
          control={
            identity.certificate === 'ok' ? (
              <code className="fleet-san">{identity.san}</code>
            ) : identity.certificate === 'unreadable' ? (
              <span className="fleet-bad">The installed certificate is unreadable.</span>
            ) : (
              <span className="fleet-note">Not enrolled</span>
            )
          }
        >
          {identity.enrolled && identity.rootFingerprint && (
            <p className="rnote fleet-sub">
              root <code title={identity.rootFingerprint}>{identity.rootFingerprint.slice(0, 16)}…</code>
              {identity.crlNumber && (
                <>
                  {' · '}revocation list #{identity.crlNumber}
                  {identity.crlNextUpdate &&
                    (identity.crlStale ? (
                      <span className="fleet-warn"> — overdue since {day(identity.crlNextUpdate)}; still enforced</span>
                    ) : (
                      <>, next by {day(identity.crlNextUpdate)}</>
                    ))}
                </>
              )}
            </p>
          )}
        </Row>

        <Row label="Listener" layout="stack">
          <p data-testid="fleet-listener" className={`rnote fleet-listener is-${listener.tone}`}>
            {listener.tone === 'ok' && <span className="pip is-ok" aria-hidden="true" />}
            {listener.text}
          </p>
        </Row>

        <Row label="Join a fleet" layout="stack">
          <p className="rnote">Create or join a fleet from the main window: Fleet in the sidebar, then the + button.</p>
          {outcome && (
            <p role="alert" className="rnote err">
              {outcome.text}
            </p>
          )}
        </Row>
      </div>
    </section>
  )
}
