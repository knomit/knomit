import { useEffect, useRef, useState, useSyncExternalStore } from 'react';
import { LogView } from './LogView';
import { visibleLines } from './logLines';
import { MAX_LINES, clearLines, connectLogStream, getLines, getReceived, subscribe } from './logStore';
import { card, cardLabel } from './manageStyles';

// ManageLogs is the server's log, live. Ported from the desktop app's Logs
// window, which this replaced and which has since been removed, with three
// changes:
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
    <div data-testid="manage-logs" style={page}>
      <div style={logCard}>
        <div style={labelRow}>
          Server log
          <span style={labelHint}>this server, live</span>
        </div>
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

        <div style={scrollerWrap}>
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

// The page fills the detail pane rather than sizing to its content. detailCol
// (RepoManager.tsx) is a stretched flex item, so its height is definite and this
// percentage resolves against its CONTENT box — the pane's own padding stays
// outside it, so nothing overflows.
const page: React.CSSProperties = { height: '100%', display: 'flex', flexDirection: 'column' };
// flex:1 to take the pane's height; column so the toolbar and the status line
// stay pinned and only the body between them scrolls.
//
// Deliberately NO minHeight:0 here. The flex default min-height:auto floors the
// card at its own min-content height, so a window too short for the toolbar plus
// the scroller's 140px plus the status line overflows the pane — which scrolls,
// because detailCol is overflowY:auto — instead of the card shrinking while its
// contents refuse to, which would draw the log straight through its bottom
// border. That floor is not a constant: `toolbar` wraps, so it rises as the
// window narrows.
const logCard: React.CSSProperties = {
  ...card, marginTop: 0, flex: 1, display: 'flex', flexDirection: 'column',
};
const labelRow: React.CSSProperties = {
  ...cardLabel, display: 'flex', alignItems: 'baseline', gap: 8,
};
// The hint rides the caption row instead of having a heading of its own. The tab
// strip already says "Logs", so the <h2> that used to sit here said it a second
// time; "live" is the part that still earns its place, being what distinguishes
// a stream from a dump of a file.
const labelHint: React.CSSProperties = { textTransform: 'none', letterSpacing: 0 };
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
// The scroller's frame. flex:1 so it absorbs every pixel the toolbar and the
// status line leave, minHeight so a very short window still shows a few lines
// instead of collapsing to a sliver. position:relative anchors the pill, which
// is out of flow and so is unmoved by the column flex.
const scrollerWrap: React.CSSProperties = {
  position: 'relative', flex: 1, minHeight: 140,
  display: 'flex', flexDirection: 'column', marginTop: 10,
};
// Bounded by its SHARE OF THE PANE, not by a fraction of the viewport. The
// reason for bounding it at all is unchanged: the log is the one Manage pane
// whose content is unbounded, and letting it grow the page would put the
// toolbar and the status line off screen exactly when they are needed. But the
// old `maxHeight: '58vh'` bought that by measuring the wrong box — the pane is
// not the viewport, so the cap left dead space below the card and did not move
// when the window was resized. flex:1 + minHeight:0 makes the PANE the bound,
// which is the box the log actually sits in.
const scroller: React.CSSProperties = {
  flex: 1, minHeight: 0, overflowY: 'auto', background: '#101010',
  border: '1px solid #1c1c1c', borderRadius: 4,
};
const pill: React.CSSProperties = {
  position: 'absolute', bottom: 12, left: '50%', transform: 'translateX(-50%)',
  background: '#22303a', color: '#cfe', border: '1px solid #3a4a58',
  borderRadius: 12, padding: '3px 12px', fontSize: 11, cursor: 'pointer',
};
const statusBar: React.CSSProperties = {
  display: 'flex', alignItems: 'center', gap: 12, paddingTop: 8, fontSize: 11, color: '#777',
};
