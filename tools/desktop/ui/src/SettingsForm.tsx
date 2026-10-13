import { useEffect, useState } from 'react'
import { FleetIdentitySection } from './FleetIdentity.tsx'
import { installNeedsRestart, type FleetIdentity, type InstallResult } from './fleet.ts'

// Mirrors the Settings struct in tools/desktop/settings.go field for field.
// The json tags there are the wire names; these must match them exactly.
export interface Settings {
  port: string
  logLevel: string
  logFormat: string
  startAtLogin: boolean
  /** [tls].addr: the fleet listener, "" = off. Applied by a reload. */
  tlsAddr: string
  /**
   * Who may reach the plaintext HTTP port. 'other' is a knomit.toml that binds
   * some other address: Settings leaves it alone and a save of 'other' writes
   * nothing.
   */
  httpScope: HttpScope
  /** The running server's own account of itself (server.json + supervisor). */
  server: ServerState
  effectivePort: number
  configPath: string
  logFilePath: string
  overriddenByEnv: string[]
}

export type HttpScope = 'off' | 'local' | 'all' | 'other'

/** Mirrors ServerState in tools/desktop/native.go field for field. */
export interface ServerState {
  running: boolean
  pid: number
  /** Started outside the app: it is neither stopped nor restarted by it. */
  attached: boolean
  canRestart: boolean
  canReload: boolean
  version: string
  desktopVersion: string
  /** Attached server and app differ in major.minor. */
  versionSkew: boolean
  /** The plaintext listener's base URL, "" when it is off. */
  httpUrl: string
  httpReason: string
  /** The fleet listener's bound address, "" when it is not listening. */
  tlsListening: string
  tlsReason: string
  generation: number
  /** Keys a reload saw change and could not apply: they wait for a restart. */
  restartNeeded: string[]
  reloadError: string
}

/** Mirrors SaveAck in tools/desktop/native.go: what became of the running server. */
export interface SaveAck {
  outcome:
    | 'applied'
    | 'restart_needed'
    | 'listener_failed'
    | 'refused'
    | 'timeout'
    | 'not_running'
    | 'unavailable'
  generation: number
  httpUrl: string
  httpReason: string
  tlsListening: string
  tlsReason: string
  restartNeeded: string[]
  error: string
}

/**
 * Shown next to "All networks", verbatim from the design (Q5): the policy is
 * that a plaintext peer that is not on this computer reads but never writes.
 */
export const ALL_NETWORKS_WARNING =
  'Anyone who can reach this computer can READ every knowledge base and clone it, without signing in. ' +
  'They cannot change anything. To let other machines work with this one securely, enrol it in a fleet ' +
  'and use the Fleet port.'

/** What the Fleet identity section needs; omitted, the section is not shown. */
export interface FleetProps {
  identity: FleetIdentity | null
  onCopyPublicKey: () => Promise<void>
  onReadClipboard: () => Promise<string>
  onInstall: (raw: string, confirmFrom: string, confirmTo: string) => Promise<InstallResult>
}

interface Props {
  initial: Settings
  onSave: (s: Settings) => Promise<SaveAck>
  // Resolves when the replacement server answers; rejects with the reason it
  // did not — see restart() below.
  onRestart: () => Promise<void>
  onRevealLog: () => Promise<void>
  /** Discards edits and closes the window, the way a dialog's Cancel does. */
  onCancel: () => void
  /**
   * Reads the settings again, for the server's state after a save or a
   * restart. Only `server` and `effectivePort` are taken from it: the form's
   * own edits are never overwritten.
   */
  onRefresh?: () => Promise<Settings>
  fleet?: FleetProps
  /** How long a restart may take before the window gives up waiting. */
  restartTimeoutMs?: number
  /** Gap between the reads that wait for the new server's record. */
  pollMs?: number
}

/**
 * Which env var overrides each field, mirroring envKeys in settings.go.
 *
 * There is no entry for startAtLogin and there must not be one: it is an OS
 * login-item registration, not a knomit.toml key, so no environment variable
 * can shadow it.
 */
const ENV_FOR: Record<string, string | undefined> = {
  port: 'KNOMIT_PORT',
  logLevel: 'KNOMIT_LOG_LEVEL',
  logFormat: 'KNOMIT_LOG_FORMAT',
  tlsAddr: 'KNOMIT_TLS_ADDR',
}

/**
 * The two variables that own the HTTP scope (settings.go configKeys, scope:
 * true): either one set makes the whole choice read-only, because half a scope
 * written under the other's override would be a surprise.
 */
const SCOPE_ENV = ['KNOMIT_HTTP', 'KNOMIT_HOST']

const SCOPES: { value: Exclude<HttpScope, 'other'>; label: string }[] = [
  { value: 'off', label: 'Off' },
  { value: 'local', label: 'This computer only' },
  { value: 'all', label: 'All networks' },
]

/** The zerolog levels config.Validate accepts, quietest first. */
const LEVELS = ['trace', 'debug', 'info', 'warn', 'error']

// The label is the VALUE; the gloss is the hint beside the control. Wire
// values are unchanged — settings.go validates on 'console' / 'json'.
const FORMATS = [
  { value: 'console', label: 'console', hint: 'Human-readable' },
  { value: 'json', label: 'json', hint: 'Structured' },
]

/**
 * Rejects exactly what validateSettings (settings.go) rejects, before the round
 * trip. Go is still the authority — this is not a substitute for it — but
 * catching it here means knomit.toml is never even opened for a value that
 * would be refused, and the user sees a sentence rather than a wrapped Go error.
 *
 * Keyed BY FIELD so the message can be attached to the control that caused it.
 * A single banner made the reader match a sentence against four inputs; a mark
 * on the offending field does not. Returns {} when the settings are acceptable.
 */
function validate(s: Settings): Record<string, string> {
  const errs: Record<string, string> = {}
  // Not trimmed, and digits only: strconv.Atoi does not trim either, so " 20000"
  // is a value Go would reject and the form must not send.
  if (!/^\d+$/.test(s.port)) {
    errs.port = `Port must be a whole number, got "${s.port}".`
  } else {
    const port = Number(s.port)
    // Below 1024 needs root on Unix; knomit runs as the logged-in user.
    if (port < 1024 || port > 65535) {
      errs.port = `Port must be between 1024 and 65535, got ${port}.`
    }
  }
  // Neither of these can be produced by a <select> over the lists below, but
  // the checks cost nothing and pin the coupling to validateSettings.
  if (!LEVELS.includes(s.logLevel)) {
    errs.logLevel = `Log level must be one of ${LEVELS.join(', ')}, got "${s.logLevel}".`
  }
  if (!FORMATS.some((f) => f.value === s.logFormat)) {
    errs.logFormat = `Log format must be console or json, got "${s.logFormat}".`
  }
  const tls = tlsAddrError(s.tlsAddr, s.port)
  if (tls) errs.tlsAddr = tls
  return errs
}

/**
 * validateTLSAddr (settings.go), before the round trip: host:port with the
 * host optional and IPv6 bracketed, the port rules Port has, and not Port
 * itself. "" is "off". Go stays the authority.
 */
function tlsAddrError(addr: string, plainPort: string): string {
  if (addr === '') return ''
  const m = /^(\[[^\]\s/]*\]|[^:\s/[\]]*):(\d+)$/.exec(addr)
  if (!m) return `The fleet listener address must be host:port (e.g. 0.0.0.0:19279), got "${addr}".`
  const port = Number(m[2])
  if (port < 1024 || port > 65535) return `The fleet listener port must be between 1024 and 65535, got ${port}.`
  if (String(port) === plainPort) return `The fleet listener cannot share port ${port} with the local server.`
  return ''
}

/** Wails rejects with an Error; unwrap it so the user sees the message alone. */
function message(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}

export function SettingsForm({
  initial,
  onSave,
  onRestart,
  onRevealLog,
  onCancel,
  onRefresh,
  fleet,
  restartTimeoutMs = 90_000,
  pollMs = 500,
}: Props) {
  const [s, setS] = useState(initial)
  // Shown beside the buttons. Reserved for failures that belong to the WINDOW
  // rather than to one control: a save the backend refused, a restart that
  // failed or timed out. Field problems never come here — they mark the field
  // instead.
  const [error, setError] = useState('')
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({})
  const [saved, setSaved] = useState('')
  const [restarting, setRestarting] = useState(false)
  // The server as the window last saw it: from the window's load, then from
  // every refresh after a save or a restart.
  const [server, setServer] = useState<ServerState>(initial.server)
  const [effectivePort, setEffectivePort] = useState(initial.effectivePort)
  // The port as of the last save that reached the file, so "configured port
  // was busy" compares what is ON DISK with what the server bound, not what is
  // half-typed in the form.
  const [savedPort, setSavedPort] = useState(initial.port)
  // What the last save's reload reported. Its restartNeeded is fresher than
  // the server record until the next refresh, and a restart clears it.
  const [ack, setAck] = useState<SaveAck | null>(null)
  // The section's identity: the one loaded with the window, then whatever an
  // install hands back, so the rows show what was just installed.
  const [identity, setIdentity] = useState<FleetIdentity | null>(fleet?.identity ?? null)
  useEffect(() => setIdentity(fleet?.identity ?? null), [fleet?.identity])
  // An install whose listener only a restart can start (see installNeedsRestart).
  const [installRestart, setInstallRestart] = useState(false)

  // "Saved." is a notification, not a state: it reports that something just
  // happened, so it has to expire. Left standing it becomes a claim about the
  // present — a form edited after a save still reading "Saved." is telling the
  // user their unsaved changes are on disk.
  useEffect(() => {
    if (!saved) return
    const t = setTimeout(() => setSaved(''), 2600)
    return () => clearTimeout(t)
  }, [saved])
  // The port, the scope and [tls].addr are applied by the reload a save
  // triggers. What waits for a restart is what the SERVER says it could not
  // apply (auth.require, ...), plus a first enrolment whose listener is wanted
  // but not running.
  const restartKeys = ack ? ack.restartNeeded : server.restartNeeded
  const needsRestart = restartKeys.length > 0 || installRestart

  // envOverrides returns a NIL slice when nothing is set, and encoding/json
  // renders that as `null` rather than `[]`. That is the ordinary case, so
  // defaulting here is what keeps the window from coming up blank.
  const overriddenBy = initial.overriddenByEnv ?? []
  // The server bound a different port than the one on disk (the configured one
  // was held), unless the scope says there should be no port at all.
  const portBusy = effectivePort > 0 && String(effectivePort) !== savedPort && s.httpScope !== 'off'
  const scopeEnv = SCOPE_ENV.filter((k) => overriddenBy.includes(k))
  const overridden = (field: string) => {
    const key = ENV_FOR[field]
    return key !== undefined && overriddenBy.includes(key)
  }

  function edit(patch: Partial<Settings>) {
    const next = { ...s, ...patch }
    setS(next)
    setSaved('')
    // Clear a mark the moment the value stops being wrong, rather than making
    // the user press Save again to find out. Only re-validates fields that are
    // already marked, so typing into a clean form never raises an error
    // mid-keystroke — "1" on the way to "19278" is not a mistake to report.
    if (Object.keys(fieldErrors).length > 0) {
      const still = validate(next)
      const kept: Record<string, string> = {}
      for (const k of Object.keys(fieldErrors)) if (still[k]) kept[k] = still[k]
      setFieldErrors(kept)
    }
  }

  /** The server's record again, for the header, the restart notice and the versions. */
  async function refresh(): Promise<Settings | null> {
    if (!onRefresh) return null
    try {
      const fresh = await onRefresh()
      setServer(fresh.server)
      setEffectivePort(fresh.effectivePort)
      return fresh
    } catch {
      // The header keeps what it had; the next save or restart reads again.
      return null
    }
  }

  async function save() {
    setError('')
    setSaved('')
    const invalid = validate(s)
    setFieldErrors(invalid)
    if (Object.keys(invalid).length > 0) {
      // The messages render inline under their fields. Nothing to open, and
      // nothing to choose between: every refusal is on screen at once.
      return
    }
    let result: SaveAck
    try {
      result = await onSave(s)
    } catch (e) {
      // applySettings validates before touching either backend, so a rejected
      // save changed nothing on disk and the server was not told.
      setError(message(e))
      return
    }
    // The file is saved whatever the ack says; the ack says what became of the
    // RUNNING server. A failure to apply is shown as the failure it is, not
    // behind a cheerful "Saved.".
    setAck(result)
    setSavedPort(s.port)
    const line = ackLine(result, s)
    if (line.ok) setSaved(line.text)
    else setError(line.text)
    void refresh()
  }

  /**
   * Restarts the server the app spawned and commits to a terminal state: the
   * button leaves "Restarting…" on success, on failure and on timeout, never
   * on a promise that might not settle.
   *
   * RestartServer resolves when the replacement answers on its socket, and
   * rejects with the reason it did not (a server started outside the app is
   * refused by name). The window does not trust it to settle: it gives up after
   * restartTimeoutMs with a message, because a button stuck on "Restarting…"
   * gives the user nothing to act on (it was exactly that, in the first build:
   * a promise that never settled because the whole app quit).
   *
   * After a success the new server's record is read until it names a new pid
   * (it writes server.json just after it starts answering), so the header and
   * the restart notice describe the server that is running now.
   */
  async function restart() {
    setError('')
    setSaved('')
    setRestarting(true)
    const oldPid = server.pid
    let timer: ReturnType<typeof setTimeout> | undefined
    const timeout = new Promise<never>((_, reject) => {
      timer = setTimeout(
        () =>
          reject(
            new Error(
              `The server did not come back within ${Math.round(restartTimeoutMs / 1000)} s. ` +
                'Its log file says why; quit and reopen Knomit if it stays down.',
            ),
          ),
        restartTimeoutMs,
      )
    })
    try {
      await Promise.race([onRestart(), timeout])
    } catch (e) {
      clearTimeout(timer)
      setRestarting(false)
      setError(message(e))
      void refresh()
      return
    }
    clearTimeout(timer)
    for (let i = 0; onRefresh && i < 20; i++) {
      const fresh = await refresh()
      if (fresh && fresh.server.running && fresh.server.pid !== oldPid) break
      await new Promise((r) => setTimeout(r, pollMs))
    }
    setAck(null)
    setInstallRestart(false)
    setRestarting(false)
    setSaved('Server restarted.')
  }

  function reveal() {
    onRevealLog().catch((e: unknown) => setError(message(e)))
  }

  // Read-only, not disabled. A disabled control greys its value out and reads
  // as "unavailable"; this value is perfectly valid and worth being able to
  // read and copy — it is simply owned elsewhere. readOnly refuses the edit
  // while keeping the text legible and selectable.
  //
  // On a <select> readOnly does nothing, so those still take `disabled` — the
  // chip beside them is what carries the meaning.
  const errFor = (field: string) =>
    fieldErrors[field] && (
      // Inline, not a bubble behind a button. The bubble existed because a
      // rejected field could not afford vertical space in a window that could
      // not be resized — S1's pinned footer removes that constraint, so the
      // reason for a refusal no longer costs an interaction to read.
      <p className="sub err" role="alert">
        {fieldErrors[field]}
      </p>
    )

  // Amber, not red: nothing has failed.. The value on screen is simply not the
  // one in the file, and a save will not change it — the same "this is not what
  // you think" the knowledge app paints amber.
  const envNote = (field: string) =>
    overridden(field) && (
      <p className="sub env">
        Set by <code>{ENV_FOR[field]}</code>, which beats knomit.toml — saving
        cannot change this.
      </p>
    )

  // A path is shown home-collapsed so it fits without widening the window; the
  // full value stays on `title`, so nothing is actually lost.
  //
  // Windows gets its own rule rather than the `~` one. Now that the data root
  // lives under %LOCALAPPDATA%\knomit\home, every path shown here carries ~30
  // characters of profile boilerplate — and `~` would be actively misleading,
  // because the root is no longer inside the user's home directory. The
  // variable name is what a Windows user would type to get back there.
  const short = (path: string) =>
    path
      .replace(/^[A-Za-z]:\\Users\\[^\\]+\\AppData\\Local/, '%LOCALAPPDATA%')
      .replace(/^\/(Users|home)\/[^/]+/, '~')

  return (
    // Three regions, not one scrolling column. The window is DisableResize, so
    // a form that grows — an env note under all three fields, a restart offer —
    // used to push Save past the bottom edge with no way to reach it. Only the
    // BODY scrolls now; the status header and the footer are pinned, so the
    // buttons are always where the user left them.
    <div className="ps">
      <header className="ps-status">
        {server.running || effectivePort > 0 ? (
          <>
            <span
              className={
                portBusy ? 'pip is-warn' : 'pip is-ok'
              }
              aria-hidden="true"
            />
            <span className="ps-state">Running</span>
            <code className="ps-addr">{listeningAddress(server, effectivePort)}</code>
            {portBusy && (
              // Replaces the old sentence under the port field. The number that
              // matters is the one it is actually listening on, and this is
              // where someone looks to find it.
              <span className="ps-aside">— configured port was busy</span>
            )}
          </>
        ) : (
          <>
            {/* Still booting. Do not invent a bound address in this state. */}
            <span className="pip" aria-hidden="true" />
            <span className="ps-state is-muted">Starting…</span>
          </>
        )}
      </header>

      <div className="ps-body">
        {server.attached && (
          <div className="banner" role="note">
            <p>
              The server was started outside the app{server.pid > 0 ? ` (pid ${server.pid})` : ''}; restart is
              not available, changes apply on reload.
            </p>
            {server.version && (
              <p className="sub">
                Server <code>{server.version}</code> · App <code>{server.desktopVersion}</code>
              </p>
            )}
            {server.versionSkew && (
              <p className="sub warn" role="note">
                The server and the app are different versions ({majorMinor(server.version)} and{' '}
                {majorMinor(server.desktopVersion)}): this window may not match the server&apos;s API.
              </p>
            )}
          </div>
        )}

        {/* One grouped list, not four icon-headed sections. Five controls did
            not need four eyebrows and four hand-drawn glyphs — the scaffolding
            outweighed the content and the eye counted headings instead of
            settings. Card and hairlines is also the vocabulary the Manage pane
            already uses. */}
        <div className="list">
          <div className="row scope">
            <div className="rowlabel" id="httpScopeLabel">
              HTTP port
            </div>
            <div className="control scopes" role="radiogroup" aria-labelledby="httpScopeLabel">
              {SCOPES.map((o) => (
                <label key={o.value} className="radio">
                  <input
                    type="radio"
                    name="httpScope"
                    value={o.value}
                    checked={s.httpScope === o.value}
                    disabled={scopeEnv.length > 0}
                    onChange={() => edit({ httpScope: o.value })}
                  />
                  {o.label}
                </label>
              ))}
              {scopeEnv.length > 0 && <span className="chip">env</span>}
            </div>
            {s.httpScope === 'off' && (
              <p className="sub">
                No TCP port is open. This app and the kb tool use the local socket; browsers and MCP
                clients that connect to http://127.0.0.1 cannot.
              </p>
            )}
            {s.httpScope === 'local' && (
              <p className="sub">
                Only programs on this computer can connect, on <code>http://127.0.0.1:{s.port}</code>.
              </p>
            )}
            {s.httpScope === 'all' && (
              <p className="sub warn" role="note">
                {ALL_NETWORKS_WARNING}
              </p>
            )}
            {s.httpScope === 'other' && (
              <p className="sub">
                knomit.toml binds a specific address. Settings leaves it alone; edit the file to change it.
              </p>
            )}
            {scopeEnv.length > 0 && (
              <p className="sub env">
                Set by <code>{scopeEnv.join(' and ')}</code>, which beats knomit.toml — saving cannot change this.
              </p>
            )}
          </div>

          <div className="row">
            <label htmlFor="port">Port</label>
            <div className="control">
              <input
                id="port"
                className="k-input port"
                value={s.port}
                readOnly={overridden('port')}
                aria-invalid={fieldErrors.port ? true : undefined}
                onChange={(e) => edit({ port: e.target.value })}
              />
              {overridden('port') ? (
                <span className="chip">env</span>
              ) : (
                <span className="hint">
                  {s.httpScope === 'off' ? 'used when the HTTP port is on' : '1024–65535'}
                </span>
              )}
            </div>
            {errFor('port')}
            {envNote('port')}
          </div>

          <div className="row">
            <label htmlFor="tlsAddr">Fleet listener</label>
            <div className="control">
              <input
                id="tlsAddr"
                className="k-input addr"
                value={s.tlsAddr}
                placeholder="off"
                spellCheck={false}
                readOnly={overridden('tlsAddr')}
                aria-invalid={fieldErrors.tlsAddr ? true : undefined}
                onChange={(e) => edit({ tlsAddr: e.target.value })}
              />
              {overridden('tlsAddr') ? (
                <span className="chip">env</span>
              ) : (
                <span className="hint">Fleet port, host:port for enrolled peers</span>
              )}
            </div>
            {errFor('tlsAddr')}
            {envNote('tlsAddr')}
          </div>

          <div className="row">
            <label htmlFor="logLevel">Log level</label>
            <div className="control">
              <select
                id="logLevel"
                className="k-select"
                value={s.logLevel}
                disabled={overridden('logLevel')}
                aria-invalid={fieldErrors.logLevel ? true : undefined}
                onChange={(e) => edit({ logLevel: e.target.value })}
              >
                {LEVELS.map((l) => (
                  <option key={l} value={l}>
                    {l}
                  </option>
                ))}
              </select>
              {overridden('logLevel') && <span className="chip">env</span>}
            </div>
            {errFor('logLevel')}
            {envNote('logLevel')}
          </div>

          <div className="row">
            <label htmlFor="logFormat">Log format</label>
            <div className="control">
              <select
                id="logFormat"
                className="k-select"
                value={s.logFormat}
                disabled={overridden('logFormat')}
                aria-invalid={fieldErrors.logFormat ? true : undefined}
                onChange={(e) => edit({ logFormat: e.target.value })}
              >
                {FORMATS.map((f) => (
                  <option key={f.value} value={f.value}>
                    {f.label}
                  </option>
                ))}
              </select>
              {overridden('logFormat') ? (
                <span className="chip">env</span>
              ) : (
                <span className="hint">
                  {FORMATS.find((f) => f.value === s.logFormat)?.hint}
                </span>
              )}
            </div>
            {errFor('logFormat')}
            {envNote('logFormat')}
          </div>

          <div className="row">
            <label htmlFor="startAtLogin">Start at login</label>
            <div className="control">
              {/* A real checkbox with a switch drawn on it, not a div with a
                  click handler: keyboard, form semantics and the accessibility
                  tree all come free that way. */}
              <input
                id="startAtLogin"
                className="sw"
                type="checkbox"
                checked={s.startAtLogin}
                onChange={(e) => edit({ startAtLogin: e.target.checked })}
              />
            </div>
          </div>
        </div>

        {fleet && (
          <FleetIdentitySection
            identity={identity}
            onCopyPublicKey={fleet.onCopyPublicKey}
            onReadClipboard={fleet.onReadClipboard}
            onInstall={fleet.onInstall}
            onInstalled={(next) => {
              if (next) setIdentity(next)
              if (installNeedsRestart(next)) setInstallRestart(true)
            }}
          />
        )}

        {/* A footnote, not a peer of Port: dim, mono, home-collapsed. */}
        <dl className="ps-paths">
          <dt>Config</dt>
          <dd title={initial.configPath}>{short(initial.configPath)}</dd>
          <dt>Log file</dt>
          {initial.logFilePath ? (
            <dd title={initial.logFilePath}>
              <span className="ps-path">{short(initial.logFilePath)}</span>
              <button type="button" className="linkbtn" onClick={reveal}>
                Reveal
              </button>
            </dd>
          ) : (
            // resolveLogFile (logging.go) returns "" only when there is no
            // `[log] file` AND no resolvable logs directory — nothing is being
            // written to disk at all. Reveal is DROPPED rather than disabled:
            // there is no directory to open, and RevealLogFile would run
            // `open -R ""`, which fails with nothing on screen to explain it.
            // Saying why beats a button that quietly does nothing.
            <dd className="ps-nofile">
              none — set <code>[log] file</code> in knomit.toml
            </dd>
          )}
        </dl>
      </div>

      <footer className="ps-foot">
        {/* Restart is an outcome of Save, so it belongs beside Save. It used to
            render mid-form, where accepting a port change shoved the buttons
            down at the moment the user was reaching for them. */}
        {needsRestart && (
          <div className="restartbar">
            <p>
              {restartReason(restartKeys, installRestart)}
              {server.attached &&
                ` The server was started outside the app${server.pid > 0 ? ` (pid ${server.pid})` : ''}: restart it where it was started.`}
            </p>
            <button
              type="button"
              className="k-btn"
              onClick={restart}
              disabled={restarting || !server.canRestart}
              title={server.canRestart ? undefined : 'Only a server the app started can be restarted here.'}
            >
              {restarting ? 'Restarting…' : 'Restart server'}
            </button>
          </div>
        )}

        <div className="actions">
          {error ? (
            <p role="alert" className="outcome is-error" title={error}>
              {error}
            </p>
          ) : saved ? (
            <p role="status" className="outcome is-ok" title={saved}>
              {saved}
            </p>
          ) : (
            <span className="outcome" aria-hidden="true" />
          )}
          <button type="button" className="k-btn" onClick={onCancel}>
            Cancel
          </button>
          <button type="button" className="k-btn is-primary" onClick={save}>
            Save
          </button>
        </div>
      </footer>
    </div>
  )
}

/**
 * One restart offer whatever owes it — not one bar per cause — saying what
 * the restart will apply.
 */
function restartReason(keys: string[], install: boolean): string {
  const parts: string[] = []
  if (keys.length > 0) {
    parts.push(`These settings take effect after a restart of the server: ${keys.join(', ')}.`)
  }
  if (install) {
    parts.push(
      'The fleet listener starts after a restart: installing a bundle does not start it in a running Knomit.',
    )
  }
  return parts.join(' ')
}

/** What the header calls the running server's address. */
function listeningAddress(server: ServerState, effectivePort: number): string {
  if (server.httpUrl) return server.httpUrl.replace(/^https?:\/\//, '')
  if (effectivePort > 0) return `127.0.0.1:${effectivePort}`
  return 'local socket only'
}

/** "0.5" from "0.5.3.6ce780d": the part a skew warning is about. */
function majorMinor(v: string): string {
  return v.replace(/^v/, '').split('.').slice(0, 2).join('.') || v
}

/**
 * What a save's ack tells the user. ok lines are confirmations; the others
 * are a save that reached the file but not (all of) the running server.
 */
function ackLine(a: SaveAck, s: Settings): { ok: boolean; text: string } {
  switch (a.outcome) {
    case 'applied': {
      const http = a.httpUrl
        ? ` HTTP port on ${a.httpUrl.replace(/^https?:\/\//, '')}.`
        : s.httpScope === 'off'
          ? ' HTTP port is off.'
          : ''
      const fleet = a.tlsListening ? ` Fleet port on ${a.tlsListening}.` : ''
      return { ok: true, text: `Saved and applied.${http}${fleet}` }
    }
    case 'restart_needed':
      return { ok: true, text: 'Saved. Listener changes are applied; the rest waits for a restart.' }
    case 'not_running':
      return { ok: true, text: 'Saved. The server is not running; the changes apply when it starts.' }
    case 'listener_failed':
      return { ok: false, text: `Saved, but a port could not be opened: ${a.error}` }
    case 'refused':
      return { ok: false, text: `Saved, but the server could not load it and kept its running settings: ${a.error}` }
    case 'timeout':
      return { ok: false, text: `Saved, but the server did not confirm: ${a.error}` }
    default:
      return { ok: false, text: `Saved, but the server could not be told: ${a.error}` }
  }
}
