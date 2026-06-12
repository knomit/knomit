import { useEffect, useState } from 'react';
import { api } from './api';

type AuthMethod = '' | 'ssh' | 'token' | 'basic';

interface Props {
  repo: string;
  readOnly: boolean;
  onConnectAdvanced: (repo: string) => void;
}

// OriginForm is the inline remote-config panel shown in the Repo Manager
// detail pane. It reads the current origin, lets the operator set URL / branch
// / auth, and persists via PUT /origin (which also activates sync). The richer
// multi-step connect-and-reconcile flow is reachable via onConnectAdvanced,
// which the parent opens AFTER closing the manager (never stacked).
export function OriginForm({ repo, readOnly, onConnectAdvanced }: Props) {
  const [url, setUrl] = useState('');
  const [branch, setBranch] = useState('');
  const [authMethod, setAuthMethod] = useState<AuthMethod>('');
  const [token, setToken] = useState('');
  const [user, setUser] = useState('');
  const [password, setPassword] = useState('');
  const [lastSync, setLastSync] = useState<{ at: string | null; status: string | null; error: string | null } | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [msg, setMsg] = useState('');
  const [err, setErr] = useState('');

  useEffect(() => {
    let cancelled = false;
    setLoading(true); setMsg(''); setErr('');
    setUrl(''); setBranch(''); setAuthMethod(''); setToken(''); setUser(''); setPassword('');
    api.getOrigin(repo).then(o => {
      if (cancelled) return;
      if (o) {
        setUrl(o.url || '');
        setBranch(o.branch || '');
        const m = o.auth_method;
        if (m === 'ssh' || m === 'token' || m === 'basic') setAuthMethod(m);
        setLastSync({ at: o.last_sync_at, status: o.last_status, error: o.last_error });
      } else {
        setLastSync(null);
      }
    }).catch(() => { if (!cancelled) setErr('could not load origin'); })
      .finally(() => { if (!cancelled) setLoading(false); });
    return () => { cancelled = true; };
  }, [repo]);

  const save = async () => {
    setErr(''); setMsg(''); setSaving(true);
    try {
      await api.setOrigin(repo, {
        url,
        branch: branch || undefined,
        auth_method: authMethod || undefined,
        token: authMethod === 'token' ? token : undefined,
        user: authMethod === 'basic' ? user : undefined,
        password: authMethod === 'basic' ? password : undefined,
      });
      setMsg('Origin saved — sync activated.');
      setToken(''); setPassword('');
    } catch (e) {
      setErr(String(e));
    } finally {
      setSaving(false);
    }
  };

  if (loading) return <div style={{ color: '#777', fontSize: 13, marginTop: 16 }}>Loading origin…</div>;

  const disabled = readOnly || saving;
  return (
    <div style={{ marginTop: 20 }}>
      <div style={sectionLabel}>Origin</div>
      <label style={label}>Remote URL</label>
      <input style={input} value={url} disabled={disabled} placeholder="git@github.com:me/kb.git"
        onChange={e => setUrl(e.target.value)} />
      <label style={label}>Upstream branch</label>
      <input style={input} value={branch} disabled={disabled} placeholder="main"
        onChange={e => setBranch(e.target.value)} />
      <label style={label}>Auth method</label>
      <select style={input} value={authMethod} disabled={disabled}
        onChange={e => setAuthMethod(e.target.value as AuthMethod)}>
        <option value="">none / inferred</option>
        <option value="ssh">ssh</option>
        <option value="token">token</option>
        <option value="basic">basic</option>
      </select>
      {authMethod === 'token' && (
        <>
          <label style={label}>Token</label>
          <input style={input} type="password" value={token} disabled={disabled}
            placeholder="(unchanged unless set)" onChange={e => setToken(e.target.value)} />
        </>
      )}
      {authMethod === 'basic' && (
        <>
          <label style={label}>User</label>
          <input style={input} value={user} disabled={disabled} onChange={e => setUser(e.target.value)} />
          <label style={label}>Password</label>
          <input style={input} type="password" value={password} disabled={disabled}
            placeholder="(unchanged unless set)" onChange={e => setPassword(e.target.value)} />
        </>
      )}

      {lastSync && (
        <div style={{ fontSize: 12, color: lastSync.status === 'failed' ? '#f88' : '#9c9', marginTop: 8 }}>
          last sync: {lastSync.at ? new Date(lastSync.at).toLocaleString() : 'never'}
          {lastSync.status ? ` · ${lastSync.status}` : ''}
          {lastSync.error ? ` — ${lastSync.error}` : ''}
        </div>
      )}
      {msg && <div style={{ color: '#9c9', fontSize: 13, marginTop: 8 }}>{msg}</div>}
      {err && <div style={{ color: '#f88', fontSize: 13, marginTop: 8 }}>{err}</div>}

      <div style={{ display: 'flex', alignItems: 'center', gap: 12, marginTop: 12 }}>
        <button type="button" style={btn(disabled || !url, 'primary')} disabled={disabled || !url} onClick={save}>
          {saving ? 'Saving…' : 'Save origin'}
        </button>
        <button type="button" style={linkBtn(readOnly)} disabled={readOnly}
          onClick={() => onConnectAdvanced(repo)}>
          Advanced: connect &amp; reconcile a remote →
        </button>
      </div>
    </div>
  );
}

const sectionLabel: React.CSSProperties = { fontSize: 13, color: '#888', textTransform: 'uppercase', borderBottom: '1px solid #222', paddingBottom: 6, marginBottom: 10 };
const label: React.CSSProperties = { fontSize: 12, color: '#888', marginBottom: 4, marginTop: 8, display: 'block' };
const input: React.CSSProperties = { width: '100%', boxSizing: 'border-box', background: '#111', border: '1px solid #333', color: '#eee', padding: '6px 8px', borderRadius: 4, fontSize: 13 };
const btn = (disabled: boolean, variant: 'primary' | 'secondary' = 'secondary'): React.CSSProperties => ({ background: disabled ? '#222' : variant === 'primary' ? '#1d4ed8' : '#2a2a2a', color: disabled ? '#666' : '#eee', border: '1px solid #333', borderRadius: 4, padding: '6px 14px', fontSize: 13, cursor: disabled ? 'default' : 'pointer' });
const linkBtn = (disabled: boolean): React.CSSProperties => ({ background: 'none', border: 'none', color: disabled ? '#555' : '#6cf', fontSize: 12, cursor: disabled ? 'default' : 'pointer', padding: 0, textAlign: 'left' });
