import { useEffect, useState, type ComponentType } from 'react'
import { FleetIdentitySection } from './FleetIdentity.tsx'
import type { FleetIdentity } from './fleet.ts'
import { BroadcastIcon, FolderIcon, GearIcon, GlobeIcon } from './icons.tsx'
import { Row } from './Row.tsx'

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

/**
 * The sidebar's pages, in order. Every page edits the same Settings and one
 * Save sends them all: the pages divide the window, not the transaction.
 */
type Page = 'general' | 'network' | 'fleet' | 'logs'

const PAGES: { id: Page; label: string; Icon: ComponentType<{ color: string; size?: number }> }[] = [
  { id: 'general', label: 'General', Icon: GearIcon },
  { id: 'network', label: 'Network', Icon: GlobeIcon },
  { id: 'fleet', label: 'Fleet', Icon: BroadcastIcon },
  { id: 'logs', label: 'Logs', Icon: FolderIcon },
]

/**
 * The page each validated field lives on. A refused field on a page that is
 * not in view would otherwise be an error nobody can see: Save switches to its
 * page and the sidebar marks it (see save()).
 */
const PAGE_OF: Record<string, Page> = {
  port: 'network',
  tlsAddr: 'network',
  logLevel: 'logs',
  logFormat: 'logs',
}

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
  const [page, setPage] = useState<Page>('general')
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
  const identity = fleet?.identity ?? null

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
  // apply (auth.require, ...).
  const restartKeys = ack ? ack.restartNeeded : server.restartNeeded
  const needsRestart = restartKeys.length > 0

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
      // The messages render inline under their fields, and the sidebar marks
      // every page that holds one. If the page in view holds none, go to the
      // first that does: a refusal on a page nobody is looking at would read
      // as a Save that did nothing.
      const held = pagesWithErrors(invalid)
      if (!held.includes(page)) setPage(held[0])
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
  // On a <select> or a radio readOnly does nothing, so those still take
  // `disabled` — the chip beside them is what carries the meaning.
  const errFor = (field: string) =>
    fieldErrors[field] && (
      // Inline, under the row, not a bubble behind a button: the footer is
      // pinned, so the reason for a refusal never costs an interaction to read.
      <p className="rnote err" role="alert">
        {fieldErrors[field]}
      </p>
    )

  // Amber, not red: nothing has failed. The value on screen is simply not the
  // one in the file, and a save will not change it — the same "this is not what
  // you think" the knowledge app paints amber.
  const envNote = (field: string) =>
    overridden(field) && (
      <p className="rnote env">
        Set by <code>{ENV_FOR[field]}</code>, which beats knomit.toml — saving
        cannot change this.
      </p>
    )
  const envChip = (on: boolean) => on && <span className="chip">env</span>

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

  // A missing section is omitted, never shown disabled: without the fleet
  // props there is no Fleet page to go to.
  const pages = PAGES.filter((p) => p.id !== 'fleet' || fleet)
  const errorPages = pagesWithErrors(fieldErrors)
  const titleOf = (id: Page) => PAGES.find((p) => p.id === id)!.label

  return (
    // Sidebar and pane. The window is DisableResize, so only the PAGE scrolls:
    // the sidebar (with the server status at its foot) and the footer (restart
    // offer, outcome, Cancel and Save) are pinned, so the buttons are always
    // where the user left them, whichever page is in view.
    <div className="ps">
      <aside className="side">
        {/* Icon-headed sections, reversing the earlier "one grouped list"
            ruling: with four pages to move between, a glyph per page is a
            landmark rather than scaffolding. The glyphs are the knowledge
            app's own (icons.tsx copies them), and the selection is a neutral
            pill, not an accent: no hue here claims anything about the data. */}
        <nav className="nav" aria-label="Settings sections">
          {pages.map(({ id, label, Icon }) => {
            const bad = errorPages.includes(id)
            return (
              <button
                key={id}
                type="button"
                aria-current={page === id ? 'page' : undefined}
                onClick={() => setPage(id)}
              >
                <span className="nav-ic" aria-hidden="true">
                  <Icon color="currentColor" size={15} />
                </span>
                <span>{label}</span>
                {bad && (
                  // Red, because red already means error in this window. The
                  // page's own rows say which field and why. The space keeps
                  // the button's name "Network has an error", not one word.
                  <>
                    {' '}
                    <span className="nav-err" role="img" aria-label="has an error" title="A value on this page was refused" />
                  </>
                )}
              </button>
            )
          })}
        </nav>

        <div className="sstat">
          {server.running || effectivePort > 0 ? (
            <>
              <span className="l1">
                <span className={portBusy ? 'pip is-warn' : 'pip is-ok'} aria-hidden="true" />
                <span className="ps-state">Running</span>
                {server.attached && <span className="att">· attached</span>}
              </span>
              <code className="addr">{listeningAddress(server, effectivePort)}</code>
              {portBusy && (
                // The number that matters is the one it is actually listening
                // on, and this is where someone looks to find it.
                <span className="aside">— configured port was busy</span>
              )}
            </>
          ) : (
            <span className="l1">
              {/* Still booting. Do not invent a bound address in this state. */}
              <span className="pip" aria-hidden="true" />
              <span className="ps-state is-muted">Starting…</span>
            </span>
          )}
        </div>
      </aside>

      <main className="main">
        <div className="content">
          <section className="page" hidden={page !== 'general'} aria-labelledby="page-general">
            <h2 className="ptitle" id="page-general">
              {titleOf('general')}
            </h2>
            {server.attached && (
              <div className="banner" role="note">
                <p>
                  The server was started outside the app{server.pid > 0 ? ` (pid ${server.pid})` : ''}; restart is
                  not available, changes apply on reload.
                </p>
                {server.version && (
                  <p>
                    Server <code>{server.version}</code> · App <code>{server.desktopVersion}</code>
                  </p>
                )}
                {server.versionSkew && (
                  <p className="warn" role="note">
                    The server and the app are different versions ({majorMinor(server.version)} and{' '}
                    {majorMinor(server.desktopVersion)}): this window may not match the server&apos;s API.
                  </p>
                )}
              </div>
            )}

            <h3 className="gh">Startup</h3>
            <div className="card">
              <Row
                label="Start at login"
                htmlFor="startAtLogin"
                control={
                  // A real checkbox with a switch drawn on it, not a div with a
                  // click handler: keyboard, form semantics and the
                  // accessibility tree all come free that way.
                  <input
                    id="startAtLogin"
                    className="sw"
                    type="checkbox"
                    checked={s.startAtLogin}
                    onChange={(e) => edit({ startAtLogin: e.target.checked })}
                  />
                }
              />
            </div>

            <h3 className="gh">Files</h3>
            <div className="card">
              <Row label="Config" sub={short(initial.configPath)} subTitle={initial.configPath} />
            </div>
          </section>

          <section className="page" hidden={page !== 'network'} aria-labelledby="page-network">
            <h2 className="ptitle" id="page-network">
              {titleOf('network')}
            </h2>
            <h3 className="gh">HTTP</h3>
            <div className="card">
              <Row
                label="HTTP port"
                labelId="httpScopeLabel"
                control={
                  <>
                    {envChip(scopeEnv.length > 0)}
                    {/* Native radios drawn as a segmented control: arrow keys,
                        form semantics and the accessibility tree come free. */}
                    <div
                      className={scopeEnv.length > 0 ? 'seg is-ro' : 'seg'}
                      role="radiogroup"
                      aria-labelledby="httpScopeLabel"
                    >
                      {SCOPES.map((o) => (
                        // The selected look is a class from state, not
                        // :has(input:checked): macOS 11's WebKit (before
                        // Safari 15.4) has no :has(). Focus uses the sibling
                        // selector `input:focus-visible + span` for the same
                        // reason.
                        <label key={o.value} className={s.httpScope === o.value ? 'seg-opt is-on' : 'seg-opt'}>
                          <input
                            type="radio"
                            name="httpScope"
                            value={o.value}
                            checked={s.httpScope === o.value}
                            disabled={scopeEnv.length > 0}
                            onChange={() => edit({ httpScope: o.value })}
                          />
                          <span>{o.label}</span>
                        </label>
                      ))}
                    </div>
                  </>
                }
              >
                {s.httpScope === 'off' && (
                  <p className="rnote">
                    No TCP port is open. This app and the kb tool use the local socket; browsers and MCP
                    clients that connect to http://127.0.0.1 cannot.
                  </p>
                )}
                {s.httpScope === 'local' && (
                  <p className="rnote">
                    Only programs on this computer can connect, on <code>http://127.0.0.1:{s.port}</code>.
                  </p>
                )}
                {s.httpScope === 'all' && (
                  <p className="rnote warn" role="note">
                    {ALL_NETWORKS_WARNING}
                  </p>
                )}
                {s.httpScope === 'other' && (
                  <p className="rnote">
                    knomit.toml binds a specific address. Settings leaves it alone; edit the file to change it.
                  </p>
                )}
                {scopeEnv.length > 0 && (
                  <p className="rnote env">
                    Set by <code>{scopeEnv.join(' and ')}</code>, which beats knomit.toml — saving cannot change
                    this.
                  </p>
                )}
              </Row>

              <Row
                label="Port"
                htmlFor="port"
                sub={overridden('port') ? undefined : s.httpScope === 'off' ? 'used when the HTTP port is on' : '1024–65535'}
                control={
                  <>
                    {envChip(overridden('port'))}
                    <input
                      id="port"
                      className="k-input port"
                      value={s.port}
                      readOnly={overridden('port')}
                      aria-invalid={fieldErrors.port ? true : undefined}
                      onChange={(e) => edit({ port: e.target.value })}
                    />
                  </>
                }
              >
                {errFor('port')}
                {envNote('port')}
              </Row>
            </div>

            <h3 className="gh">Fleet</h3>
            <div className="card">
              <Row
                label="Fleet listener"
                htmlFor="tlsAddr"
                sub="Fleet port, host:port for enrolled peers"
                control={
                  <>
                    {envChip(overridden('tlsAddr'))}
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
                  </>
                }
              >
                {errFor('tlsAddr')}
                {envNote('tlsAddr')}
              </Row>
            </div>
          </section>

          {fleet && (
            // Kept mounted while hidden, like every page: a bundle loaded or a
            // root confirmation open on this page survives a look at Network.
            <section className="page" hidden={page !== 'fleet'} aria-labelledby="page-fleet">
              <h2 className="ptitle" id="page-fleet">
                {titleOf('fleet')}
              </h2>
              <FleetIdentitySection
                identity={identity}
                onCopyPublicKey={fleet.onCopyPublicKey}
              />
            </section>
          )}

          <section className="page" hidden={page !== 'logs'} aria-labelledby="page-logs">
            <h2 className="ptitle" id="page-logs">
              {titleOf('logs')}
            </h2>
            <h3 className="gh">Output</h3>
            <div className="card">
              <Row
                label="Log level"
                htmlFor="logLevel"
                control={
                  <>
                    {envChip(overridden('logLevel'))}
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
                  </>
                }
              >
                {errFor('logLevel')}
                {envNote('logLevel')}
              </Row>

              <Row
                label="Log format"
                htmlFor="logFormat"
                // The label is the VALUE; the gloss says what it means.
                sub={FORMATS.find((f) => f.value === s.logFormat)?.hint}
                control={
                  <>
                    {envChip(overridden('logFormat'))}
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
                  </>
                }
              >
                {errFor('logFormat')}
                {envNote('logFormat')}
              </Row>
            </div>

            <h3 className="gh">File</h3>
            <div className="card">
              {initial.logFilePath ? (
                <Row
                  label="Log file"
                  sub={short(initial.logFilePath)}
                  subTitle={initial.logFilePath}
                  control={
                    <button type="button" className="k-btn" onClick={reveal}>
                      Reveal
                    </button>
                  }
                />
              ) : (
                // resolveLogFile (logging.go) returns "" only when there is no
                // `[log] file` AND no resolvable logs directory — nothing is
                // being written to disk at all. Reveal is DROPPED rather than
                // disabled: there is no directory to open, and RevealLogFile
                // would run `open -R ""`, which fails with nothing on screen to
                // explain it. Saying why beats a button that quietly does nothing.
                <Row
                  label="Log file"
                  sub={
                    <>
                      none — set <code>[log] file</code> in knomit.toml
                    </>
                  }
                />
              )}
            </div>
          </section>
        </div>

        <footer className="foot">
          {/* Restart is an outcome of Save, so it belongs beside Save, under
              every page: whichever page caused it, the offer is in one place. */}
          {needsRestart && (
            <div className="restartbar">
              <p>
                {restartReason(restartKeys)}
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
      </main>
    </div>
  )
}

/** The pages holding a refused field, in sidebar order. */
function pagesWithErrors(errs: Record<string, string>): Page[] {
  return PAGES.map((p) => p.id).filter((id) => Object.keys(errs).some((f) => PAGE_OF[f] === id))
}

/**
 * One restart offer whatever owes it — not one bar per cause — saying what
 * the restart will apply.
 */
function restartReason(keys: string[]): string {
  return `These settings take effect after a restart of the server: ${keys.join(', ')}.`
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
