import { useEffect, useRef, useState, useSyncExternalStore } from 'react'
import { LogView, visibleLines } from './LogView.tsx'
import { MAX_LINES, clearLines, getLines, subscribe } from './logStore.ts'
import { Call } from '@wailsio/runtime'
import './App.css'

// The console levels zerolog writes. Ordered loudest-last so the list reads
// the way a severity filter is expected to.
//
// The labels say "and above" because that is what the filter does — it is a
// floor, not an equality test (see RANK in LogView.tsx). A bare "Warn" would
// promise only warnings and then show errors too, which is the same lie in the
// opposite direction from the bug this replaced.
const LEVELS = [
  { token: 'DBG', label: 'Debug and above' },
  { token: 'INF', label: 'Info and above' },
  { token: 'WRN', label: 'Warn and above' },
  { token: 'ERR', label: 'Error and above' },
]

// The live log viewer. It owns no lines of its own: the scrollback lives in
// logStore, which is subscribed before React mounts so the backlog batch cannot
// be missed. See the note in logStore.ts.
export function LogsApp() {
  const lines = useSyncExternalStore(subscribe, getLines)
  const [level, setLevel] = useState('')
  const [query, setQuery] = useState('')
  const [follow, setFollow] = useState(true)
  const scrollRef = useRef<HTMLDivElement>(null)
  // Purely for the status bar. GetSettings is the same binding SettingsApp
  // calls; this window wants one field of it. A rejection is not worth an error
  // state — the bar simply renders without the path.
  const [logPath, setLogPath] = useState('')
  // Set while the effect below moves the scroller itself, so its own scroll
  // does not look like the user scrolling away. Without it, Follow switches
  // itself off on the first line that arrives.
  const selfScroll = useRef(false)
  // The line count when Follow was released, so the pill can say how much has
  // arrived since — not how many lines exist.
  const releasedAt = useRef(0)

  // Pin to the bottom while following. Also runs on a level or query change,
  // because narrowing shortens the document and would otherwise leave the view
  // stranded past the new end.
  useEffect(() => {
    if (!follow) return
    const el = scrollRef.current
    if (!el) return
    selfScroll.current = true
    el.scrollTop = el.scrollHeight
    // Cleared on the next frame rather than immediately: the scroll event this
    // assignment triggers is dispatched asynchronously.
    const id = requestAnimationFrame(() => {
      selfScroll.current = false
    })
    return () => cancelAnimationFrame(id)
  }, [lines, level, query, follow])

  useEffect(() => {
    Call.ByName('main.NativeService.GetSettings')
      .then((s: { logFilePath?: string }) => setLogPath(s.logFilePath ?? ''))
      .catch(() => {})
  }, [])

  // Scrolling up to read is a request to stop being dragged to the bottom.
  // Before this, the only way out was noticing the checkbox.
  function onScroll() {
    if (selfScroll.current) return
    const el = scrollRef.current
    if (!el) return
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 24
    if (!atBottom && follow) {
      releasedAt.current = lines.length
      setFollow(false)
    }
  }

  const behind = follow ? 0 : Math.max(0, lines.length - releasedAt.current)
  // Same function the body renders through, so the count cannot disagree with
  // what is on screen.
  const shown = visibleLines(lines, level, query).length

  return (
    <div className="logs">
      <header className="toolbar">
        <label>
          Level
          <select
            className="k-select"
            value={level}
            onChange={(e) => setLevel(e.target.value)}
          >
            <option value="">All</option>
            {LEVELS.map(({ token, label }) => (
              <option key={token} value={token}>
                {label}
              </option>
            ))}
          </select>
        </label>
        <div className="search">
          <input
            type="search"
            className="k-input"
            placeholder="Search"
            aria-label="Search log lines"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
          {query && (
            <span className="hits">
              {shown} / {lines.length}
            </span>
          )}
        </div>
        {/* A real toggle, not a styled div: aria-pressed and keyboard operation
            come free, and the label still says what it does. */}
        <button
          type="button"
          className={follow ? 'k-btn tbtn on' : 'k-btn tbtn'}
          aria-pressed={follow}
          onClick={() => setFollow(!follow)}
        >
          Following
        </button>
        {/* Clears the view only. The file is the source of truth and is never
            touched from here — a "Clear" that deleted the log would destroy the
            evidence someone opened this window to read. */}
        {/* Pasting an excerpt into an issue is why this window gets opened, so
            Copy takes what is SHOWN — post-filter, post-search — not the whole
            buffer. */}
        <button
          type="button"
          className="k-btn"
          onClick={() => {
            void navigator.clipboard?.writeText(
              visibleLines(lines, level, query).join('\n'),
            )
          }}
        >
          Copy
        </button>
        <button type="button" className="k-btn" onClick={clearLines}>
          Clear view
        </button>
      </header>
      <div className="scroller" ref={scrollRef} onScroll={onScroll}>
        <LogView lines={lines} level={level} query={query} />
        {behind > 0 && (
          <button
            type="button"
            className="newpill"
            onClick={() => setFollow(true)}
          >
            ↓ {behind} new {behind === 1 ? 'line' : 'lines'}
          </button>
        )}
      </div>
      <footer className="statusbar">
        <span>
          {level ? `${level} and above` : 'All levels'} · showing{' '}
          <strong>{shown}</strong> of {lines.length}
        </span>
        {/* The cap is silent otherwise: logStore drops the oldest lines past
            MAX_LINES, and a log viewer that quietly discards history is lying
            by omission. Only said when it actually bites. */}
        {lines.length >= MAX_LINES && (
          <span className="sb-warn">oldest lines dropped ({MAX_LINES} max)</span>
        )}
        <span className="sb-spacer" />
        {logPath && (
          <>
            <span className="sb-path" title={logPath}>
              {logPath.replace(/^\/(Users|home)\/[^/]+/, '~')}
            </span>
            <button
              type="button"
              className="linkbtn"
              onClick={() => {
                void Call.ByName('main.NativeService.RevealLogFile')
              }}
            >
              Reveal
            </button>
          </>
        )}
      </footer>
    </div>
  )
}
