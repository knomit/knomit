import { useEffect, useRef, useState } from 'react'
import { CLASS_TEXT, listenerStatus, type FleetIdentity, type InstallResult } from './fleet.ts'

interface Props {
  /** null while GetIdentity is in flight. */
  identity: FleetIdentity | null
  /** PublicKeyLine, then onto the clipboard. Rejects with a reason to show. */
  onCopyPublicKey: () => Promise<void>
  /** The clipboard's text, read through the Wails runtime. */
  onReadClipboard: () => Promise<string>
  /** InstallBundle(raw, confirmFrom, confirmTo). */
  onInstall: (raw: string, confirmFrom: string, confirmTo: string) => Promise<InstallResult>
  /** A bundle was installed; the identity as it stands now, when Go sent one. */
  onInstalled: (identity: FleetIdentity | null) => void
}

/** A loaded bundle: its text, which is sent and NEVER rendered, and where it came from. */
interface Loaded {
  raw: string
  source: string
}

type Outcome = { kind: 'ok' | 'error'; text: string } | null

/**
 * The pair a root confirmation sends back: ("", bundle root) for a first
 * install, (installed root, bundle root) for a move to another fleet.
 */
interface Confirm {
  from: string
  to: string
  principal: string
  stale: boolean
}

function message(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}

/** The date part of an RFC 3339 timestamp: the day is what a person plans by. */
const day = (ts: string) => ts.slice(0, 10)

/**
 * Settings → Fleet identity (knomit#256): what a desktop with no `knomit` CLI
 * needs to join a fleet. Copy this instance's public key for the operator;
 * load the bundle they send back, from the clipboard or a file; install it.
 *
 * The bundle is never rendered — only where it came from. Every refusal is a
 * CLASS from Go, mapped to a sentence here; Go never sends a bundle-derived
 * refusal's text. Trusting a fleet root is never a default (knomit#299): Go
 * refuses a bundle whose root the user has not seen — on a first install
 * (root_unconfirmed) as on a move to another fleet (root_differs) — naming
 * the fingerprints and the principal; this section asks the user to check the
 * root against the value the operator gave them out of band, and only an
 * explicit "Join this fleet" or "Replace fleet root" sends that exact pair
 * back.
 */
export function FleetIdentitySection({ identity, onCopyPublicKey, onReadClipboard, onInstall, onInstalled }: Props) {
  const [loaded, setLoaded] = useState<Loaded | null>(null)
  const [busy, setBusy] = useState(false)
  const [outcome, setOutcome] = useState<Outcome>(null)
  const [confirm, setConfirm] = useState<Confirm | null>(null)
  const [copied, setCopied] = useState(false)
  const fileInput = useRef<HTMLInputElement>(null)

  // "Copied." reports an event; it must expire, like the form's "Saved.".
  useEffect(() => {
    if (!copied) return
    const t = setTimeout(() => setCopied(false), 2600)
    return () => clearTimeout(t)
  }, [copied])

  function load(raw: string, source: string) {
    setOutcome(null)
    setConfirm(null)
    if (raw.trim() === '') {
      setLoaded(null)
      setOutcome({ kind: 'error', text: `The ${source} is empty.` })
      return
    }
    setLoaded({ raw, source })
  }

  async function paste() {
    try {
      load(await onReadClipboard(), 'clipboard')
    } catch (e) {
      setOutcome({ kind: 'error', text: message(e) })
    }
  }

  function pick(files: FileList | null) {
    const f = files?.[0]
    if (!f) return
    const r = new FileReader()
    r.onload = () => load(String(r.result ?? ''), f.name)
    r.onerror = () => setOutcome({ kind: 'error', text: `Could not read ${f.name}.` })
    r.readAsText(f)
    // Let the same file be chosen again after a refusal.
    if (fileInput.current) fileInput.current.value = ''
  }

  async function copy() {
    setOutcome(null)
    try {
      await onCopyPublicKey()
      setCopied(true)
    } catch (e) {
      setOutcome({ kind: 'error', text: message(e) })
    }
  }

  async function install(from = '', to = '') {
    if (!loaded) return
    setBusy(true)
    setOutcome(null)
    let res: InstallResult
    try {
      res = await onInstall(loaded.raw, from, to)
    } catch (e) {
      setBusy(false)
      setOutcome({ kind: 'error', text: message(e) })
      return
    }
    setBusy(false)
    if (res.installed) {
      setLoaded(null)
      setConfirm(null)
      setOutcome({ kind: 'ok', text: 'Installed.' })
      onInstalled(res.identity)
      return
    }
    if (res.class === 'root_unconfirmed' || res.class === 'root_differs' || res.class === 'confirmation_stale') {
      setConfirm({
        from: res.installedRootFingerprint,
        to: res.bundleRootFingerprint,
        principal: res.bundlePrincipal,
        stale: res.class === 'confirmation_stale',
      })
      return
    }
    setConfirm(null)
    const text = CLASS_TEXT[res.class] ?? CLASS_TEXT.error
    setOutcome({ kind: 'error', text: res.class === 'error' && res.message ? `${text} ${res.message}` : text })
  }

  if (!identity) {
    return (
      <section className="fleet" aria-labelledby="fleet-h">
        <h2 id="fleet-h" className="k-eyebrow fleet-h">
          Fleet identity
        </h2>
        <p className="fleet-note">Loading…</p>
      </section>
    )
  }

  const listener = listenerStatus(identity.tls)
  const noKey = identity.state === 'no_key'

  return (
    <section className="fleet" aria-labelledby="fleet-h">
      <h2 id="fleet-h" className="k-eyebrow fleet-h">
        Fleet identity
      </h2>
      <div className="list">
        <div className="row">
          <span className="row-label">This instance</span>
          <div className="control">
            {noKey ? (
              <span className="fleet-note">The key is created once Knomit has started.</span>
            ) : (
              <>
                <code className="fp" title={identity.keyFingerprint}>
                  {identity.keyFingerprint.slice(0, 8)}
                </code>
                <button type="button" className="linkbtn" onClick={copy}>
                  Copy public key
                </button>
                {copied && (
                  <span role="status" className="fleet-ok">
                    Copied.
                  </span>
                )}
              </>
            )}
          </div>
        </div>

        <div className="row">
          <span className="row-label">Enrollment</span>
          <div className="control fleet-facts">
            {identity.certificate === 'ok' ? (
              <>
                <code className="fleet-san">{identity.san}</code>
                <span className="hint">until {day(identity.notAfter)}</span>
              </>
            ) : identity.certificate === 'unreadable' ? (
              <span className="fleet-bad">The installed certificate is unreadable.</span>
            ) : (
              <span className="fleet-note">Not enrolled</span>
            )}
          </div>
          {identity.enrolled && identity.rootFingerprint && (
            <p className="sub fleet-sub">
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
        </div>

        <div className="row">
          <span className="row-label">Listener</span>
          <p data-testid="fleet-listener" className={`fleet-listener is-${listener.tone}`}>
            {listener.text}
          </p>
        </div>

        <div className="row">
          <span className="row-label">Install bundle</span>
          <div className="control fleet-install">
            <button type="button" className="k-btn" onClick={paste} disabled={busy}>
              Paste from clipboard
            </button>
            <label className="k-btn fleet-file">
              Choose file…
              <input
                ref={fileInput}
                type="file"
                accept=".pem,.crt,.txt,application/x-pem-file,text/plain"
                onChange={(e) => pick(e.target.files)}
                disabled={busy}
              />
            </label>
            <button
              type="button"
              className="k-btn is-primary"
              onClick={() => install()}
              disabled={!loaded || busy || confirm !== null}
            >
              {busy ? 'Installing…' : 'Install'}
            </button>
          </div>
          {loaded && (
            <p className="sub fleet-note">
              Bundle loaded from {loaded.source === 'clipboard' ? 'the clipboard' : loaded.source}.
            </p>
          )}
          {outcome &&
            (outcome.kind === 'ok' ? (
              <p role="status" className="sub fleet-ok">
                {outcome.text}
              </p>
            ) : (
              <p role="alert" className="sub err">
                {outcome.text}
              </p>
            ))}
        </div>
      </div>

      {confirm && (() => {
        // Another program installed the bundle's OWN root after the question
        // was asked: nothing is being replaced, the bundle renews under it.
        const same = confirm.from !== '' && confirm.from === confirm.to
        const move = confirm.from !== '' && !same
        return (
          <div
            role="group"
            aria-label={move ? 'Replace the fleet root' : 'Confirm the fleet root'}
            className="k-callout is-warn fleet-confirm"
          >
            {confirm.stale && !same && (
              <p className="fleet-warn">The fleet roots changed since you were asked. Check them again:</p>
            )}
            {same ? (
              <p>
                Since you were asked, another program installed this bundle’s fleet root on this instance. Installing the
                bundle now only renews this instance’s certificate under that root.
              </p>
            ) : move ? (
              <p>
                This bundle is from a <strong>different fleet</strong>. Installing it moves this instance to that fleet:
                peers of the current fleet will refuse it.
              </p>
            ) : (
              <p>
                This instance is joining a fleet. Anyone who can write your clipboard or a file can hand you a bundle, so
                first check the fleet root below against the fingerprint the fleet operator gave you directly.
              </p>
            )}
            <dl className="fleet-roots">
              {move && (
                <>
                  <dt>Current root</dt>
                  <dd>
                    <code>{confirm.from}</code>
                  </dd>
                </>
              )}
              <dt>{move ? 'Bundle’s root' : 'Fleet root'}</dt>
              <dd>
                <code>{confirm.to}</code>
              </dd>
              <dt>This instance as</dt>
              <dd>
                <code>{confirm.principal}</code>
              </dd>
            </dl>
            {confirm.from && (
              <p className="fleet-note">Check the fleet root against the fingerprint the fleet operator gave you.</p>
            )}
            <div className="fleet-confirm-actions">
              <button type="button" className="k-btn" onClick={() => setConfirm(null)} disabled={busy}>
                {move ? 'Keep the current fleet' : 'Cancel'}
              </button>
              <button
                type="button"
                className={move ? 'k-btn is-danger' : 'k-btn'}
                onClick={() => install(confirm.from, confirm.to)}
                disabled={busy}
              >
                {move ? 'Replace fleet root' : same ? 'Install' : 'Join this fleet'}
              </button>
            </div>
          </div>
        )
      })()}
    </section>
  )
}
