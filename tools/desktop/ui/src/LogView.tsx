import type React from 'react'
interface Props {
  lines: string[]
  /** Case-insensitive substring, applied AFTER the severity filter. */
  query?: string
  /** Console level token to filter on (DBG/INF/WRN/ERR). Empty shows all. */
  level?: string
}

// The severity token of a line, or undefined when it is not a log line we can
// read. Derived from the SAME parse the renderer uses — see parseLine, and read
// the note there before reintroducing a separate "just take the second token"
// shortcut here.
function levelOf(line: string): string | undefined {
  return parseLine(line)?.level
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

// indexOf, not a RegExp: a user typing "(" while searching must not throw, and
// escaping a pattern to avoid that is more machinery than a substring needs.
function hasMatch(line: string, query: string): boolean {
  return line.toLowerCase().includes(query.toLowerCase())
}

// What a search runs against: the text the window RENDERS, not the bytes on
// disk. For a console line the two are the same. For a JSON record they are
// not, and searching the raw form gets it backwards in both directions — a
// query for `api=19278`, which is what the user can see, would find nothing,
// while `"api":` would select a line and then highlight nothing, because
// highlighting works off the reconstructed message. Searching what is on
// screen is the only version of this that is not surprising.
function searchText(line: string): string {
  const p = parseLine(line)
  return p ? `${p.ts} ${p.level} ${p.msg}` : line
}

/** Splits text into alternating non-match / match runs for highlighting. */
export function highlight(text: string, query: string): { text: string; hit: boolean }[] {
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
export function splitTail(msg: string): { head: string; tail: string } {
  const m = /\s(?=[a-z_]+=)/.exec(msg)
  return m ? { head: msg.slice(0, m.index), tail: msg.slice(m.index + 1) } : { head: msg, tail: '' }
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
function emptyMessage(hasLines: boolean, level?: string, query?: string): string {
  if (hasLines && query) return `No lines match “${query}”.`
  if (hasLines && level) return `No lines at ${level} or above yet.`
  return 'Waiting for log output…'
}

// zerolog's JSON `level` values mapped onto the three-letter tokens the console
// format writes. RANK, the Level menu and the rendered `.lvl` column all speak
// tokens, so this table is the one place the two vocabularies meet.
const JSON_LEVEL: Record<string, string | undefined> = {
  trace: 'TRC',
  debug: 'DBG',
  info: 'INF',
  warn: 'WRN',
  error: 'ERR',
  fatal: 'FTL',
  panic: 'PNC',
}

// zerolog's reserved JSON keys. Everything else in the record is a caller's
// structured field and belongs in the dimmed tail, exactly as the console
// writer renders it.
const JSON_RESERVED = new Set(['level', 'time', 'message'])

// Renders one structured field's value the way the console writer would.
function fieldValue(v: unknown): string {
  return typeof v === 'object' && v !== null ? JSON.stringify(v) : String(v)
}

// Reads a zerolog JSON record, reshaping it into the same three parts a console
// line yields — including flattening the leftover fields back into the
// `key=value` tail, so splitTail and the whole render path below stay identical
// for both formats.
//
// Returns undefined for anything without a level we recognise, rather than
// guessing: an unreadable line renders raw, which is the honest outcome.
function parseJSON(line: string): { ts: string; level: string; msg: string } | undefined {
  const trimmed = line.trimStart()
  if (!trimmed.startsWith('{')) return undefined
  let rec: Record<string, unknown>
  try {
    rec = JSON.parse(trimmed) as Record<string, unknown>
  } catch {
    return undefined // a truncated or half-flushed line
  }
  if (typeof rec !== 'object' || rec === null) return undefined
  const level = JSON_LEVEL[String(rec.level)]
  if (!level) return undefined

  const tail = Object.keys(rec)
    .filter((k) => !JSON_RESERVED.has(k))
    .map((k) => `${k}=${fieldValue(rec[k])}`)
  const msg = [rec.message === undefined ? '' : String(rec.message), ...tail]
    .filter((p) => p !== '')
    .join(' ')
  return { ts: String(rec.time ?? ''), level, msg }
}

/**
 * Splits a log line into its three parts so the timestamp can recede and the
 * message can lead.
 *
 * Handles BOTH shapes the file can carry, and that is load-bearing rather than
 * generous. `log.format` is a first-class choice in the Settings dialog, and
 * with "json" the file is a stream of records with no whitespace-delimited
 * level token. When this understood only the console shape, every JSON line
 * fell through to `undefined` — which the severity filter reads as "unrankable,
 * therefore always show" (see atOrAbove). The Level menu did not empty the
 * pane; it silently stopped filtering at all, which is the worse failure,
 * because a control that appears to work is not one anybody re-examines.
 *
 * This is also the ONLY parse: levelOf delegates here rather than taking the
 * second whitespace token on its own. Two parsers that had to agree about what
 * a level is were free to disagree, and did.
 *
 * Returns undefined for anything neither shape explains — a wrapped
 * continuation, or the backend's no-log-file notice — which then renders whole
 * and unstyled rather than being mangled by a bad guess.
 */
type Parsed = { ts: string; level: string; msg: string } | undefined

// Reads a console-formatted line: `<stamp> <LVL> <message>`. Only ever called
// after parseJSON has declined the line — see the ordering note in parseLine.
function parseConsole(line: string): Parsed {
  const m = /^(\S+)\s+([A-Z]{3})\s+([\s\S]*)$/.exec(line)
  return m ? { ts: m[1], level: m[2], msg: m[3] } : undefined
}

// A render asks for each line's parse up to three times — the severity filter,
// the search, and the row itself — and a batch re-renders the whole scrollback.
// For the console format that is a regex per call and would not be worth a
// cache; for JSON it is a JSON.parse, and 5000 lines times three per batch is
// not. Lines are immutable strings that are never rewritten, so the parse is a
// pure function of the key and can be cached without invalidation.
//
// Bounded, and dropped wholesale rather than by LRU: the working set is the
// scrollback, the store caps that at MAX_LINES, and a clear costs one rebuild
// of a set we were about to walk anyway. An unbounded Map here would hold every
// line the window has ever seen, which is the leak the scrollback cap exists to
// prevent.
const PARSE_CACHE_MAX = 12000
const parseCache = new Map<string, Parsed>()

function parseLine(line: string): Parsed {
  const hit = parseCache.get(line)
  // `undefined` is a legitimate cached value (an unparseable line), so ask
  // whether the key is present rather than whether the value is truthy —
  // otherwise every raw line is re-parsed on every pass, which is exactly the
  // case the cache is here for.
  if (hit !== undefined || parseCache.has(line)) return hit

  // JSON FIRST, and the order is load-bearing rather than tidy. The console
  // pattern can match a json line by accident: it asks only for a
  // whitespace-free run, then a three-letter uppercase token, and a record
  // whose first embedded space is followed by such a token satisfies it —
  // `{"level":"error","message":"HTTP GET failed"}` parses as level "GET".
  //
  // That is not merely a row rendered wrong. "GET" is absent from RANK, which
  // atOrAbove reads as unrankable and therefore always-show, so the misparse
  // walks the line straight past the severity filter — the exact failure json
  // support was added to fix, just reached by a different route.
  //
  // parseJSON declines anything not starting with "{" immediately, so this
  // costs a prefix check for the console lines that are the common case.
  const parsed: Parsed = parseJSON(line) ?? parseConsole(line)

  if (parseCache.size >= PARSE_CACHE_MAX) parseCache.clear()
  parseCache.set(line, parsed)
  return parsed
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

/**
 * The lines a given threshold and query leave visible.
 *
 * Exported because the toolbar's hit count has to be the SAME number as the
 * body renders. Computing it twice is how "3 / 500" ends up disagreeing with
 * what is on screen.
 *
 * Search narrows what the threshold already allowed, so the count reads as
 * "matches at this level" rather than "matches in the file".
 */
export function visibleLines(lines: string[], level?: string, query?: string): string[] {
  const bySeverity = level ? lines.filter((line) => atOrAbove(line, level)) : lines
  return query ? bySeverity.filter((line) => hasMatch(searchText(line), query)) : bySeverity
}

export function LogView({ lines, level, query }: Props) {
  const shown = visibleLines(lines, level, query)
  if (shown.length === 0) {
    return (
      <pre className="logview">
        <div className="logempty">{emptyMessage(lines.length > 0, level, query)}</div>
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
            </span>
            <span className="lvl">{parts.level}</span>
            {/* Highlighting is confined to the message. A hit inside the
                timestamp or the level column would mark a column the user is
                not reading, and the level token is a fixed vocabulary anyway. */}
            <span className="msg">
              {(() => {
                const { head, tail } = splitTail(parts.msg)
                const paint = (text: string) =>
                  query
                    ? highlight(text, query).map((run, j) =>
                        run.hit ? <mark key={j}>{run.text}</mark> : run.text,
                      )
                    : text
                return (
                  <>
                    {paint(head)}
                    {tail && <span className="tail"> {paint(tail)}</span>}
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
    <pre className="logview">
      {rows}
    </pre>
  )
}
