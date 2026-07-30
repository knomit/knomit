interface Props {
  lines: string[]
  /** Console level token to filter on (DBG/INF/WRN/ERR). Empty shows all. */
  level?: string
}

// The log file is console-formatted text ("1:12PM INF reconcile loop stopped
// repo=core"), so the level is the SECOND whitespace-separated token, not a
// structured field. Matching the token anywhere in the line would hide every
// INF line that merely mentions ERR, which is exactly the line someone
// filtering for errors is hunting through.
function levelOf(line: string): string | undefined {
  return line.trim().split(/\s+/)[1]
}

// An empty view is ambiguous in a way that costs the user real time: it looks
// identical whether the app is idle, the filter is too narrow, or the window is
// wired to a file nothing is writing to. Naming which one it is turns a blank
// rectangle into an answer. (The backend says its piece too, for the case where
// there is no log file at all to tail — see noLogFileNotice in logstream.go.)
function emptyMessage(hasLines: boolean, level?: string): string {
  if (hasLines && level) return `No lines at this level yet — the filter is set to ${level}.`
  return 'Waiting for log output…'
}

export function LogView({ lines, level }: Props) {
  const shown = level ? lines.filter((line) => levelOf(line) === level) : lines
  if (shown.length === 0) {
    return (
      <pre className="logview">
        <div className="logempty">{emptyMessage(lines.length > 0, level)}</div>
      </pre>
    )
  }
  return (
    <pre className="logview">
      {shown.map((line, i) => (
        // Index keys: lines are append-only and never reordered, and the
        // content itself is not unique (repeated messages are normal).
        <div key={i} className="logline">
          {line}
        </div>
      ))}
    </pre>
  )
}
