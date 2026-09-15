import type React from 'react';
import { parseLine, visibleLines } from './logLines';

// Ported from the desktop app's Logs window (tools/desktop/ui/src/LogView.tsx).
// The PARSING is copied deliberately unchanged — it carries several fixes that
// each cost a real bug (json before console, the severity FLOOR, unrankable
// lines always shown), and the comments explaining them are worth more here
// than a rewrite would be. What changed is presentation: appearance is inline
// style objects, as everywhere else under Manage, instead of App.css classes.
//
// The class names survive as semantic markers and test hooks — they carry no
// stylesheet here — so the ported tests select the same rows the desktop ones
// did.
//
// The desktop copy is untouched; retiring it is a separate change.
interface Props {
  lines: string[]
  /** Case-insensitive substring, applied AFTER the severity filter. */
  query?: string
  /** Console level token to filter on (DBG/INF/WRN/ERR). Empty shows all. */
  level?: string
}

/** Splits text into alternating non-match / match runs for highlighting. */
function highlight(text: string, query: string): { text: string; hit: boolean }[] {
  if (!query) return [{ text, hit: false }]
  const hay = text.toLowerCase()
  const needle = query.toLowerCase()
  const out: { text: string; hit: boolean }[] = []
  let i = 0
  for (;;) {
    const at = hay.indexOf(needle, i)
    if (at < 0) break
    if (at > i) out.push({ text: text.slice(i, at), hit: false })
    out.push({ text: text.slice(at, at + needle.length), hit: true })
    i = at + needle.length
  }
  if (i < text.length) out.push({ text: text.slice(i), hit: false })
  return out
}

// A console message is a human sentence followed by a structured tail
// ("reconcile failed repo=core error=..."). Splitting at the first ` key=`
// lets the tail recede: it is reference detail, and at full contrast it
// competes with the sentence that says what happened.
//
// No match means the whole message is the sentence, which is the right
// fallback — most lines have no tail at all.
function splitTail(msg: string): { head: string; tail: string } {
  const m = /\s(?=[a-z_]+=)/.exec(msg)
  return m ? { head: msg.slice(0, m.index), tail: msg.slice(m.index + 1) } : { head: msg, tail: '' }
}

// An empty view is ambiguous in a way that costs the user real time: it looks
// identical whether the app is idle, the filter is too narrow, or the window is
// wired to a file nothing is writing to. Naming which one it is turns a blank
// rectangle into an answer. (The backend says its piece too, for the case where
// there is no log file at all to tail — see noLogFileNotice in logstream.go.)
function emptyMessage(hasLines: boolean, level?: string, query?: string): string {
  if (hasLines && query) return `No lines match “${query}”.`
  if (hasLines && level) return `No lines at ${level} or above yet.`
  return 'Waiting for log output…'
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

const viewStyle: React.CSSProperties = {
  margin: 0, padding: 0, fontFamily: 'var(--k-font-mono, ui-monospace, monospace)',
  fontSize: 12, lineHeight: '17px', whiteSpace: 'pre-wrap', wordBreak: 'break-word',
};
const emptyStyle: React.CSSProperties = { color: '#666', padding: 12 };
const dayStyle: React.CSSProperties = {
  color: '#7c9', fontSize: 11, letterSpacing: '0.06em', textTransform: 'uppercase',
  padding: '10px 8px 4px', borderBottom: '1px solid #222', margin: '4px 0',
};
const lineStyle: React.CSSProperties = { display: 'flex', gap: 8, padding: '0 8px' };
const tsStyle: React.CSSProperties = { color: '#555', flexShrink: 0 };
const msgStyle: React.CSSProperties = { color: '#ddd', minWidth: 0 };
const tailStyle: React.CSSProperties = { color: '#777' };
const markStyle: React.CSSProperties = { background: '#4a4520', color: '#ffd', borderRadius: 2 };

// Each level gets its own colour in the level column ONLY. Colouring the whole
// row would make a busy error log unreadable, and colouring nothing would make
// the column the one part of the line the eye cannot use.
const LEVEL_COLOR: Record<string, string> = {
  TRC: '#666', DBG: '#6a8caf', INF: '#4ade80', WRN: '#facc15', ERR: '#f87171',
  FTL: '#f87171', PNC: '#f87171',
};
// Precomputed per level, not built per row. A 5000-line scrollback re-renders
// whole, so a style object allocated inside the row loop is 5000 allocations a
// batch — and they are all one of seven values.
const LVL_STYLES: Record<string, React.CSSProperties> = Object.fromEntries(
  [...Object.keys(LEVEL_COLOR), ''].map(k => [k, { color: LEVEL_COLOR[k] ?? '#777', flexShrink: 0, width: 26 }]),
);
const lvlStyle = (level?: string): React.CSSProperties => LVL_STYLES[level ?? ''] ?? LVL_STYLES[''];

export function LogView({ lines, level, query }: Props) {
  const shown = visibleLines(lines, level, query)
  if (shown.length === 0) {
    return (
      <pre className="logview" style={viewStyle} data-testid="logview">
        <div className="logempty" style={emptyStyle}>{emptyMessage(lines.length > 0, level, query)}</div>
      </pre>
    )
  }
  // Emitted as a flat list rather than grouped: a divider is a row in the
  // stream, and the stream is what Follow scrolls to the bottom of.
  const rows: React.ReactNode[] = []
  let lastDay: string | undefined
  shown.forEach((line, i) => {
    const parts = parseLine(line)
    const day = dayOf(parts?.ts)
    // First dated line gets a divider too — without it the topmost lines in the
    // backlog are the only ones on screen with no day at all.
    if (day && day !== lastDay) {
      rows.push(
        <div key={`day-${i}`} className="logday" style={dayStyle}>
          {dayLabel(day)}
        </div>,
      )
      lastDay = day
    }
    rows.push(
      // Index keys: lines are append-only and never reordered, and the content
      // itself is not unique (repeated messages are normal).
      <div key={i} className="logline" style={lineStyle} data-level={parts?.level}>
        {parts ? (
          <>
            {/* title carries the stamp the file holds, so the precision the
                window drops is one hover away rather than gone. */}
            <span className="ts" style={tsStyle} title={parts.ts}>
              {displayTime(parts.ts)}
            </span>
            <span className="lvl" style={lvlStyle(parts.level)}>{parts.level}</span>
            {/* Highlighting is confined to the message. A hit inside the
                timestamp or the level column would mark a column the user is
                not reading, and the level token is a fixed vocabulary anyway. */}
            <span className="msg" style={msgStyle}>
              {(() => {
                const { head, tail } = splitTail(parts.msg)
                const paint = (text: string) =>
                  query
                    ? highlight(text, query).map((run, j) =>
                        run.hit ? <mark key={j} style={markStyle}>{run.text}</mark> : run.text,
                      )
                    : text
                return (
                  <>
                    {paint(head)}
                    {tail && <span className="tail" style={tailStyle}> {paint(tail)}</span>}
                  </>
                )
              })()}
            </span>
          </>
        ) : (
          line
        )}
      </div>,
    )
  })

  return (
    <pre className="logview" style={viewStyle} data-testid="logview">
      {rows}
    </pre>
  )
}
