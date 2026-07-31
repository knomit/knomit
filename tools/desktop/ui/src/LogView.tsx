import type React from 'react'
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

// Severity order, quietest first. The filter is a FLOOR: picking a level asks
// for that level and everything louder. An equality test — which this replaced
// — meant choosing Warn hid every ERR, FTL and PNC, so a user filtering for
// trouble saw strictly less of it than "All" did.
//
// A line with no rank (a wrapped continuation, the noLogFileNotice from
// logstream.go) is never filtered out. Dropping a line we failed to parse is
// worse than showing one the filter did not ask for: an absent line is
// indistinguishable from nothing having happened.
const RANK: Record<string, number> = {
  TRC: 0,
  DBG: 1,
  INF: 2,
  WRN: 3,
  ERR: 4,
  FTL: 5,
  PNC: 6,
}

function atOrAbove(line: string, min: string): boolean {
  const rank = RANK[levelOf(line) ?? '']
  return rank === undefined || rank >= RANK[min]
}

// An empty view is ambiguous in a way that costs the user real time: it looks
// identical whether the app is idle, the filter is too narrow, or the window is
// wired to a file nothing is writing to. Naming which one it is turns a blank
// rectangle into an answer. (The backend says its piece too, for the case where
// there is no log file at all to tail — see noLogFileNotice in logstream.go.)
function emptyMessage(hasLines: boolean, level?: string): string {
  if (hasLines && level) return `No lines at ${level} or above yet.`
  return 'Waiting for log output…'
}

// Splits a console-formatted line into its three parts so the timestamp can
// recede and the message can lead.
//
// Returns undefined for anything that is not console-shaped — a wrapped
// continuation, or the backend's no-log-file notice — which then renders whole
// and unstyled rather than being mangled by a bad guess.
function split(line: string): { ts: string; level: string; msg: string } | undefined {
  const m = /^(\S+)\s+([A-Z]{3})\s+([\s\S]*)$/.exec(line)
  return m ? { ts: m[1], level: m[2], msg: m[3] } : undefined
}

// The FILE stamp is RFC3339 ("2026-07-31T11:15:39-04:00") because a log file
// outlives the day it was written and a bare clock time cannot tell Monday's
// crash from this morning's. A live tail has the opposite problem: the date is
// identical on every visible line, so rendering it spends 11 of 25 characters
// restating the obvious.
//
// So the file keeps the full stamp and the window shows the clock, with the
// date recovered two ways it cannot be lost: a divider whenever the day
// changes, and the original value on hover.
//
// Falls through unchanged for any stamp that is not RFC3339 — a rotated file
// written by an older build (time.Kitchen, "5:01PM"), or a line that carries
// only a time.
function displayTime(ts: string): string {
  const m = /^\d{4}-\d{2}-\d{2}T(\d{2}:\d{2}:\d{2})/.exec(ts)
  return m ? m[1] : ts
}

// The calendar day a stamp belongs to, or undefined when it carries no date —
// in which case no divider can be derived, and none is shown.
function dayOf(ts: string | undefined): string | undefined {
  if (!ts) return undefined
  const m = /^(\d{4}-\d{2}-\d{2})T/.exec(ts)
  return m ? m[1] : undefined
}

function dayLabel(day: string): string {
  const d = new Date(`${day}T00:00:00`)
  if (Number.isNaN(d.getTime())) return day
  return d.toLocaleDateString(undefined, {
    weekday: 'long',
    day: 'numeric',
    month: 'long',
  })
}

export function LogView({ lines, level }: Props) {
  const shown = level ? lines.filter((line) => atOrAbove(line, level)) : lines
  if (shown.length === 0) {
    return (
      <pre className="logview">
        <div className="logempty">{emptyMessage(lines.length > 0, level)}</div>
      </pre>
    )
  }
  // Emitted as a flat list rather than grouped: a divider is a row in the
  // stream, and the stream is what Follow scrolls to the bottom of.
  const rows: React.ReactNode[] = []
  let lastDay: string | undefined
  shown.forEach((line, i) => {
    const parts = split(line)
    const day = dayOf(parts?.ts)
    // First dated line gets a divider too — without it the topmost lines in the
    // backlog are the only ones on screen with no day at all.
    if (day && day !== lastDay) {
      rows.push(
        <div key={`day-${i}`} className="logday">
          {dayLabel(day)}
        </div>,
      )
      lastDay = day
    }
    rows.push(
      // Index keys: lines are append-only and never reordered, and the content
      // itself is not unique (repeated messages are normal).
      <div key={i} className="logline" data-level={parts?.level}>
        {parts ? (
          <>
            {/* title carries the stamp the file holds, so the precision the
                window drops is one hover away rather than gone. */}
            <span className="ts" title={parts.ts}>
              {displayTime(parts.ts)}
            </span>{' '}
            <span className="lvl">{parts.level}</span>{' '}
            <span className="msg">{parts.msg}</span>
          </>
        ) : (
          line
        )}
      </div>,
    )
  })

  return (
    <pre className="logview">
      {rows}
    </pre>
  )
}
