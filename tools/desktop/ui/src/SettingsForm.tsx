import { useEffect, useState } from 'react'

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
  /** Discards edits and closes the window, the way a dialog's Cancel does. */
  onCancel: () => void
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
  return errs
}

/** Wails rejects with an Error; unwrap it so the user sees the message alone. */
function message(e: unknown): string {
  return e instanceof Error ? e.message : String(e)
}

export function SettingsForm({ initial, onSave, onRestart, onRevealLog, onCancel }: Props) {
  const [s, setS] = useState(initial)
  // Shown beside the buttons. Reserved for failures that belong to the WINDOW
  // rather than to one control: a save the backend refused, a relaunch that
  // could not resolve its bundle. Field problems never come here — they mark
  // the field instead.
  const [error, setError] = useState('')
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({})
  const [saved, setSaved] = useState(false)
  const [restarting, setRestarting] = useState(false)
  // The port as of the last save that actually succeeded, so the restart offer
  // reflects what is ON DISK rather than what is in the form. Deriving it this
  // way — rather than keeping a needsRestart flag that each save recomputes —
  // is what stops a LATER failed save from retracting a restart that an
  // earlier successful one genuinely owes.
  const [savedPort, setSavedPort] = useState(initial.port)

  // "Saved." is a notification, not a state: it reports that something just
  // happened, so it has to expire. Left standing it becomes a claim about the
  // present — a form edited after a save still reading "Saved." is telling the
  // user their unsaved changes are on disk.
  useEffect(() => {
    if (!saved) return
    const t = setTimeout(() => setSaved(false), 2600)
    return () => clearTimeout(t)
  }, [saved])
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
    const next = { ...s, ...patch }
    setS(next)
    setSaved(false)
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

  async function save() {
    setError('')
    setSaved(false)
    const invalid = validate(s)
    setFieldErrors(invalid)
    if (Object.keys(invalid).length > 0) {
      // The messages render inline under their fields. Nothing to open, and
      // nothing to choose between: every refusal is on screen at once.
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
    // "Saved." is suppressed only on the save that OWES the restart, where the
    // warning below is the stronger message and a cheerful "Saved." beside it
    // reads as "and you are done". Every later save still gets its
    // confirmation, including while that warning is still standing — otherwise
    // one port change would silence the feedback on every save after it.
    const changedPort = s.port !== savedPort
    setSaved(!(changedPort && s.port !== initial.port))
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
   * "Restarting…" and stays there until the window it lives in is destroyed.
   * Measured at ~60ms, so treat it as a flash, not a progress indication: once
   * the server is up, serverBoot.stop's select returns immediately on a closed
   * b.done (serverboot.go), and the multi-second stopGrace path only applies to
   * a boot still in flight. Do not build anything that assumes the state is
   * on screen long enough to read.
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
  const short = (path: string) => path.replace(/^\/(Users|home)\/[^/]+/, '~')

  return (
    // Three regions, not one scrolling column. The window is DisableResize, so
    // a form that grows — an env note under all three fields, a restart offer —
    // used to push Save past the bottom edge with no way to reach it. Only the
    // BODY scrolls now; the status header and the footer are pinned, so the
    // buttons are always where the user left them.
    <div className="ps">
      <header className="ps-status">
        {initial.effectivePort > 0 ? (
          <>
            <span
              className={
                String(initial.effectivePort) === initial.port
                  ? 'pip is-ok'
                  : 'pip is-warn'
              }
              aria-hidden="true"
            />
            <span className="ps-state">Running</span>
            <code className="ps-addr">127.0.0.1:{initial.effectivePort}</code>
            {String(initial.effectivePort) !== initial.port && (
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
        {/* One grouped list, not four icon-headed sections. Five controls did
            not need four eyebrows and four hand-drawn glyphs — the scaffolding
            outweighed the content and the eye counted headings instead of
            settings. Card and hairlines is also the vocabulary the Manage pane
            already uses. */}
        <div className="list">
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
                <span className="hint">1024–65535</span>
              )}
            </div>
            {errFor('port')}
            {envNote('port')}
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

        {/* A footnote, not a peer of Port: dim, mono, home-collapsed. */}
        <dl className="ps-paths">
          <dt>Config</dt>
          <dd title={initial.configPath}>{short(initial.configPath)}</dd>
          <dt>Log file</dt>
          <dd title={initial.logFilePath}>
            <span className="ps-path">{short(initial.logFilePath)}</span>
            <button type="button" className="linkbtn" onClick={reveal}>
              Reveal
            </button>
          </dd>
        </dl>
      </div>

      <footer className="ps-foot">
        {/* Restart is an outcome of Save, so it belongs beside Save. It used to
            render mid-form, where accepting a port change shoved the buttons
            down at the moment the user was reaching for them. */}
        {needsRestart && (
          <div className="restartbar">
            <p>
              The port change takes effect after a restart. Connected MCP
              clients (Claude Code and anything else using the bridge) will need
              restarting to reconnect.
            </p>
            <button
              type="button"
              className="k-btn"
              onClick={restart}
              disabled={restarting}
            >
              {restarting ? 'Restarting…' : 'Restart Now'}
            </button>
          </div>
        )}

        <div className="actions">
          {error ? (
            <p role="alert" className="outcome is-error" title={error}>
              {error}
            </p>
          ) : saved ? (
            <p role="status" className="outcome is-ok" title="Saved.">
              Saved.
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
