// How a log line is READ: parsed into its three parts, ranked by severity, and
// filtered. LogView.tsx is how one LOOKS.
//
// Split out for a mechanical reason with a real consequence: a module that
// exports anything besides components opts out of react-refresh, and
// ManageLogs needs visibleLines — it renders the "N shown" count through the
// very same function the body filters with, so the two can never disagree.
// Everything visibleLines leans on had to come with it, which is why the whole
// parse layer lives here rather than just the one function.
//
// The PARSING is carried over unchanged from the desktop app's Logs window: it
// encodes several fixes that each cost a real bug — json tried before console,
// the severity FLOOR, unrankable lines always shown — and the comments
// explaining them are worth more than a tidier rewrite.

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

function atOrAbove(line: string, min: string): boolean {
  const rank = RANK[levelOf(line) ?? '']
  return rank === undefined || rank >= RANK[min]
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
export type Parsed = { ts: string; level: string; msg: string } | undefined

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

export function parseLine(line: string): Parsed {
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
