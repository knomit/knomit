import { useEffect, useState } from 'react'
import type React from 'react'

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

const FORMATS = [
  { value: 'console', label: 'console (human-readable)' },
  { value: 'json', label: 'json (structured)' },
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

/**
 * Section glyphs. Inline SVG rather than an icon dependency: three 12px marks
 * do not justify a package, and these inherit currentColor so they cannot drift
 * from the label beside them.
 *
 * Deliberately NOT colored. The palette assigns one meaning to each accent
 * (green live, blue link, amber warning, red error) and tinting section
 * headings would spend those meanings on decoration — the glyph's job is to
 * make a section recognisable at a glance, which shape does without hue.
 */
const ICONS: Record<string, React.ReactNode> = {
  // Stacked bars with a status pip: a listening service.
  server: (
    <>
      <rect x="1.5" y="2" width="11" height="4" rx="1" />
      <rect x="1.5" y="8" width="11" height="4" rx="1" />
      <circle cx="4" cy="4" r="0.9" fill="currentColor" stroke="none" />
      <circle cx="4" cy="10" r="0.9" fill="currentColor" stroke="none" />
    </>
  ),
  // Ragged lines: a log, not a document.
  logging: (
    <>
      <line x1="2" y1="3.5" x2="12" y2="3.5" />
      <line x1="2" y1="7" x2="9" y2="7" />
      <line x1="2" y1="10.5" x2="11" y2="10.5" />
    </>
  ),
  // A folder: where things are on disk.
  locations: (
    <>
      <path d="M1.5 3.5h4l1.2 1.6h5.8v6.4a1 1 0 0 1-1 1h-10a1 1 0 0 1-1-1z" />
    </>
  ),
  // Power symbol.
  startup: (
    <>
      <path d="M4 4.2a5 5 0 1 0 6 0" />
      <line x1="7" y1="1.5" x2="7" y2="6.5" />
    </>
  ),
}

function SectionHeading({ icon, children }: { icon: string; children: string }) {
  return (
    <h2 className="k-eyebrow section-heading">
      <svg
        className="section-icon"
        viewBox="0 0 14 14"
        width="12"
        height="12"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.3"
        strokeLinecap="round"
        aria-hidden="true"
      >
        {ICONS[icon]}
      </svg>
      {children}
    </h2>
  )
}

/**
 * The mark on a field whose value was refused. Collapsed to a single glyph so a
 * rejected field costs no vertical space in a window that cannot be resized,
 * and expands in place on click.
 *
 * A <button>, not an icon with a tooltip attribute: the message has to be
 * reachable by keyboard and readable by a screen reader, and title= is neither.
 * The expanded message carries role="alert" so it is announced when it opens.
 */
function FieldError({
  id,
  message,
  open,
  onToggle,
}: {
  id: string
  message: string
  open: boolean
  onToggle: () => void
}) {
  return (
    <span className="fielderr">
      <button
        type="button"
        className="fielderr-mark"
        aria-label={`Show the problem with this field`}
        aria-expanded={open}
        aria-controls={`${id}-error`}
        onClick={onToggle}
      >
        !
      </button>
      {open && (
        <span id={`${id}-error`} role="alert" className="fielderr-bubble">
          {message}
        </span>
      )}
    </span>
  )
}

export function SettingsForm({ initial, onSave, onRestart, onRevealLog, onCancel }: Props) {
  const [s, setS] = useState(initial)
  // Shown beside the buttons. Reserved for failures that belong to the WINDOW
  // rather than to one control: a save the backend refused, a relaunch that
  // could not resolve its bundle. Field problems never come here — they mark
  // the field instead.
  const [error, setError] = useState('')
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({})
  // Which field's message is currently expanded. One at a time: two open
  // bubbles in a 500px dialog overlap into noise.
  const [openError, setOpenError] = useState('')
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
      if (!kept[openError]) setOpenError('')
    }
  }

  async function save() {
    setError('')
    setSaved(false)
    const invalid = validate(s)
    setFieldErrors(invalid)
    const first = Object.keys(invalid)[0]
    if (first) {
      // Open the first offending field's message rather than leaving a bare
      // glyph. Pressing Save and getting only a small red mark, with the reason
      // one click away, is a puzzle; the mark is for finding the field again
      // later, not for explaining the refusal in the first place.
      setOpenError(first)
      return
    }
    setOpenError('')
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

  const errFor = (field: string) =>
    fieldErrors[field] && (
      <FieldError
        id={field}
        message={fieldErrors[field]}
        open={openError === field}
        onToggle={() => setOpenError(openError === field ? '' : field)}
      />
    )

  // Amber, not red: nothing has failed.. The value on screen is simply not the
  // one in the file, and a save will not change it — the same "this is not what
  // you think" the knowledge app paints amber.
  const envNote = (field: string) =>
    overridden(field) && (
      <p className="note is-override">
        Set by <code>{ENV_FOR[field]}</code> in the environment, which beats
        knomit.toml — saving cannot change this.
      </p>
    )

  return (
    <div className="settings">
      {/* Three named groups rather than one flat stack of five controls: what
          the server binds, what gets logged, what happens at login. The eyebrow
          is the knowledge app's own section label. There is no <h1>: the window
          title bar already says "Knomit Settings", and repeating it inside cost
          a line of a window that has none to spare. */}
      <section className="group">
        <SectionHeading icon="server">Server</SectionHeading>
        <div className="field">
          <label htmlFor="port">Port</label>
          <div className="control">
            <input
              id="port"
              className="k-input"
              value={s.port}
              disabled={overridden('port')}
              aria-invalid={fieldErrors.port ? true : undefined}
              onChange={(e) => edit({ port: e.target.value })}
            />
            {errFor('port')}
          </div>
        </div>
        {envNote('port')}
        {initial.effectivePort > 0 && String(initial.effectivePort) !== initial.port && (
          <p className="note is-effective">
            Currently bound to <strong>{initial.effectivePort}</strong> — the
            configured port was unavailable.
          </p>
        )}
      </section>

      <section className="group">
        <SectionHeading icon="logging">Logging</SectionHeading>
        <div className="field">
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
            {errFor('logLevel')}
          </div>
        </div>
        {envNote('logLevel')}

        <div className="field">
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
            {errFor('logFormat')}
          </div>
        </div>
        {envNote('logFormat')}
      </section>

      <section className="group">
        <SectionHeading icon="startup">Startup</SectionHeading>
        <div className="field">
          <label htmlFor="startAtLogin">Start at login</label>
          <div className="control">
            <input
              id="startAtLogin"
              type="checkbox"
              checked={s.startAtLogin}
              onChange={(e) => edit({ startAtLogin: e.target.checked })}
            />
          </div>
        </div>
      </section>

      {needsRestart && (
        <div className="restart k-callout is-warn">
          <p>
            The port change takes effect after a restart. Connected MCP clients
            (Claude Code and anything else using the bridge) will need
            restarting to reconnect.
          </p>
          <button
            type="button"
            className="k-btn is-primary"
            onClick={restart}
            disabled={restarting}
          >
            {restarting ? 'Restarting…' : 'Restart Now'}
          </button>
        </div>
      )}

      <section className="group">
        <SectionHeading icon="locations">Locations</SectionHeading>
        <dl className="paths">
          <dt>Config</dt>
          <dd>{initial.configPath}</dd>
          <dt>Log file</dt>
          <dd>
            {initial.logFilePath}{' '}
            <button type="button" className="k-btn" onClick={reveal}>
              Reveal
            </button>
          </dd>
        </dl>
      </section>

      {/* Trailing edge, primary last — where a dialog's buttons belong, and
          where the platform puts them. "Saved." sits to their left so the
          confirmation reads as part of the same row rather than as a new line
          shifting everything below it. */}
      {/* The outcome of Save reads where Save is, not at the far end of the
          window. It also costs no reserved space: the button row is already
          here and already has a height, so a message arriving or expiring
          cannot move anything.
          Clipped to one line with an ellipsis, full text on hover — a long Go
          error must not be allowed to reflow the row it sits in. */}
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
    </div>
  )
}
