import { useEffect, useRef, useState, useSyncExternalStore } from 'react'
import { LogView } from './LogView.tsx'
import { clearLines, getLines, subscribe } from './logStore.ts'
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
  const [follow, setFollow] = useState(true)
  const scrollRef = useRef<HTMLDivElement>(null)

  // Pin to the bottom while following. Also runs on a level change, because
  // narrowing the filter shortens the document and would otherwise leave the
  // view stranded past the new end.
  useEffect(() => {
    if (!follow) return
    const el = scrollRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [lines, level, follow])

  return (
    <div className="logs">
      <header className="toolbar">
        <label>
          <input
            type="checkbox"
            checked={follow}
            onChange={(e) => setFollow(e.target.checked)}
          />
          Follow
        </label>
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
        {/* Clears the view only. The file is the source of truth and is never
            touched from here — a "Clear" that deleted the log would destroy the
            evidence someone opened this window to read. */}
        <span className="spacer" />
        <button type="button" className="k-btn" onClick={clearLines}>
          Clear
        </button>
      </header>
      <div className="scroller" ref={scrollRef}>
        <LogView lines={lines} level={level} />
      </div>
    </div>
  )
}
