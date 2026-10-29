import { Fragment, useState } from 'react';
import type { CSSProperties } from 'react';
import type { ClientSession, ClientSessionBindingRow, ClientSessionPolicy } from './api';
import { BookIcon, ChevronDownIcon, GitBranchIcon, LayersIcon } from './icons';
import { LENS } from './utils';
import type { SessionTarget } from './useTargetSessions';
import { tableWrap, th, policyFooter } from './tableStyles';

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
type WriteChip = { branch: string };
type BindingGroup = {
  key: string; kind: string; uid: string; name: string | null;
  count: number; chips: WriteChip[];
};

// writeChip says what a handle on this target WRITES to: its own branch pin,
// or null when it named none ("" means the target's own branch, and a chip
// saying nothing is worse than no chip).
function writeChip(branch: string): WriteChip | null {
  return branch ? { branch } : null;
}

// targetLines is every line the Binding cell draws for one session: the
// grouped handles, or the singular `binding` fallback when the session
// presented none.
function targetLines(r: ClientSession): BindingGroup[] {
  const lines = groupBindings(r.bindings);
  if (lines.length === 0) {
    lines.push({
      key: `${r.binding.kind}:${r.binding.uid}`,
      kind: r.binding.kind, uid: r.binding.uid, name: r.binding.name,
      count: 1, chips: [],
    });
  }
  return lines;
}

function groupBindings(bindings: ClientSessionBindingRow[]): BindingGroup[] {
  // A Map keeps insertion order, which is first appearance, which — because
  // the server returns the set most-recently-used first — puts the target used
  // most recently on top.
  const byTarget = new Map<string, BindingGroup>();
  for (const b of bindings) {
    const key = `${b.kind}:${b.uid}`;
    let g = byTarget.get(key);
    if (!g) { g = { key, kind: b.kind, uid: b.uid, name: b.name, count: 0, chips: [] }; byTarget.set(key, g); }
    g.count++;
    const chip = writeChip(b.branch);
    // Distinct branches only: the common case is N handles writing to one
    // branch, and repeating the chip N times would be noise. When they
    // genuinely diverge, both chips show.
    if (chip && !g.chips.some(c => c.branch === chip.branch)) g.chips.push(chip);
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
      {/* Every line drawn is the target's own (an unbound session, whose pin
          arrives empty, never matches a target's binding filter), so kind and
          uid are always there to fall back to. */}
      {name ?? <span style={{ color: '#777' }}>{`${kind}:${uid}`}</span>}
    </span>
  );
}

// BranchChip is the top bar's branch button, at table scale: blue fork on
// transparent. Copied rather than shared — TopBar's own chip carries a caret, a
// drag-region opt-out and a click target that mean nothing here, and factoring
// the two together would drag all of it into this file.
function BranchChip({ branch }: WriteChip) {
  return (
    <span data-testid="session-branch-chip" title={branch}
          style={{
            display: 'inline-flex', alignItems: 'center', gap: 4, whiteSpace: 'nowrap',
            fontFamily: 'var(--k-font-mono)', fontSize: 11, lineHeight: 1.5,
            borderRadius: 3, padding: '0 5px',
            color: '#8af', background: 'transparent', border: '1px solid transparent',
          }}>
      <GitBranchIcon color="currentColor" size={10} />
      {branch}
    </span>
  );
}


// SessionsTable is the client-session table: one row per session, the
// binding groups, the per-handle detail and the policy line as its footer. It
// has no card and no caption: the Activity facet puts the counts in its
// toolbar.
//
// It is a flex column that fills its parent with minHeight 0 and scrolls in
// its own table wrapper, so it never grows to its rows' min-content height and
// pushes the footer off the window
// (kb/gotchas/web/ui/manage-page/flex-min-content-propagation).
//
// Only the target's binding groups are drawn: the server already filtered the
// sessions to ones bound to it, and a session bound here AND elsewhere shows
// just this target's line and handles.
export function SessionsTable({ rows, policy, error, now, target }: {
  rows: ClientSession[];
  policy: ClientSessionPolicy | null;
  error: string | null;
  now: Date;
  target: SessionTarget;
}) {
  const targetKey = `${target.kind}:${target.uid}`;
  // Which rows have their per-handle detail open, keyed by session id. Per page
  // load, deliberately not persisted — but keyed by id rather than by index, so
  // a poll that reorders or re-fetches the rows does not close what is open or
  // move it onto a different session.
  const [expanded, setExpanded] = useState<Set<string>>(() => new Set());
  // Drop ids that are no longer on the page, whenever a new page arrives.
  // Keying by id is what lets an open row survive a poll, but it also lets a
  // stale id outlive the row it named: a session that leaves the list —
  // purged, or filtered out by a Show hidden change — and later comes back
  // would spring open on its own, having never been asked to. Adjusted during
  // render (React's "state from props" pattern) rather than in an effect, and
  // left untouched when nothing was dropped.
  const [seenRows, setSeenRows] = useState(rows);
  if (seenRows !== rows) {
    setSeenRows(rows);
    if (expanded.size > 0) {
      const live = new Set(rows.map(s => s.id));
      const next = new Set([...expanded].filter(id => live.has(id)));
      if (next.size !== expanded.size) setExpanded(next);
    }
  }

  return (
    <div data-testid="sessions-table" style={root}>
      {error && <p data-testid="session-error" style={errorLine}>{error}</p>}
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
                  branch when it is set. */}
              <th style={th}></th><th style={th}>Client</th><th style={th}>Parent</th><th style={th}>Host · cwd</th><th style={th}>Binding</th><th style={th}>Last seen</th><th style={th}>Requests</th>
            </tr>
          </thead>
          <tbody>
            {rows.map(r => {
              const dead = r.state === 'dead';
              // Filtered to the target: a session bound here AND elsewhere shows
              // only this target's line, because the page is about this target.
              const lines = targetLines(r).filter(l => l.key === targetKey);
              const bindings = r.bindings.filter(b => `${b.kind}:${b.uid}` === targetKey);
              // The toggle reveals what the grouped line does not carry —
              // and it carries no handle at all, so ONE handle needs it just
              // as much: the handle is what an operator correlating a log
              // line is after. Only a session that presented none has
              // nothing to open.
              const handles = bindings.length;
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
                      is the only case the array is empty. */}
                  <td data-testid="session-bindings">
                    <div style={targetsCol}>
                      {lines.map(g => (
                        <div key={g.key} data-testid="session-binding-group" style={targetLine}>
                          <TargetName kind={g.kind} uid={g.uid} name={g.name} />
                          {g.count > 1 && <span style={handleCount}>×{g.count}</span>}
                          {g.chips.map(c => <BranchChip key={c.branch} {...c} />)}
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
                            {bindings.map((b, i) => {
                              const chip = writeChip(b.branch);
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
  );
}

const root: CSSProperties = { flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column' };
const errorLine: CSSProperties = {
  color: '#f87171', fontSize: 12, margin: '6px 0 0', flexShrink: 0, padding: '0 12px',
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
