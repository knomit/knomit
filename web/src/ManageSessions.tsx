import { useCallback, useEffect, useState } from 'react';
import type { CSSProperties } from 'react';
import { api } from './api';
import type { ClientSession, ClientSessionPolicy } from './api';
import { card, cardLabel } from './manageStyles';
import { useClientSessionChanges } from './useClientSessionChanges';

// ManageSessions lists every MCP client session the server has seen.
//
// Presence is LAST-SEEN derived — the server records each request's time and
// computes live/idle/dead at read time. No MCP client holds a connection open
// for presence, and kb/decisions/mcp/client-sessions/liveness-last-seen says
// it must stay that way. That decision is about the MCP client; it says
// nothing about the BROWSER, and this page does subscribe to a server-pushed
// change stream, because a session that has already arrived should not wait
// up to 30s to appear. The stream carries "row X changed" and nothing else:
// every render still comes from the list endpoint.
//
// The 30s poll stays. It is the clock — the ages in the table advance with no
// events at all — and it is the fallback while the stream is down.

const POLL_MS = 30_000;

// relativeTime renders an age the way the table reads it. Not exported: this
// module exports components only (react-refresh), and the test reads the
// rendered cell rather than the function.
function relativeTime(iso: string, now: Date): string {
  const s = Math.max(0, Math.round((now.getTime() - new Date(iso).getTime()) / 1000));
  if (s < 60) return `${s}s ago`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m} min ago`;
  const h = Math.round(m / 60);
  if (h < 48) return `${h} h ago`;
  return `${Math.round(h / 24)} d ago`;
}

const STATE_COLOR: Record<ClientSession['state'], string> = { live: '#4ade80', idle: '#facc15', dead: '#555' };

// Amber rather than red: the page is correct, just partial. Red would read as
// a failure and send someone looking for a broken request.
const truncatedNote: CSSProperties = { marginLeft: 8, color: '#facc15', fontSize: 11 };

export function ManageSessions({ onLiveCount }: {
  /** Reports the live count after every poll, so the Manage tab badge can
   *  ride this page's refresh instead of running a second loop.
   *
   *  `truncated` travels with it because the count is computed from a BOUNDED
   *  page: when the server cut the list, the number is a lower bound, not a
   *  total. A consumer that rendered it as exact would be the silent
   *  truncation this page's own note exists to prevent. */
  onLiveCount?: (n: number | null, truncated?: boolean) => void;
}) {
  const [rows, setRows] = useState<ClientSession[]>([]);
  // The server bounds the page. A cut list looks exactly like a complete one,
  // so this is the only thing that can tell a reader there is more.
  const [truncated, setTruncated] = useState(false);
  const [policy, setPolicy] = useState<ClientSessionPolicy | null>(null);
  const [showHidden, setShowHidden] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [now, setNow] = useState(() => new Date());

  const load = useCallback(() => {
    api.listClientSessions({ includeHidden: showHidden })
      .then(r => {
        setRows(r.sessions); setPolicy(r.policy); setTruncated(r.truncated); setError(null);
        onLiveCount?.(r.sessions.filter(s => s.state === 'live').length, r.truncated);
      })
      .catch(e => { setError(String(e)); onLiveCount?.(null, false); })
      // finally, so the clock advances on EVERY attempt, success or failure.
      // The rows on screen are the last good data either way, so their ages
      // must keep moving while the error banner is up — a frozen "2 min ago"
      // under an error reads as a session that is still fresh. It also keeps
      // this off the synchronous path: a setState in the effect body would
      // cascade a render on every mount.
      .finally(() => setNow(new Date()));
  }, [showHidden, onLiveCount]);

  // Live: the server pings when a row changes, and the hook throttles the
  // resulting re-reads.
  useClientSessionChanges(true, load);

  useEffect(() => {
    load();
    const t = setInterval(load, POLL_MS);
    const onFocus = () => load();
    window.addEventListener('focus', onFocus);
    return () => { clearInterval(t); window.removeEventListener('focus', onFocus); };
  }, [load]);

  const live = rows.filter(r => r.state === 'live').length;

  return (
    <div data-testid="manage-sessions" style={page}>
      <div style={sessionsCard}>
        {/* The count and Show hidden ride the card's caption row. The tab strip
            already says "Sessions", so the heading that used to sit above this
            said it twice. Show hidden belongs HERE specifically — it acts on
            the block as a whole, which is the case
            kb/decisions/ui/manage-page/block-affordance-in-heading/b9176d5e.md
            was written for. */}
        <div style={labelRow}>
          MCP clients
          <span style={labelCount}>{live} live · {rows.length} shown</span>
          {/* TRUNCATION IS NOT AN ERROR, but it must not be silent either: the
              page is capped server-side, and a reader who cannot tell a cut
              list from a complete one will read "12 shown" as "12 exist". */}
          {truncated && (
            <span data-testid="session-truncated" style={truncatedNote}>
              showing the {rows.length} most recent of more
            </span>
          )}
          <label style={showHiddenLabel}>
            <input type="checkbox" aria-label="Show hidden" checked={showHidden} onChange={e => setShowHidden(e.target.checked)} />
            Show hidden
          </label>
        </div>
        {error && <p data-testid="session-error" style={errorLine}>{error}</p>}
        {/* A plain block, deliberately NOT a column flex like the Logs
            scroller's wrapper: an auto-height table inside a block wrapper sits
            at the top and keeps its natural row heights, so a handful of rows
            do not stretch to fill the pane. Both axes scroll — the table is
            wider than the pane on a narrow window. */}
        <div style={tableWrap}>
          <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 12 }}>
            <thead>
              {/* Sticky on the CELLS, not on <thead>: cell-level sticky is the
                  broadly supported form, and this also renders in WKWebView via
                  the desktop shell, not only in Chrome. borderCollapse stays
                  'collapse' — under 'separate' a border on a <tr> is not painted
                  at all, which would silently delete every row separator below.
                  So the header's rule is an inset shadow, which travels with the
                  cell the way a collapsed border would not. */}
              <tr style={{ color: '#777', textAlign: 'left' }}>
                <th style={th}></th><th style={th}>Client</th><th style={th}>Parent</th><th style={th}>Host · cwd</th><th style={th}>Binding</th><th style={th}>Branch</th><th style={th}>Last seen</th><th style={th}>Requests</th>
              </tr>
            </thead>
            <tbody>
              {rows.map(r => {
                const dead = r.state === 'dead';
                return (
                  <tr key={r.id} data-testid="session-row" style={{ color: dead ? '#666' : '#ddd', borderTop: '1px solid #222' }}>
                    <td title={r.state}><span style={{ display: 'inline-block', width: 8, height: 8, borderRadius: 4, background: STATE_COLOR[r.state] }} /></td>
                    <td>
                      {r.client.name ? `${r.client.name} ${r.client.version}` : <span style={{ color: '#777' }}>unknown</span>}
                      {/* A row the server never saw initialize: it outlived a
                          restart and was rebuilt from request headers. */}
                      {!r.client.initialized && <span style={{ marginLeft: 6, color: '#a78bfa' }}>resumed</span>}
                      {r.ended_at && <span style={{ marginLeft: 6, color: '#777' }}>ended</span>}
                      <div style={{ color: '#666', fontSize: 11 }}>{r.transport} · {r.user_agent}</div>
                    </td>
                    <td>{r.bridge.parent || '—'}{r.bridge.pid ? <span style={{ color: '#666' }}> #{r.bridge.pid}</span> : null}</td>
                    <td><div>{r.bridge.host || r.remote_addr}</div><div style={{ color: '#666', fontSize: 11 }}>{r.bridge.cwd}</div></td>
                    {/* Every handle the session has presented, most recently
                        used first — NOT just the last one. One session id can
                        serve several concurrent callers, so a single value here
                        would show whichever call landed most recently and hide
                        the rest. Falls back to the singular `binding` for a
                        session that presented no handle at all (a URL-scoped
                        caller), which is the only case the array is empty. */}
                    <td data-testid="session-bindings">
                      {r.bindings.length > 0
                        ? r.bindings.map(b => (
                            <div key={b.handle || `${b.kind}:${b.uid}`} data-testid="session-binding" style={{ whiteSpace: 'nowrap' }}>
                              {b.name ?? <span style={{ color: '#777' }}>{b.kind}:{b.uid}</span>}
                              {/* The handle, shortened. Full value on hover:
                                  it is 32 opaque characters and would dominate
                                  the row, but an operator correlating a log
                                  line needs the whole thing. Absent on a
                                  read-only server, which redacts it. */}
                              {b.handle && (
                                <span title={b.handle} style={{ marginLeft: 6, color: '#666', fontSize: 11 }}>
                                  {b.handle.slice(0, 6)}…
                                </span>
                              )}
                              {b.branch && (
                                <span style={{ marginLeft: 6, color: '#888', fontSize: 11 }}>@{b.branch}</span>
                              )}
                            </div>
                          ))
                        : (r.binding.name ?? (r.binding.kind
                            ? <span style={{ color: '#777' }}>{r.binding.kind}:{r.binding.uid}</span>
                            : <span style={{ color: '#777' }}>—</span>))}
                    </td>
                    <td style={{ color: '#888' }}>{r.branch || '—'}</td>
                    <td title={r.last_seen_at}>{relativeTime(r.last_seen_at, now)}</td>
                    <td>{r.request_count}</td>
                  </tr>
                );
              })}
              {rows.length === 0 && !error && (
                <tr><td colSpan={8} style={{ color: '#666', padding: 12 }}>No client sessions in the presence window.</td></tr>
              )}
            </tbody>
          </table>
        </div>
        {/* The thresholds come from the server with the rows, so this line can
            never disagree with the states above it. Pinned at the card's foot,
            the same role the count line plays on Logs. */}
        {policy && (
          <p data-testid="session-policy" style={policyFooter}>
            live within {Math.round(policy.live_window_s / 60)} min · dead after {Math.round(policy.dead_after_s / 60)} min of silence
            · hidden after {Math.round(policy.hidden_after_s / 3600)} h
            {/* Retention 0 disables the purge entirely — "kept 0 d" would say
                the opposite of what it means. */}
            · {policy.retention_s === 0 ? 'never purged' : `kept ${Math.round(policy.retention_s / 86400)} d`}
          </p>
        )}
      </div>
    </div>
  );
}

// TABLE_FLOOR is the shortest the table area is allowed to get; CARD_FLOOR adds
// the card's own chrome so the card can always draw itself.
//
// Sized for the WORST case — chrome INCLUDING the error line — rather than made
// conditional on `error`. A conditional floor would resize the table at the
// moment an error appears, and that error arrives asynchronously from a failed
// poll, with no user action behind it: the layout would move under someone at
// the instant they started reading why it broke. The corpus settled this same
// tradeoff the same way in
// kb/decisions/ui/summary-panel/dashboard-quick-search/f3e24c49.md (motif
// layout-shifts-under-cursor), which rejected a placement that "pushes the
// whole dashboard down 34px, so the numbers being read move as the reader
// reaches for the box". The precedent is for the direction, not for the cost
// being free: this floor does pay a few unused pixels, and pays them only below
// ~280px of pane height, in a window nobody works in.
//
// The +150 is NOT just the measured chrome, and should not be trimmed to it.
// Measured at 1400px wide, this card's chrome is 73px (card 270 - table area
// 197), or roughly 97 with the error line present. The remaining ~53 is
// deliberate slack: the caption row wraps at narrow widths — the count and Show
// hidden drop below "MCP clients" — and the toolbar-equivalent grows with it,
// so the true chrome rises as the window narrows and a floor cut to 73 would
// stop containing the card exactly there. Logs' own addend (120 against a
// measured 116) is close to its chrome because its caption cannot wrap the same
// way; the two files look like one rule and are not.
const TABLE_FLOOR = 120;
const CARD_FLOOR = TABLE_FLOOR + 150;
// Fills the detail pane rather than sizing to the rows. detailCol is a
// stretched flex item, so its height is definite and this resolves against its
// content box, leaving the pane's own padding outside.
const page: React.CSSProperties = {
  height: '100%', minHeight: CARD_FLOOR, display: 'flex', flexDirection: 'column',
};
// minHeight:0 is load-bearing for the same reason it is on the Logs card: the
// flex default min-height:auto floors a card at its MIN-CONTENT height, and
// min-content propagates up from the content — the wrapper's own minHeight is a
// floor, not a ceiling, so it does not stop the table's full height travelling
// up. Left at auto the card grows to fit every row and the pane scrolls instead
// of the table. The small-window floor lives on the root instead, where it is
// not fighting flex:1 for the same axis.
//
// And the card must never be given an `overflow` of its own. That degradation
// works by letting the card overflow the root so detailCol scrolls it; a scroll
// container here would absorb exactly that overflow, the root would never
// outgrow the pane, and the floor would stop doing anything — besides nesting a
// second scrollbar inside tableWrap's. This is why the `overflowX: 'auto'` that
// used to sit on this card moved into tableWrap rather than staying put.
const sessionsCard: React.CSSProperties = {
  ...card, marginTop: 0, flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column',
};
// flexShrink:0 on all three pieces of chrome, so tableWrap is the only thing
// that gives when the pane is short.
const labelRow: React.CSSProperties = {
  ...cardLabel, display: 'flex', alignItems: 'center', gap: 10, flexShrink: 0,
};
// Not uppercase or letter-spaced: it reads as prose beside the caption, and at
// cardLabel's 10px the count would be unreadable tracked out.
const labelCount: React.CSSProperties = { textTransform: 'none', letterSpacing: 0 };
// The gap is load-bearing, not tidiness, and it is the ONLY thing separating
// the box from its words. This label used to be plain inline text, where the
// `{' '}` between them rendered as a real space. Making it a flex container to
// centre the box against the words took that space away — whitespace between
// flex children is not rendered — and the checkbox has no margin of its own to
// fall back on, because App.css resets `*` to margin:0. So the two collapsed
// together and, in the desktop WebKit view, the native control's box overlapped
// the "S". Removing this gap re-breaks it; there is no second mechanism.
const showHiddenLabel: React.CSSProperties = {
  marginLeft: 'auto', textTransform: 'none', letterSpacing: 0,
  display: 'flex', alignItems: 'center', gap: 6, color: '#aaa',
};
const errorLine: React.CSSProperties = {
  color: '#f87171', fontSize: 12, margin: '6px 0 0', flexShrink: 0,
};
const tableWrap: React.CSSProperties = {
  flex: 1, minHeight: TABLE_FLOOR, overflow: 'auto', marginTop: 6,
};
const th: React.CSSProperties = {
  position: 'sticky', top: 0, background: '#111',
  boxShadow: 'inset 0 -1px 0 #222', padding: '2px 0',
};
const policyFooter: React.CSSProperties = {
  color: '#666', fontSize: 11, margin: '8px 0 0', flexShrink: 0,
};
