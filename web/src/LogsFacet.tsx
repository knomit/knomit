import { useEffect, useRef, useState, useSyncExternalStore } from 'react';
import type { KeyboardEvent } from 'react';
import { LogView } from './LogView';
import { visibleLines } from './logLines';
import { MAX_LINES, clearLines, connectLogStream, getLines, getReceived, subscribe } from './logStore';
import { FacetToolbar } from './FacetToolbar';

// LogsFacet is the Logs server page: the server's log, live. Ported from the
// desktop app's Logs window, which this replaced and which has since been
// removed, with three changes:
//
//  - The source is GET /api/v1/logs/events instead of a Wails event, reached
//    through the same API base every other call uses. That is the whole point
//    of the design: the log stream is part of the API, so the desktop gets it
//    through the mount it already has rather than a second transport.
//  - The status bar's log-file path and "Reveal in Finder" are gone — there is
//    no settings binding here, and the browser cannot open a Finder window. In
//    their place it reports what the STREAM can tell you: how deep the
//    server's ring is, and whether anything has been dropped.
//  - Appearance is inline style objects, like the rest of the shell.
//
// It is laid out like every facet: its controls are the facet toolbar (the
// shell's second level) and the log fills the body below it, with no card.

// The console levels zerolog writes, loudest last, as the segments of the
// level control. Each segment is a FLOOR, not an equality test (see RANK in
// logLines.ts): "warn" shows errors too, which the title on each says. "all"
// is the empty floor, which also admits the trace lines no segment names.
const LEVELS = [
  { token: '', label: 'all', title: 'All levels' },
  { token: 'DBG', label: 'debug', title: 'Debug and above' },
  { token: 'INF', label: 'info', title: 'Info and above' },
  { token: 'WRN', label: 'warn', title: 'Warn and above' },
  { token: 'ERR', label: 'error', title: 'Error and above' },
];

export function LogsFacet() {
  const lines = useSyncExternalStore(subscribe, getLines);
  // Arrivals, not length. Both come off the same store and the same
  // notification, so they can never be read a batch apart.
  const received = useSyncExternalStore(subscribe, getReceived);
  const [level, setLevel] = useState('');
  const [query, setQuery] = useState('');
  const [follow, setFollow] = useState(true);
  const scrollRef = useRef<HTMLDivElement>(null);
  // Set while the effect below moves the scroller itself, so its own scroll
  // does not look like the user scrolling away. Without it, Follow switches
  // itself off on the first line that arrives.
  const selfScroll = useRef(false);
  // The arrival count when Follow was released, so the pill can say how much
  // has come in since — not how many lines exist. Marked against the store's
  // received counter rather than lines.length, which stops rising once the
  // scrollback hits MAX_LINES and would peg the pill at zero from then on,
  // exactly when a log is busy enough for it to matter.
  //
  // STATE, not a ref, because the render below reads it to size the pill. As a
  // ref it happened to work only because the one writer also called
  // setFollow(false) in the same handler and that re-rendered — so the pill was
  // correct by coincidence, and a future write without a paired state change
  // would have left it stale with nothing to catch it. Both setStates sit in an
  // event handler, where React batches them into one render.
  const [releasedAt, setReleasedAt] = useState(0);

  // One stream per mounted page, closed on unmount. Opened here rather than at
  // module scope (as the desktop store does) because nothing can arrive before
  // this component exists: it is what opens the connection.
  useEffect(() => connectLogStream(), []);

  // Pin to the bottom while following. Also runs on a level or query change,
  // because narrowing shortens the document and would otherwise leave the view
  // stranded past the new end.
  useEffect(() => {
    if (!follow) return;
    const el = scrollRef.current;
    if (!el) return;
    selfScroll.current = true;
    el.scrollTop = el.scrollHeight;
    // Cleared on the next frame rather than immediately: the scroll event this
    // assignment triggers is dispatched asynchronously.
    const id = requestAnimationFrame(() => { selfScroll.current = false; });
    return () => cancelAnimationFrame(id);
  }, [lines, level, query, follow]);

  // Scrolling up to read is a request to stop being dragged to the bottom.
  function onScroll() {
    if (selfScroll.current) return;
    const el = scrollRef.current;
    if (!el) return;
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
    if (!atBottom && follow) {
      setReleasedAt(received);
      setFollow(false);
    }
  }

  const behind = follow ? 0 : Math.max(0, received - releasedAt);
  // Same function the body renders through, so the count cannot disagree with
  // what is on screen.
  const shown = visibleLines(lines, level, query).length;

  return (
    <div data-testid="logs-facet" style={page}>
      <FacetToolbar>
        <LevelControl value={level} onChange={setLevel} />
        <div style={{ display: 'flex', alignItems: 'center', gap: 6, minWidth: 0 }}>
          <input
            type="search"
            style={searchInput}
            placeholder="Search log…"
            aria-label="Search log lines"
            value={query}
            onChange={e => setQuery(e.target.value)}
          />
          {query && <span style={{ color: '#888', fontSize: 11, whiteSpace: 'nowrap' }}>{shown} / {lines.length}</span>}
        </div>
        <div style={{ flex: 1 }} />
        {/* A real toggle, not a styled div: aria-pressed and keyboard
            operation come free, and the label still says what it does. */}
        <button
          type="button"
          style={toolBtn(follow)}
          aria-pressed={follow}
          onClick={() => setFollow(!follow)}
        >
          Following
        </button>
        {/* Pasting an excerpt into an issue is why this page gets opened, so
            Copy takes what is SHOWN — post-filter, post-search — not the
            whole buffer. */}
        <button
          type="button"
          style={toolBtn(false)}
          onClick={() => { void navigator.clipboard?.writeText(visibleLines(lines, level, query).join('\n')); }}
        >
          Copy
        </button>
        {/* Clears the view only. The server's ring is untouched, and so is
            the subscription: the next line still arrives. */}
        <button
          type="button"
          style={toolBtn(false)}
          onClick={() => {
            // Re-base the mark in the same click that resets the count, or a
            // clear made while scrolled up leaves releasedAt above the
            // store's fresh zero and the pill reads nothing until it climbs
            // back.
            setReleasedAt(0);
            clearLines();
          }}
        >
          Clear view
        </button>
      </FacetToolbar>

      <div style={scrollerWrap}>
        <div className="scroller" data-testid="logs-scroller" style={scroller} ref={scrollRef} onScroll={onScroll}>
          <LogView lines={lines} level={level} query={query} />
        </div>
        {behind > 0 && (
          <button type="button" style={pill} onClick={() => setFollow(true)}>
            ↓ {behind} new {behind === 1 ? 'line' : 'lines'}
          </button>
        )}
      </div>

      <footer data-testid="logs-status" style={statusBar}>
        <span>this server, live</span>
        <span>
          {level ? `${level} and above` : 'All levels'} · showing <strong>{shown}</strong> of {lines.length}
        </span>
        {/* The cap is silent otherwise: logStore drops the oldest lines past
            MAX_LINES, and a log viewer that quietly discards history is lying
            by omission. Only said when it actually bites. */}
        {lines.length >= MAX_LINES && (
          <span style={{ color: '#facc15' }}>oldest lines dropped ({MAX_LINES} max)</span>
        )}
      </footer>
    </div>
  );
}

// LevelControl is the severity floor as a segmented control: a radiogroup,
// one tab stop, arrows move and select. Selecting is cheap (a filter on what is
// already in memory), so selection following the arrow keys loses nothing.
function LevelControl({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  const refs = useRef<(HTMLButtonElement | null)[]>([]);
  const selected = Math.max(0, LEVELS.findIndex(l => l.token === value));
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const step = e.key === 'ArrowRight' || e.key === 'ArrowDown' ? 1 : e.key === 'ArrowLeft' || e.key === 'ArrowUp' ? -1 : 0;
    if (step === 0) return;
    e.preventDefault();
    const next = (selected + step + LEVELS.length) % LEVELS.length;
    onChange(LEVELS[next].token);
    refs.current[next]?.focus();
  };
  return (
    <div role="radiogroup" aria-label="Level" style={segGroup} onKeyDown={onKeyDown}>
      {LEVELS.map((l, i) => {
        const on = i === selected;
        return (
          <button
            key={l.label}
            ref={el => { refs.current[i] = el; }}
            type="button"
            role="radio"
            aria-checked={on}
            tabIndex={on ? 0 : -1}
            title={l.title}
            onClick={() => onChange(l.token)}
            style={seg(on, i === 0)}
          >
            {l.label}
          </button>
        );
      })}
    </div>
  );
}

// The page fills the facet body and is bounded by it: flex '1 1 0' with
// minHeight 0, so the log's whole scrollback cannot propagate up as this
// column's min-content height and push the status line off screen
// (kb/gotchas/web/ui/manage-page/flex-min-content-propagation). Only the
// scroller scrolls; overflow hidden here so nothing else can.
const page: React.CSSProperties = {
  flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column', overflow: 'hidden',
  background: '#141414',
};
// The log's frame takes every pixel the toolbar and the status line leave. It
// carries NO overflow of its own: position relative only anchors the pill,
// which is out of flow and so unmoved by the column flex.
const scrollerWrap: React.CSSProperties = {
  position: 'relative', flex: 1, minHeight: 0,
  display: 'flex', flexDirection: 'column',
};
// Bounded by its share of the body, not by a fraction of the viewport (the old
// `maxHeight: '58vh'` measured the wrong box and left dead space).
const scroller: React.CSSProperties = {
  flex: 1, minHeight: 0, overflowY: 'auto', background: '#101010',
  padding: '6px 0',
};
const pill: React.CSSProperties = {
  position: 'absolute', bottom: 16, left: '50%', transform: 'translateX(-50%)',
  background: '#22303a', color: '#cfe', border: '1px solid #3a4a58',
  borderRadius: 12, padding: '3px 12px', fontSize: 11, cursor: 'pointer',
};
const statusBar: React.CSSProperties = {
  display: 'flex', alignItems: 'center', gap: 12, flexShrink: 0,
  padding: '5px 12px', fontSize: 11, color: '#777',
  borderTop: '1px solid #1c1c1c', background: '#0f0f0f',
};
const segGroup: React.CSSProperties = {
  display: 'inline-flex', flexShrink: 0,
  border: '1px solid #262626', borderRadius: 4, overflow: 'hidden',
};
const seg = (on: boolean, first: boolean): React.CSSProperties => ({
  background: on ? '#1f1f1f' : '#161616', color: on ? '#eee' : '#888',
  border: 'none', borderLeft: first ? 'none' : '1px solid #262626',
  padding: '3px 8px', fontSize: 11, fontFamily: 'var(--k-font-mono)', cursor: 'pointer',
});
const searchInput: React.CSSProperties = {
  background: '#161616', color: '#ddd', border: '1px solid #242424', borderRadius: 4,
  padding: '4px 8px', fontSize: 12, width: 240, minWidth: 0,
};
const toolBtn = (on: boolean): React.CSSProperties => ({
  background: on ? '#22303a' : 'transparent', color: on ? '#cfe' : '#9a9a9a',
  border: '1px solid #333', borderRadius: 4, padding: '3px 9px', fontSize: 12, cursor: 'pointer',
  flexShrink: 0,
});
