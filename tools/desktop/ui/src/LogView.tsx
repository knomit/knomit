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

export function LogView({ lines, level }: Props) {
  const shown = level ? lines.filter((line) => levelOf(line) === level) : lines
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
