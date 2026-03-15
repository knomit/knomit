import { useState, useEffect } from 'react';
import { api } from './api';
import type { OriginResponse } from './api';

interface Props {
  repo: string;
  onClose: () => void;
}

export function OriginModal({ repo, onClose }: Props) {
  const [origin, setOrigin] = useState<OriginResponse | null | undefined>(undefined);
  const [loading, setLoading] = useState(true);
  const [error] = useState('');

  // Form state
  const [url, setUrl] = useState('');
  const [authMethod, setAuthMethod] = useState<'token' | 'basic' | 'ssh'>('token');
  const [token, setToken] = useState('');
  const [user, setUser] = useState('');
  const [password, setPassword] = useState('');
  const [confirm, setConfirm] = useState('');
  const [submitError, setSubmitError] = useState('');
  const [submitting, setSubmitting] = useState(false);

  useEffect(() => {
    api.getOrigin(repo)
      .then(r => { setOrigin(r); setLoading(false); })
      .catch(() => { setOrigin(null); setLoading(false); });
  }, [repo]);

  const handleSubmit = () => {
    setSubmitError('');
    setSubmitting(true);
    const opts: Parameters<typeof api.setOrigin>[1] = { url };
    if (authMethod === 'token') {
      opts.auth_method = 'token';
      opts.token = token;
    } else if (authMethod === 'basic') {
      opts.auth_method = 'basic';
      opts.user = user;
      opts.password = password;
    } else {
      opts.auth_method = 'ssh';
    }
    api.setOrigin(repo, opts)
      .then(() => {
        setSubmitting(false);
        api.getOrigin(repo).then(r => setOrigin(r)).catch(() => {});
        setConfirm('');
      })
      .catch((e: Error) => {
        setSubmitting(false);
        setSubmitError(e.message || 'Unknown error');
      });
  };

  const overlay: React.CSSProperties = {
    position: 'fixed', top: 0, left: 0, right: 0, bottom: 0,
    background: 'rgba(0,0,0,0.6)', display: 'flex', alignItems: 'center',
    justifyContent: 'center', zIndex: 1000,
  };

  const modal: React.CSSProperties = {
    background: '#0d0d0d', border: '1px solid #222', borderRadius: 8,
    padding: 24, width: 480, maxWidth: '90vw', maxHeight: '80vh',
    overflowY: 'auto', color: '#eee', fontFamily: 'system-ui, sans-serif',
  };

  const label: React.CSSProperties = {
    fontSize: 12, color: '#888', marginBottom: 4, display: 'block',
  };

  const input: React.CSSProperties = {
    width: '100%', boxSizing: 'border-box' as const, background: '#1a1a1a',
    border: '1px solid #333', color: '#eee', padding: '6px 8px',
    borderRadius: 4, fontSize: 13, marginBottom: 12,
  };

  const btn = (disabled: boolean): React.CSSProperties => ({
    padding: '6px 16px', borderRadius: 4, border: 'none', fontSize: 13,
    cursor: disabled ? 'not-allowed' : 'pointer',
    background: disabled ? '#333' : '#2563eb', color: disabled ? '#666' : '#fff',
  });

  return (
    <div style={overlay} onClick={onClose}>
      <div style={modal} onClick={e => e.stopPropagation()}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 16 }}>
          <h2 style={{ margin: 0, fontSize: 16 }}>Origin Configuration</h2>
          <button onClick={onClose} style={{ background: 'none', border: 'none', color: '#666', cursor: 'pointer', fontSize: 16 }}>✕</button>
        </div>

        {loading && <div style={{ color: '#666', fontSize: 13 }}>Loading...</div>}

        {!loading && origin === null && (
          <div style={{ color: '#888', fontSize: 13, marginBottom: 16, padding: '12px', background: '#111', borderRadius: 4 }}>
            No origin configured.
          </div>
        )}

        {!loading && origin && (
          <div style={{ marginBottom: 20, padding: 12, background: '#111', borderRadius: 4, fontSize: 13 }}>
            <div style={{ marginBottom: 6 }}>
              <span style={{ color: '#888' }}>URL: </span>
              <span style={{ color: '#ddd', fontFamily: 'monospace' }}>{origin.url}</span>
            </div>
            <div style={{ marginBottom: 6 }}>
              <span style={{ color: '#888' }}>Branch: </span>
              <span style={{ color: '#ddd' }}>{origin.branch}</span>
            </div>
            <div style={{ marginBottom: 6 }}>
              <span style={{ color: '#888' }}>Sync interval: </span>
              <span style={{ color: '#ddd' }}>{origin.interval}s</span>
              {origin.push_interval > 0 && (
                <span style={{ color: '#888', marginLeft: 12 }}>Push interval: <span style={{ color: '#ddd' }}>{origin.push_interval}s</span></span>
              )}
            </div>
            <div style={{ marginBottom: 4 }}>
              <span style={{ color: '#888' }}>Last sync: </span>
              <span style={{ color: origin.last_status === 'ok' ? '#4caf50' : origin.last_status === 'error' ? '#f44336' : '#888' }}>
                {origin.last_status || 'never'}
              </span>
              {origin.last_sync_at && <span style={{ color: '#666', marginLeft: 8, fontSize: 11 }}>{origin.last_sync_at}</span>}
            </div>
            {origin.last_error && (
              <div style={{ color: '#f44336', fontSize: 11, marginTop: 4 }}>{origin.last_error}</div>
            )}
            {origin.last_push_status && (
              <div style={{ marginTop: 6 }}>
                <span style={{ color: '#888' }}>Last push: </span>
                <span style={{ color: origin.last_push_status === 'ok' ? '#4caf50' : '#f44336' }}>
                  {origin.last_push_status}
                </span>
                {origin.last_push_at && <span style={{ color: '#666', marginLeft: 8, fontSize: 11 }}>{origin.last_push_at}</span>}
                {origin.last_push_error && <div style={{ color: '#f44336', fontSize: 11, marginTop: 2 }}>{origin.last_push_error}</div>}
              </div>
            )}
          </div>
        )}

        {error && <div style={{ color: '#f44336', fontSize: 13, marginBottom: 12 }}>{error}</div>}

        {!loading && (
          <>
            <h3 style={{ fontSize: 14, margin: '0 0 12px', color: '#ccc' }}>Change Origin</h3>

            <label style={label}>Remote URL</label>
            <input style={input} value={url} onChange={e => setUrl(e.target.value)} placeholder="https://github.com/user/repo.git" />

            <label style={label}>Auth Method</label>
            <select
              value={authMethod}
              onChange={e => setAuthMethod(e.target.value as 'token' | 'basic' | 'ssh')}
              style={{ ...input, cursor: 'pointer' }}
            >
              <option value="token">Token</option>
              <option value="basic">Basic (user/password)</option>
              <option value="ssh">SSH</option>
            </select>

            {authMethod === 'token' && (
              <>
                <label style={label}>Token</label>
                <input style={input} type="password" value={token} onChange={e => setToken(e.target.value)} placeholder="ghp_..." />
              </>
            )}

            {authMethod === 'basic' && (
              <>
                <label style={label}>Username</label>
                <input style={input} value={user} onChange={e => setUser(e.target.value)} />
                <label style={label}>Password</label>
                <input style={input} type="password" value={password} onChange={e => setPassword(e.target.value)} />
              </>
            )}

            <div style={{ fontSize: 11, color: '#f9a825', marginBottom: 12, lineHeight: 1.4 }}>
              Warning: Changing the origin will reconfigure the remote for this knowledge base.
              This cannot be easily undone.
            </div>

            <label style={label}>Type "yes" to confirm</label>
            <input style={input} value={confirm} onChange={e => setConfirm(e.target.value)} placeholder="yes" />

            {submitError && <div style={{ color: '#f44336', fontSize: 12, marginBottom: 8 }}>{submitError}</div>}

            <button
              disabled={confirm !== 'yes' || !url || submitting}
              onClick={handleSubmit}
              style={btn(confirm !== 'yes' || !url || submitting)}
            >
              {submitting ? 'Setting...' : 'Set Origin'}
            </button>
          </>
        )}
      </div>
    </div>
  );
}
