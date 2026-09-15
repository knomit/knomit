import { useEffect, useRef, useState, useSyncExternalStore } from 'react';
import { LogView } from './LogView';
import { visibleLines } from './logLines';
import { MAX_LINES, clearLines, connectLogStream, getLines, getReceived, subscribe } from './logStore';
import { card, cardLabel } from './manageStyles';

// ManageLogs is the server's log, live. Ported from the desktop app's Logs
// window (tools/desktop/ui/src/LogsApp.tsx), with three changes:
//
//  - The source is GET /api/v1/logs/events instead of a Wails event, reached
//    through the same API base every other call uses. That is the whole point
//    of the design: the log stream is part of the API, so the desktop gets it
//    through the mount it already has rather than a second transport.
//  - The status bar's log-file path and "Reveal in Finder" are gone — there is
//    no settings binding here, and the browser cannot open a Finder window. In
//    their place it reports what the STREAM can tell you: how deep the
//    server's ring is, and whether anything has been dropped.
//  - Appearance is inline style objects, like the other Manage pages.
//
// The desktop copy is untouched; retiring it is a separate change.

// The console levels zerolog writes. Ordered loudest-last so the list reads the
// way a severity filter is expected to.
//
// The labels say "and above" because that is what the filter does — it is a
// floor, not an equality test (see RANK in LogView.tsx). A bare "Warn" would
// promise only warnings and then show errors too.
const LEVELS = [
  { token: 'DBG', label: 'Debug and above' },
  { token: 'INF', label: 'Info and above' },
  { token: 'WRN', label: 'Warn and above' },
  { token: 'ERR', label: 'Error and above' },
];

export function ManageLogs() {
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
    <div data-testid="manage-logs">
      <div style={{ display: 'flex', alignItems: 'baseline', gap: 12 }}>
        <h2 style={{ margin: 0, fontSize: 16 }}>Logs</h2>
        <span style={{ color: '#888', fontSize: 12 }}>this server, live</span>
      </div>

      <div style={{ ...card, marginTop: 10 }}>
        <div style={cardLabel}>Server log</div>
        <header style={toolbar}>
          <label style={toolLabel}>
            Level
            <select style={selectStyle} value={level} onChange={e => setLevel(e.target.value)}>
              <option value="">All</option>
              {LEVELS.map(({ token, label }) => (
                <option key={token} value={token}>{label}</option>
              ))}
            </select>
          </label>
          <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
            <input
              type="search"
              style={inputStyle}
              placeholder="Search"
              aria-label="Search log lines"
              value={query}
              onChange={e => setQuery(e.target.value)}
            />
            {query && <span style={{ color: '#888', fontSize: 11 }}>{shown} / {lines.length}</span>}
          </div>
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
        </header>

        <div style={{ position: 'relative' }}>
          <div className="scroller" style={scroller} ref={scrollRef} onScroll={onScroll}>
            <LogView lines={lines} level={level} query={query} />
          </div>
          {behind > 0 && (
            <button type="button" style={pill} onClick={() => setFollow(true)}>
              ↓ {behind} new {behind === 1 ? 'line' : 'lines'}
            </button>
          )}
        </div>

        <footer style={statusBar}>
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
    </div>
  );
}

const toolbar: React.CSSProperties = {
  display: 'flex', alignItems: 'center', gap: 10, flexWrap: 'wrap',
  padding: '4px 0 10px', borderBottom: '1px solid #222',
};
const toolLabel: React.CSSProperties = { display: 'flex', alignItems: 'center', gap: 6, fontSize: 12, color: '#aaa' };
const selectStyle: React.CSSProperties = {
  background: '#141414', color: '#ddd', border: '1px solid #333', borderRadius: 4, padding: '3px 6px', fontSize: 12,
};
const inputStyle: React.CSSProperties = { ...selectStyle, width: 200 };
const toolBtn = (on: boolean): React.CSSProperties => ({
  background: on ? '#22303a' : 'transparent', color: on ? '#cfe' : '#9a9a9a',
  border: '1px solid #333', borderRadius: 4, padding: '3px 9px', fontSize: 12, cursor: 'pointer',
});
// A bounded scroller, not the page: the log is the one Manage pane whose
// content is unbounded, and letting it grow the page would put the toolbar and
// the status line off screen exactly when they are needed.
const scroller: React.CSSProperties = {
  maxHeight: '58vh', minHeight: 220, overflowY: 'auto', background: '#101010',
  border: '1px solid #1c1c1c', borderRadius: 4, marginTop: 10,
};
const pill: React.CSSProperties = {
  position: 'absolute', bottom: 12, left: '50%', transform: 'translateX(-50%)',
  background: '#22303a', color: '#cfe', border: '1px solid #3a4a58',
  borderRadius: 12, padding: '3px 12px', fontSize: 11, cursor: 'pointer',
};
const statusBar: React.CSSProperties = {
  display: 'flex', alignItems: 'center', gap: 12, paddingTop: 8, fontSize: 11, color: '#777',
};
