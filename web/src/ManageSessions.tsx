import { useCallback, useEffect, useState } from 'react';
import { api } from './api';
import type { ClientSession, ClientSessionPolicy } from './api';
import { card, cardLabel } from './manageStyles';

// ManageSessions lists every MCP client session the server has seen.
// Presence is LAST-SEEN based — the server records each request's time and
// derives live/idle/dead at read time; nothing here is a live connection, and
// nothing on this page holds one open. The page polls: the data's granularity
// is minutes, so 30s is already finer than what it shows. No SSE by design
// (kb/decisions/mcp/client-sessions/liveness-last-seen).

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

export function ManageSessions({ binding }: { binding?: string }) {
  const [rows, setRows] = useState<ClientSession[]>([]);
  const [policy, setPolicy] = useState<ClientSessionPolicy | null>(null);
  const [showHidden, setShowHidden] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [now, setNow] = useState(() => new Date());

  const load = useCallback(() => {
    api.listClientSessions({ binding, includeHidden: showHidden })
      .then(r => { setRows(r.sessions); setPolicy(r.policy); setError(null); })
      .catch(e => setError(String(e)))
      // finally, so the clock advances on EVERY attempt, success or failure.
      // The rows on screen are the last good data either way, so their ages
      // must keep moving while the error banner is up — a frozen "2 min ago"
      // under an error reads as a session that is still fresh. It also keeps
      // this off the synchronous path: a setState in the effect body would
      // cascade a render on every mount.
      .finally(() => setNow(new Date()));
  }, [binding, showHidden]);

  useEffect(() => {
    load();
    const t = setInterval(load, POLL_MS);
    const onFocus = () => load();
    window.addEventListener('focus', onFocus);
    return () => { clearInterval(t); window.removeEventListener('focus', onFocus); };
  }, [load]);

  const live = rows.filter(r => r.state === 'live').length;

  return (
    <div data-testid="manage-sessions">
      <div style={{ display: 'flex', alignItems: 'baseline', gap: 12 }}>
        <h2 style={{ margin: 0, fontSize: 16 }}>Sessions</h2>
        <span style={{ color: '#888', fontSize: 12 }}>{live} live · {rows.length} shown</span>
        <label style={{ marginLeft: 'auto', fontSize: 12, color: '#aaa' }}>
          <input type="checkbox" aria-label="Show hidden" checked={showHidden} onChange={e => setShowHidden(e.target.checked)} />
          {' '}Show hidden
        </label>
      </div>
      {/* The thresholds come from the server with the rows, so this line can
          never disagree with the states above it. */}
      {policy && (
        <p data-testid="session-policy" style={{ color: '#666', fontSize: 11, margin: '6px 0 0' }}>
          live within {Math.round(policy.live_window_s / 60)} min · dead after {Math.round(policy.dead_after_s / 60)} min of silence
          · hidden after {Math.round(policy.hidden_after_s / 3600)} h
          {/* Retention 0 disables the purge entirely — "kept 0 d" would say
              the opposite of what it means. */}
          · {policy.retention_s === 0 ? 'never purged' : `kept ${Math.round(policy.retention_s / 86400)} d`}
        </p>
      )}
      {error && <p data-testid="session-error" style={{ color: '#f87171' }}>{error}</p>}
      <div style={{ ...card, overflowX: 'auto' }}>
        <div style={cardLabel}>MCP clients</div>
        <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 12 }}>
          <thead>
            <tr style={{ color: '#777', textAlign: 'left' }}>
              <th></th><th>Client</th><th>Parent</th><th>Host · cwd</th><th>Binding</th><th>Branch</th><th>Last seen</th><th>Requests</th>
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
                  <td>{r.binding.name ?? <span style={{ color: '#777' }}>{r.binding.kind}:{r.binding.uid}</span>}</td>
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
    </div>
  );
}
