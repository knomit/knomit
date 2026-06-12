import { useState } from 'react';
import { api, type CreateEvent, type CreateRepoBody } from './api';

type Mode = 'preset' | 'custom' | 'clone';

export function CreateRepoForm({ onDone, onCancel }: { onDone: (name: string) => void; onCancel: () => void }) {
  const [name, setName] = useState('');
  const [mode, setMode] = useState<Mode>('preset');
  const [preset, setPreset] = useState('default');
  const [yaml, setYaml] = useState('');
  const [originUrl, setOriginUrl] = useState('');
  const [branch, setBranch] = useState('');
  const [authMethod, setAuthMethod] = useState('token');
  const [authToken, setAuthToken] = useState('');
  const [events, setEvents] = useState<CreateEvent[]>([]);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');

  const submit = async () => {
    setErr(''); setEvents([]); setBusy(true);
    const body: CreateRepoBody = { name, mode };
    if (mode === 'preset') body.ontology_preset = preset;
    if (mode === 'custom') body.ontology_yaml = yaml;
    if (mode === 'clone') body.origin = { url: originUrl, branch, auth_method: authMethod, auth_token: authToken };
    let failed = false;
    let doneName = name;
    try {
      await api.createRepo(body, (e) => {
        setEvents(prev => [...prev, e]);
        if (e.type === 'done' && e.repo) doneName = e.repo.name;
        if (e.type === 'error') { failed = true; setErr(e.detail || e.title || 'create failed'); }
      });
      if (!failed) onDone(doneName);
    } catch (e) {
      setErr(String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div style={box}>
      <input style={input} placeholder="repo name (a-z0-9-_)" value={name} onChange={e => setName(e.target.value)} disabled={busy} />
      <div style={{ display: 'flex', gap: 8, margin: '8px 0' }}>
        {(['preset', 'custom', 'clone'] as Mode[]).map(mo => (
          <button key={mo} type="button" style={tab(mode === mo)} onClick={() => setMode(mo)} disabled={busy}>{mo}</button>
        ))}
      </div>

      {mode === 'preset' && (
        <select style={input} value={preset} onChange={e => setPreset(e.target.value)} disabled={busy}>
          <option value="default">default</option>
          <option value="code">code</option>
        </select>
      )}
      {mode === 'custom' && (
        <textarea style={{ ...input, height: 160, fontFamily: 'monospace' }} placeholder="ontology.yaml" value={yaml} onChange={e => setYaml(e.target.value)} disabled={busy} />
      )}
      {mode === 'clone' && (
        <>
          <input style={input} placeholder="remote URL" value={originUrl} onChange={e => setOriginUrl(e.target.value)} disabled={busy} />
          <input style={input} placeholder="upstream branch (optional)" value={branch} onChange={e => setBranch(e.target.value)} disabled={busy} />
          <select style={input} value={authMethod} onChange={e => setAuthMethod(e.target.value)} disabled={busy}>
            <option value="token">token</option>
            <option value="basic">basic</option>
            <option value="ssh">ssh</option>
          </select>
          {authMethod !== 'ssh' && (
            <input style={input} type="password" placeholder="token/password" value={authToken} onChange={e => setAuthToken(e.target.value)} disabled={busy} />
          )}
        </>
      )}

      {events.length > 0 && (
        <div style={progress}>
          {events.map((e, i) => (
            <div key={i} style={{ color: e.type === 'error' ? '#f88' : '#9c9' }}>
              {e.type === 'progress' ? `${e.pct ?? 0}% ${e.step ?? ''} — ${e.message ?? ''}` : e.type}
            </div>
          ))}
        </div>
      )}
      {err && <div style={{ color: '#f88', fontSize: 13, marginTop: 8 }}>{err}</div>}

      <div style={{ display: 'flex', gap: 8, marginTop: 12 }}>
        <button type="button" style={btn(busy || !name, 'primary')} disabled={busy || !name} onClick={submit}>Create</button>
        <button type="button" style={btn(busy)} disabled={busy} onClick={onCancel}>Cancel</button>
      </div>
    </div>
  );
}

const box: React.CSSProperties = { border: '1px solid #333', borderRadius: 6, padding: 16, margin: '12px 0', background: '#1a1a1a' };
const input: React.CSSProperties = { width: '100%', boxSizing: 'border-box', background: '#111', border: '1px solid #333', color: '#eee', padding: '6px 8px', borderRadius: 4, fontSize: 13, marginBottom: 6 };
const progress: React.CSSProperties = { marginTop: 10, padding: 10, background: '#0c0c0c', borderRadius: 4, fontSize: 12, fontFamily: 'monospace', maxHeight: 160, overflow: 'auto' };
const tab = (active: boolean): React.CSSProperties => ({ flex: 1, background: active ? '#1d4ed8' : '#2a2a2a', color: '#eee', border: '1px solid #333', borderRadius: 4, padding: '6px 0', fontSize: 13, cursor: 'pointer' });
const btn = (disabled: boolean, variant: 'primary' | 'secondary' = 'secondary'): React.CSSProperties => ({ background: disabled ? '#222' : variant === 'primary' ? '#1d4ed8' : '#2a2a2a', color: disabled ? '#666' : '#eee', border: '1px solid #333', borderRadius: 4, padding: '6px 14px', fontSize: 13, cursor: disabled ? 'default' : 'pointer' });
