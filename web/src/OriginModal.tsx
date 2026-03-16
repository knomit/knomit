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

  // Form state
  const [url, setUrl] = useState('');
  const [authMethod, setAuthMethod] = useState<'token' | 'basic' | 'ssh' | ''>('');
  const [token, setToken] = useState('');
  const [user, setUser] = useState('');
  const [password, setPassword] = useState('');
  const [submitError, setSubmitError] = useState('');
  const [submitting, setSubmitting] = useState(false);

  // Track whether the URL changed (needs confirm)
  const urlChanged = origin ? url !== origin.url : url !== '';

  useEffect(() => {
    api.getOrigin(repo)
      .then(r => {
        setOrigin(r);
        if (r) {
          setUrl(r.url);
          setAuthMethod((r.auth_method || '') as typeof authMethod);
        }
        setLoading(false);
      })
      .catch(() => { setOrigin(null); setLoading(false); });
  }, [repo]);

  const handleSubmit = () => {
    setSubmitError('');
    setSubmitting(true);
    const opts: Parameters<typeof api.setOrigin>[1] = {};
    if (urlChanged) opts.url = url;
    if (authMethod) opts.auth_method = authMethod;
    if (authMethod === 'token' && token) opts.token = token;
    if (authMethod === 'basic') {
      if (user) opts.user = user;
      if (password) opts.password = password;
    }
    api.setOrigin(repo, opts)
      .then(() => { setSubmitting(false); onClose(); })
      .catch((e: Error) => {
        setSubmitting(false);
        setSubmitError(e.message || 'Unknown error');
      });
  };

  const hasChanges = urlChanged || token || user || password ||
    (origin && authMethod !== origin.auth_method);
  const needsUrl = !origin && !url;

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
          <button onClick={onClose} style={{ background: 'none', border: 'none', color: '#666', cursor: 'pointer', fontSize: 16 }}>x</button>
        </div>

        {loading && <div style={{ color: '#666', fontSize: 13 }}>Loading...</div>}

        {!loading && origin && (
          <div style={{ marginBottom: 20, padding: 12, background: '#111', borderRadius: 4, fontSize: 13 }}>
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

        {!loading && (
          <>
            <label style={label}>Remote URL</label>
            <input style={input} value={url} onChange={e => setUrl(e.target.value)} placeholder="git@github.com:user/repo.git" />

            <label style={label}>Auth Method</label>
            <select
              value={authMethod}
              onChange={e => setAuthMethod(e.target.value as typeof authMethod)}
              style={{ ...input, cursor: 'pointer' }}
            >
              <option value="">None</option>
              <option value="ssh">SSH (knomit key)</option>
              <option value="token">Token</option>
              <option value="basic">Basic (user/password)</option>
            </select>

            {authMethod === 'token' && (
              <>
                <label style={label}>Token {origin?.auth_method === 'token' && <span style={{ color: '#666' }}>(leave blank to keep current)</span>}</label>
                <input style={input} type="password" value={token} onChange={e => setToken(e.target.value)} placeholder="ghp_..." />
              </>
            )}

            {authMethod === 'basic' && (
              <>
                <label style={label}>Username {origin?.auth_method === 'basic' && <span style={{ color: '#666' }}>(leave blank to keep current)</span>}</label>
                <input style={input} value={user} onChange={e => setUser(e.target.value)} />
                <label style={label}>Password</label>
                <input style={input} type="password" value={password} onChange={e => setPassword(e.target.value)} />
              </>
            )}

            {submitError && <div style={{ color: '#f44336', fontSize: 12, marginBottom: 8 }}>{submitError}</div>}

            <button
              disabled={!hasChanges || needsUrl || submitting}
              onClick={handleSubmit}
              style={btn(!hasChanges || needsUrl || submitting)}
            >
              {submitting ? 'Saving...' : 'Save'}
            </button>
          </>
        )}
      </div>
    </div>
  );
}
