import { useState } from 'react'

// Mirrors the Settings struct in tools/desktop/settings.go field for field.
// The json tags there are the wire names; these must match them exactly.
export interface Settings {
  port: string
  logLevel: string
  logFormat: string
  startAtLogin: boolean
  effectivePort: number
  configPath: string
  logFilePath: string
  overriddenByEnv: string[]
}

interface Props {
  initial: Settings
  onSave: (s: Settings) => Promise<void>
  // Fired, deliberately NOT awaited for success — see restart() below.
  onRestart: () => Promise<void>
  onRevealLog: () => Promise<void>
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
}

/** The zerolog levels config.Validate accepts, quietest first. */
const LEVELS = ['trace', 'debug', 'info', 'warn', 'error']

const FORMATS = [
  { value: 'console', label: 'console (human-readable)' },
  { value: 'json', label: 'json (structured)' },
]

/**
 * Rejects exactly what validateSettings (settings.go) rejects, before the
 * round trip. Go is still the authority — this is not a substitute for it —
 * but catching it here means knomit.toml is never even opened for a value that
 * would be refused, and the user sees a sentence rather than a wrapped Go
 * error. Returns '' when the settings are acceptable.
 */
function validate(s: Settings): string {
  // Not trimmed, and digits only: strconv.Atoi does not trim either, so " 20000"
  // is a value Go would reject and the form must not send.
  if (!/^\d+$/.test(s.port)) {
    return `Port must be a number, got "${s.port}".`
  }
  const port = Number(s.port)
  // Below 1024 needs root on Unix; knomit runs as the logged-in user.
  if (port < 1024 || port > 65535) {
    return `Port must be between 1024 and 65535, got ${port}.`
  }
  // The empty level validateSettings singles out cannot be produced by a
  // <select> over LEVELS, but the check costs nothing and pins the coupling.
  if (!LEVELS.includes(s.logLevel)) {
    return `Log level must be one of ${LEVELS.join(', ')}, got "${s.logLevel}".`
  }
  if (!FORMATS.some((f) => f.value === s.logFormat)) {
    return `Log format must be console or json, got "${s.logFormat}".`
  }
  return ''
}

/** Wails rejects with an Error; unwrap it so the user sees the message alone. */
function message(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}

export function SettingsForm({ initial, onSave, onRestart, onRevealLog }: Props) {
  const [s, setS] = useState(initial)
  const [error, setError] = useState('')
  const [saved, setSaved] = useState(false)
  const [restarting, setRestarting] = useState(false)
  // The port as of the last save that actually succeeded, so the restart offer
  // reflects what is ON DISK rather than what is in the form. Deriving it this
  // way — rather than keeping a needsRestart flag that each save recomputes —
  // is what stops a LATER failed save from retracting a restart that an
  // earlier successful one genuinely owes.
  const [savedPort, setSavedPort] = useState(initial.port)
  // Only the port needs a restart: it is bound once at boot. Level and format
  // are applied live by SaveSettings.
  const needsRestart = savedPort !== initial.port

  // envOverrides returns a NIL slice when nothing is set, and encoding/json
  // renders that as `null` rather than `[]`. That is the ordinary case, so
  // defaulting here is what keeps the window from coming up blank.
  const overriddenBy = initial.overriddenByEnv ?? []
  const overridden = (field: string) => {
    const key = ENV_FOR[field]
    return key !== undefined && overriddenBy.includes(key)
  }

  function edit(patch: Partial<Settings>) {
    setS({ ...s, ...patch })
    setSaved(false)
  }

  async function save() {
    setError('')
    setSaved(false)
    const invalid = validate(s)
    if (invalid) {
      setError(invalid)
      return
    }
    try {
      await onSave(s)
    } catch (e) {
      // applySettings validates before touching either backend, so a rejected
      // save changed nothing on disk — savedPort deliberately stays put.
      setError(message(e))
      return
    }
    setSaved(true)
    setSavedPort(s.port)
  }

  /**
   * Fires RestartApp and commits to a terminal state. Read this before
   * "fixing" it into an `await`.
   *
   * RestartApp NEVER RETURNS on the success path. Its last step is
   * onRestart -> wapp.Quit(), which is InvokeSync(destroy) -> NSApp terminate:
   * the process is gone before the Go function reaches `return nil`, so the
   * Wails transport never writes a response and THIS PROMISE NEVER SETTLES.
   * That was confirmed empirically in Task 9 — the harness's post-call log line
   * never printed. A promise that never settles is the SUCCESS signal here.
   *
   * So there is nothing to await and no spinner to clear: the button enters
   * "Restarting…" and stays there until the window it lives in is destroyed,
   * a few seconds later (RestartApp first drains the HTTP server, so the state
   * is visible for a real interval rather than a flash).
   *
   * A REJECTION, however, is real and must be shown: RestartApp returns an
   * error when the relaunch target cannot be resolved or the spawn fails —
   * which is every time in a dev build, since bundlePathFor needs a .app. The
   * app is still running in that case, so the button comes back.
   */
  function restart() {
    setError('')
    setRestarting(true)
    onRestart().catch((e: unknown) => {
      setRestarting(false)
      setError(message(e))
    })
  }

  function reveal() {
    onRevealLog().catch((e: unknown) => setError(message(e)))
  }

  const envNote = (field: string) =>
    overridden(field) && (
      <p className="note">
        Set by {ENV_FOR[field]} in the environment, which beats knomit.toml —
        saving cannot change this.
      </p>
    )

  return (
    <div className="settings">
      <h1>Settings</h1>

      <div className="field">
        <label htmlFor="port">Port</label>
        <input
          id="port"
          value={s.port}
          disabled={overridden('port')}
          onChange={(e) => edit({ port: e.target.value })}
        />
      </div>
      {envNote('port')}
      {initial.effectivePort > 0 && String(initial.effectivePort) !== initial.port && (
        <p className="note">
          Currently bound to {initial.effectivePort} — the configured port was
          unavailable.
        </p>
      )}

      <div className="field">
        <label htmlFor="logLevel">Log level</label>
        <select
          id="logLevel"
          value={s.logLevel}
          disabled={overridden('logLevel')}
          onChange={(e) => edit({ logLevel: e.target.value })}
        >
          {LEVELS.map((l) => (
            <option key={l} value={l}>
              {l}
            </option>
          ))}
        </select>
      </div>
      {envNote('logLevel')}

      <div className="field">
        <label htmlFor="logFormat">Log format</label>
        <select
          id="logFormat"
          value={s.logFormat}
          disabled={overridden('logFormat')}
          onChange={(e) => edit({ logFormat: e.target.value })}
        >
          {FORMATS.map((f) => (
            <option key={f.value} value={f.value}>
              {f.label}
            </option>
          ))}
        </select>
      </div>
      {envNote('logFormat')}

      <div className="field">
        <label htmlFor="startAtLogin">Start at login</label>
        <input
          id="startAtLogin"
          type="checkbox"
          checked={s.startAtLogin}
          onChange={(e) => edit({ startAtLogin: e.target.checked })}
        />
      </div>

      <div className="actions">
        <button type="button" onClick={save}>
          Save
        </button>
        {saved && !needsRestart && (
          <span role="status" className="ok">
            Saved.
          </span>
        )}
      </div>

      {error && (
        <p role="alert" className="error">
          {error}
        </p>
      )}

      {needsRestart && (
        <div className="restart">
          <p>
            The port change takes effect after a restart. Connected MCP clients
            (Claude Code and anything else using the bridge) will need
            restarting to reconnect.
          </p>
          <button type="button" onClick={restart} disabled={restarting}>
            {restarting ? 'Restarting…' : 'Restart Now'}
          </button>
        </div>
      )}

      <dl className="paths">
        <dt>Config</dt>
        <dd>{initial.configPath}</dd>
        <dt>Log file</dt>
        <dd>
          {initial.logFilePath}{' '}
          <button type="button" onClick={reveal}>
            Reveal
          </button>
        </dd>
      </dl>
    </div>
  )
}
