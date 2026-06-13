import { useEffect, useState } from 'react';
import { api, type OriginResponse } from './api';

interface Props {
  repo: string;
  readOnly: boolean;
  onConnect: () => void;   // open the connect wizard
  onChanged: () => void;   // remote changed (e.g. disconnected) — parent refresh
}

// RemoteStatus is the read-only remote panel in the Repo Manager detail pane.
// It never edits the remote inline — connecting/changing always goes through
// the wizard (onConnect); the only inline mutation is Disconnect.
export function RemoteStatus({ repo, readOnly, onConnect, onChanged }: Props) {
  const [origin, setOrigin] = useState<OriginResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');

  useEffect(() => {
    let cancelled = false;
    setLoading(true); setErr(''); setConfirming(false);
    api.getOrigin(repo)
      .then(o => { if (!cancelled) setOrigin(o); })
      .catch(() => { if (!cancelled) setErr('could not load remote status'); })
      .finally(() => { if (!cancelled) setLoading(false); });
    return () => { cancelled = true; };
  }, [repo]);

  const disconnect = async () => {
    setErr(''); setBusy(true);
    try {
      await api.deleteOrigin(repo);
      setOrigin(null); setConfirming(false);
      onChanged();
    } catch (e) { setErr(String(e)); }
    finally { setBusy(false); }
  };

  return (
    <div style={{ marginTop: 20 }}>
      <div style={sectionLabel}>Remote</div>

      {loading && <div style={muted}>Loading…</div>}

      {!loading && !origin && (
        <>
          <div style={muted}>Not connected to a remote.</div>
          <button type="button" data-testid="remote-connect" style={btn(readOnly, 'primary')} disabled={readOnly} onClick={onConnect}>
            Connect a remote…
          </button>
        </>
      )}

      {!loading && origin && (
        <>
          <div style={{ fontSize: 13, color: '#ddd', wordBreak: 'break-all' }}>{origin.url}</div>
          <div style={{ fontSize: 12, color: '#888', marginTop: 2 }}>upstream branch: {origin.branch || 'main'}</div>
          <SyncLine o={origin} />

          {!confirming && (
            <div style={{ display: 'flex', gap: 8, marginTop: 14 }}>
              <button type="button" data-testid="remote-reconnect" style={btn(readOnly)} disabled={readOnly} onClick={onConnect}>Reconnect / change</button>
              <button type="button" data-testid="remote-disconnect" style={btn(readOnly, 'danger')} disabled={readOnly} onClick={() => setConfirming(true)}>Disconnect</button>
            </div>
          )}

          {confirming && (
            <div style={confirmBox}>
              <div style={{ fontSize: 13, marginBottom: 10 }}>Stop syncing and remove this remote? The repo stays as a local-only knowledge base — no facts are deleted.</div>
              <div style={{ display: 'flex', gap: 8 }}>
                <button type="button" data-testid="disconnect-confirm" style={btn(busy, 'danger')} disabled={busy} onClick={disconnect}>{busy ? 'Disconnecting…' : 'Disconnect'}</button>
                <button type="button" style={btn(busy)} disabled={busy} onClick={() => setConfirming(false)}>Cancel</button>
              </div>
            </div>
          )}
        </>
      )}

      {err && <div style={{ color: '#f88', fontSize: 13, marginTop: 8 }}>{err}</div>}
    </div>
  );
}

function SyncLine({ o }: { o: OriginResponse }) {
  let text = 'never synced yet';
  let color = '#888';
  if (o.last_status === 'failed') {
    text = `✗ sync failed${o.last_error ? ' — ' + o.last_error : ''}`;
    color = '#f88';
  } else if (o.last_sync_at) {
    text = `✓ last sync ${new Date(o.last_sync_at).toLocaleString()}`;
    color = '#9c9';
  }
  return <div style={{ fontSize: 12, color, marginTop: 6 }}>{text}</div>;
}

const sectionLabel: React.CSSProperties = { fontSize: 13, color: '#888', textTransform: 'uppercase', borderBottom: '1px solid #222', paddingBottom: 6, marginBottom: 12 };
const muted: React.CSSProperties = { fontSize: 13, color: '#888', marginBottom: 12 };
const confirmBox: React.CSSProperties = { marginTop: 14, padding: 14, background: '#111', border: '1px solid #333', borderRadius: 6 };
const btn = (disabled: boolean, variant: 'primary' | 'secondary' | 'danger' = 'secondary'): React.CSSProperties => ({
  background: disabled ? '#222' : variant === 'primary' ? '#1d4ed8' : variant === 'danger' ? '#7f1d1d' : '#2a2a2a',
  color: disabled ? '#666' : '#eee', border: '1px solid #333', borderRadius: 4, padding: '6px 12px', fontSize: 13, cursor: disabled ? 'default' : 'pointer',
});
