import { Fragment, useCallback, useEffect, useState } from 'react';
import type { CSSProperties } from 'react';
import { api } from './api';
import type { ClientSession, ClientSessionBindingRow, ClientSessionMount, ClientSessionPolicy } from './api';
import { card, cardLabel } from './manageStyles';
import { BookIcon, ChevronDownIcon, FlaskIcon, GitBranchIcon, LayersIcon } from './icons';
import { LENS } from './utils';
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

// Show hidden is a VIEW preference, not state the server owns, so it is
// remembered here rather than round-tripped. Every access is wrapped: a
// blocked or throwing localStorage (private windows, embedded webviews) must
// cost the page the preference and nothing else — the same shape
// repoSelection.ts uses for the last-context and splitter keys.
const SHOW_HIDDEN_KEY = 'knomit.sessions.showHidden';

function readShowHidden(): boolean {
  try { return localStorage.getItem(SHOW_HIDDEN_KEY) === '1'; } catch { return false; }
}
function writeShowHidden(on: boolean): void {
  // '0' rather than removing the key: an explicit "off" has to beat whatever
  // default a later version picks, and a missing key cannot say that.
  try { localStorage.setItem(SHOW_HIDDEN_KEY, on ? '1' : '0'); } catch { /* preference only */ }
}

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

// ── the Binding column ───────────────────────────────────────────────────
//
// A BindingGroup is every handle a session has presented against ONE target.
// The set is keyed by handle server-side, on purpose — two handles naming the
// same repo are two callers, and collapsing them by pin would discard exactly
// the distinction the handle exists to make. This grouping is PRESENTATION
// over that set: the count and the detail row keep every handle reachable, so
// nothing the key separates is lost.
type WriteChip = { branch: string; experiment?: string };
type BindingGroup = {
  key: string; kind: string; uid: string; name: string | null;
  count: number; chips: WriteChip[];
};

const chipKey = (c: WriteChip): string => (c.experiment ? `e:${c.experiment}` : `b:${c.branch}`);

// mountFor finds the URL-scoped mount for a target, matched on KIND AND UID.
// Uid alone would let a repo and a lens that happen to share one put each
// other's experiment on the wrong line.
function mountFor(kind: string, uid: string, mounts: ClientSessionMount[] | undefined): ClientSessionMount | undefined {
  return mounts?.find(m => m.kind === kind && m.uid === uid && m.experiment);
}

// writeChip says what a handle on this target WRITES to, or null when there is
// nothing to say.
//
// THE HANDLE'S OWN BRANCH WINS. kb/invariants/mcp/experiments/one-answer-per-handle
// makes "a branch pin AND an experiment" unrepresentable — a handle carries one
// or the other — so a non-empty `branch` IS this handle's answer, and a mount's
// experiment must not overwrite it. An empty branch means the handle named
// nothing, and there the mount is the only thing that knows: `mounts` is the
// STORED answer for a URL-scoped mount, and a session that opened an experiment
// writes to exp/<name>, which is why
// kb/invariants/mcp/experiments/mount-eligibility-from-the-route has it stored
// rather than re-derived from the URL. Neither → no chip, because "" means the
// target's own branch and a chip saying nothing is worse than no chip.
//
// The mount half is a DISPLAY join, not a routing claim: mounts are per
// URL-scoped mount and bindings are per handle. See
// kb/gotchas/web/ui/manage-page/sessions-binding-groups.
function writeChip(kind: string, uid: string, branch: string, mounts: ClientSessionMount[] | undefined): WriteChip | null {
  if (branch) return { branch };
  const mo = mountFor(kind, uid, mounts);
  return mo ? { branch: mo.branch, experiment: mo.experiment } : null;
}

// targetLines is every line the Binding cell draws for one session: the
// grouped handles, or the singular `binding` fallback when the session
// presented none, PLUS any mount whose target no line already names.
//
// That last part is not cosmetic. The Branch column this replaced rendered
// `r.mounts` UNCONDITIONALLY, so an experiment on a target the session has
// presented no handle for used to be visible and would otherwise now be shown
// nowhere at all.
function targetLines(r: ClientSession): BindingGroup[] {
  const lines = groupBindings(r.bindings, r.mounts);
  if (lines.length === 0) {
    const mo = mountFor(r.binding.kind, r.binding.uid, r.mounts);
    lines.push({
      key: `${r.binding.kind}:${r.binding.uid}`,
      kind: r.binding.kind, uid: r.binding.uid, name: r.binding.name,
      count: 1, chips: mo ? [{ branch: mo.branch, experiment: mo.experiment }] : [],
    });
  }
  for (const mo of r.mounts ?? []) {
    if (!mo.experiment) continue;
    const key = `${mo.kind}:${mo.uid}`;
    if (lines.some(l => l.key === key)) continue;
    // count 0: no handle stands behind this line, so there is no count to show.
    lines.push({
      key, kind: mo.kind, uid: mo.uid, name: mo.name,
      count: 0, chips: [{ branch: mo.branch, experiment: mo.experiment }],
    });
  }
  return lines;
}

function groupBindings(bindings: ClientSessionBindingRow[], mounts: ClientSessionMount[] | undefined): BindingGroup[] {
  // A Map keeps insertion order, which is first appearance, which — because
  // the server returns the set most-recently-used first — puts the target used
  // most recently on top.
  const byTarget = new Map<string, BindingGroup>();
  for (const b of bindings) {
    const key = `${b.kind}:${b.uid}`;
    let g = byTarget.get(key);
    if (!g) { g = { key, kind: b.kind, uid: b.uid, name: b.name, count: 0, chips: [] }; byTarget.set(key, g); }
    g.count++;
    const chip = writeChip(b.kind, b.uid, b.branch, mounts);
    // Distinct branches only: the common case is N handles writing to one
    // branch, and repeating the chip N times would be noise. When they
    // genuinely diverge, both chips show.
    if (chip && !g.chips.some(c => chipKey(c) === chipKey(chip))) g.chips.push(chip);
  }
  return [...byTarget.values()];
}

// The target, with the glyph the top bar already uses for its kind: the book
// for a repo, the stacked planes for a lens. An unresolvable target keeps
// today's grey kind:uid.
function TargetName({ kind, uid, name }: { kind: string; uid: string; name: string | null }) {
  return (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 5 }}>
      {kind === 'repo' && <BookIcon color="#6a8" size={11} />}
      {kind === 'lens' && <LayersIcon color={LENS.accent} size={11} />}
      {/* An unscoped session that has not yet called knomit_bind has NO pin:
          the server's bindingNames.lookup returns empty strings for a value
          that is not a PinID, so kind, uid and name all arrive empty and
          `kind:uid` would render a bare ":". */}
      {name ?? <span style={{ color: '#777' }}>{kind ? `${kind}:${uid}` : '—'}</span>}
    </span>
  );
}

// BranchChip is the top bar's branch button, at table scale: blue fork on
// transparent for an ordinary branch, green flask on #11201a with a #2a4a3a
// border for an experiment. Copied rather than shared — TopBar's own chip
// carries a caret, a drag-region opt-out and a click target that mean nothing
// here, and factoring the two together would drag all of it into this file.
function BranchChip({ branch, experiment }: WriteChip) {
  const exp = Boolean(experiment);
  return (
    <span data-testid="session-branch-chip"
          title={exp ? `Experiment ${experiment}` : branch}
          style={{
            display: 'inline-flex', alignItems: 'center', gap: 4, whiteSpace: 'nowrap',
            fontFamily: 'var(--k-font-mono)', fontSize: 11, lineHeight: 1.5,
            borderRadius: 3, padding: '0 5px',
            color: exp ? '#7c9' : '#8af',
            background: exp ? '#11201a' : 'transparent',
            border: '1px solid ' + (exp ? '#2a4a3a' : 'transparent'),
          }}>
      {exp ? <FlaskIcon color="currentColor" size={10} /> : <GitBranchIcon color="currentColor" size={10} />}
      {exp ? experiment : branch}
    </span>
  );
}

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
  // Read in the initializer so the FIRST fetch already carries it. Reading it
  // in an effect instead would render the unremembered list and correct it,
  // flashing the wrong rows.
  const [showHidden, setShowHidden] = useState(readShowHidden);
  // Which rows have their per-handle detail open, keyed by session id. Per page
  // load, deliberately not persisted — but keyed by id rather than by index, so
  // a poll that reorders or re-fetches the rows does not close what is open or
  // move it onto a different session.
  const [expanded, setExpanded] = useState<Set<string>>(() => new Set());
  const [error, setError] = useState<string | null>(null);
  const [now, setNow] = useState(() => new Date());

  const load = useCallback(() => {
    api.listClientSessions({ includeHidden: showHidden })
      .then(r => {
        setRows(r.sessions); setPolicy(r.policy); setTruncated(r.truncated); setError(null);
        // Drop ids that are no longer on the page. Keying by id is what lets
        // an open row survive a poll, but it also lets a stale id outlive the
        // row it named: a session that leaves the list — purged, or filtered
        // out by a Show hidden change — and later comes back would spring open
        // on its own, having never been asked to. Returning `prev` unchanged
        // when nothing was dropped keeps this off the re-render path.
        setExpanded(prev => {
          if (prev.size === 0) return prev;
          const live = new Set(r.sessions.map(s => s.id));
          const next = new Set([...prev].filter(id => live.has(id)));
          return next.size === prev.size ? prev : next;
        });
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
            <input type="checkbox" aria-label="Show hidden" checked={showHidden}
                   onChange={e => { setShowHidden(e.target.checked); writeShowHidden(e.target.checked); }} />
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
                {/* No Branch column. It showed `r.branch` — the branch the
                    BRIDGE declares about itself in its client header, which it
                    fills only in --repo mode and leaves empty in lens and
                    unscoped mode, and which repeats the binding's own agent
                    branch when it is set. The one live thing it carried was the
                    experiment attribution from `mounts`, and that now sits on
                    the target line, where it can also disagree per target
                    instead of per session. */}
                <th style={th}></th><th style={th}>Client</th><th style={th}>Parent</th><th style={th}>Host · cwd</th><th style={th}>Binding</th><th style={th}>Last seen</th><th style={th}>Requests</th>
              </tr>
            </thead>
            <tbody>
              {rows.map(r => {
                const dead = r.state === 'dead';
                const lines = targetLines(r);
                // The toggle reveals what the grouped line does not carry —
                // and it carries no handle at all, so ONE handle needs it just
                // as much: the handle is what an operator correlating a log
                // line is after. Only a session that presented none has
                // nothing to open.
                const handles = r.bindings.length;
                const expandable = handles >= 1;
                const open = expandable && expanded.has(r.id);
                const detailId = `session-detail-${r.id}`;
                return (
                  <Fragment key={r.id}>
                  <tr data-testid="session-row" style={{ color: dead ? '#666' : '#ddd', borderTop: '1px solid #222' }}>
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
                    {/* One line per distinct TARGET, not per handle. The set is
                        still the whole set — the count says how many handles
                        stand behind each line and the detail row lists them —
                        but a busy agent presents dozens of handles against two
                        repos, and eighteen identical lines said nothing the
                        count does not. Order is first appearance, and bindings
                        arrive most-recently-used first, so the target used most
                        recently is on top.

                        Falls back to the singular `binding` for a session that
                        presented no handle at all (a URL-scoped caller), which
                        is the only case the array is empty — and appends a line
                        for any mount whose target no handle named, which the
                        Branch column used to render unconditionally. */}
                    <td data-testid="session-bindings">
                      <div style={targetsCol}>
                        {lines.map(g => (
                          <div key={g.key} data-testid="session-binding-group" style={targetLine}>
                            <TargetName kind={g.kind} uid={g.uid} name={g.name} />
                            {g.count > 1 && <span style={handleCount}>×{g.count}</span>}
                            {g.chips.map(c => <BranchChip key={chipKey(c)} {...c} />)}
                          </div>
                        ))}
                        {expandable && (
                          <button type="button" data-testid="session-handles-toggle" style={moreButton}
                                  aria-expanded={open} aria-controls={detailId}
                                  onClick={() => setExpanded(prev => {
                                    const next = new Set(prev);
                                    if (!next.delete(r.id)) next.add(r.id);
                                    return next;
                                  })}>
                            <span style={{ display: 'inline-flex', transform: open ? 'rotate(180deg)' : undefined }}>
                              <ChevronDownIcon color="currentColor" size={10} />
                            </span>
                            {handles} {handles === 1 ? 'handle' : 'handles'}
                          </button>
                        )}
                      </div>
                    </td>
                    <td title={r.last_seen_at}>{relativeTime(r.last_seen_at, now)}</td>
                    <td>{r.request_count}</td>
                  </tr>
                  {open && (
                    // Every handle the session has presented, in server order
                    // (most recently used first), with the per-handle ages and
                    // request count the server has always returned and this
                    // table has never shown. The dot column stays empty so the
                    // block hangs under the row it belongs to.
                    <tr data-testid="session-detail" id={detailId}>
                      <td />
                      <td colSpan={6} style={{ padding: '0 0 10px' }}>
                        <div style={handlesBlock}>
                          <table style={{ borderCollapse: 'collapse', fontSize: 11 }}>
                            <thead>
                              <tr>
                                {['Handle', 'Target', 'Writes to', 'First seen', 'Last seen', 'Requests'].map(h => (
                                  <th key={h} style={handlesTh}>{h}</th>
                                ))}
                              </tr>
                            </thead>
                            <tbody>
                              {r.bindings.map((b, i) => {
                                const chip = writeChip(b.kind, b.uid, b.branch, r.mounts);
                                return (
                                  // The first entry is the one that made the
                                  // most recent call; it is brighter because
                                  // that is the one an operator is looking for.
                                  <tr key={b.handle || `${b.kind}:${b.uid}:${i}`} data-testid="session-detail-row"
                                      style={{ color: i === 0 ? '#ddd' : '#888' }}>
                                    <td style={{ ...handlesTd, fontFamily: 'var(--k-font-mono)' }} title={b.handle || undefined}>
                                      {/* Absent on a read-only server, which
                                          redacts it. An em dash, so the column
                                          does not read as an empty value. */}
                                      {b.handle ? <>{b.handle.slice(0, 6)}<span style={{ color: '#555' }}>…</span></> : '—'}
                                    </td>
                                    <td style={handlesTd}>{b.name ?? `${b.kind}:${b.uid}`}</td>
                                    <td style={handlesTd}>{chip ? <BranchChip {...chip} /> : '—'}</td>
                                    <td style={handlesTd} title={b.first_seen_at}>{relativeTime(b.first_seen_at, now)}</td>
                                    <td style={handlesTd} title={b.last_seen_at}>{relativeTime(b.last_seen_at, now)}</td>
                                    <td style={handlesTd}>{b.request_count}</td>
                                  </tr>
                                );
                              })}
                            </tbody>
                          </table>
                        </div>
                      </td>
                    </tr>
                  )}
                  </Fragment>
                );
              })}
              {rows.length === 0 && !error && (
                <tr><td colSpan={7} style={{ color: '#666', padding: 12 }}>No client sessions in the presence window.</td></tr>
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
// One line per target, stacked. Each line wraps rather than overflowing: a
// target with two divergent branch chips is wider than the column on a narrow
// window, and the table's horizontal scroll is for the table, not for a cell.
const targetsCol: React.CSSProperties = { display: 'flex', flexDirection: 'column', gap: 4 };
const targetLine: React.CSSProperties = {
  display: 'flex', alignItems: 'center', gap: 6, flexWrap: 'wrap', whiteSpace: 'nowrap',
};
const handleCount: React.CSSProperties = { color: '#666', fontSize: 11 };
// A real <button>: the row is not a link and the detail is not a navigation,
// so the affordance is the control that announces its own expanded state.
const moreButton: React.CSSProperties = {
  display: 'inline-flex', alignItems: 'center', gap: 4, marginTop: 2,
  background: 'none', border: 0, padding: 0, cursor: 'pointer',
  color: '#888', fontSize: 11, fontFamily: 'inherit',
};
// A left rule rather than indentation alone, so the block reads as belonging to
// the row above it even when the row above has scrolled to the top of the pane.
const handlesBlock: React.CSSProperties = {
  marginLeft: 14, borderLeft: '2px solid #222', padding: '6px 0 2px 12px',
};
const handlesTh: React.CSSProperties = {
  textAlign: 'left', fontWeight: 500, color: '#555',
  textTransform: 'uppercase', letterSpacing: 1, fontSize: 9.5, paddingRight: 18,
};
const handlesTd: React.CSSProperties = { padding: '2px 18px 2px 0', whiteSpace: 'nowrap' };
