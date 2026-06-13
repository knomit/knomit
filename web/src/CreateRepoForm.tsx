import { useState } from 'react';
import { api, type CreateEvent, type CreateRepoBody } from './api';

type Mode = 'preset' | 'custom' | 'clone';

const MODE_LABEL: Record<Mode, string> = {
  preset: 'Preset ontology',
  custom: 'Custom ontology',
  clone: 'Clone remote',
};

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
    <div>
      <h3 style={{ margin: '0 0 14px', fontSize: 16 }}>New repository</h3>

      <label style={label}>Name</label>
      <input data-testid="create-name" style={input} placeholder="e.g. work (a–z, 0–9, -, _)" value={name} disabled={busy}
        onChange={e => setName(e.target.value)} />

      <label style={label}>Create from</label>
      <div style={{ display: 'flex', gap: 8, marginBottom: 4 }}>
        {(['preset', 'custom', 'clone'] as Mode[]).map(mo => (
          <button key={mo} type="button" style={tab(mode === mo)} onClick={() => setMode(mo)} disabled={busy}>
            {MODE_LABEL[mo]}
          </button>
        ))}
      </div>

      {mode === 'preset' && (
        <>
          <label style={label}>Ontology preset</label>
          <select style={input} value={preset} onChange={e => setPreset(e.target.value)} disabled={busy}>
            <option value="default">default — general knowledge base</option>
            <option value="code">code — source-code knowledge base</option>
          </select>
          <div style={hint}>The ontology defines the starting set of topics and rules for the repo.</div>
        </>
      )}
      {mode === 'custom' && (
        <>
          <label style={label}>Ontology (YAML)</label>
          <textarea style={{ ...input, height: 160, fontFamily: 'monospace' }} placeholder="id: my-kb&#10;name: My KB&#10;topics:&#10;  ..." value={yaml} disabled={busy}
            onChange={e => setYaml(e.target.value)} />
          <div style={hint}>Paste a custom ontology YAML to define topics and rules.</div>
        </>
      )}
      {mode === 'clone' && (
        <>
          <label style={label}>Remote URL</label>
          <input style={input} placeholder="git@github.com:me/kb.git" value={originUrl} disabled={busy}
            onChange={e => setOriginUrl(e.target.value)} />
          <label style={label}>Upstream branch (optional)</label>
          <input style={input} placeholder="main" value={branch} disabled={busy}
            onChange={e => setBranch(e.target.value)} />
          <label style={label}>Auth method</label>
          <select style={input} value={authMethod} onChange={e => setAuthMethod(e.target.value)} disabled={busy}>
            <option value="token">token</option>
            <option value="basic">basic</option>
            <option value="ssh">ssh</option>
          </select>
          {authMethod !== 'ssh' && (
            <>
              <label style={label}>Token / password</label>
              <input style={input} type="password" placeholder="••••••••" value={authToken} disabled={busy}
                onChange={e => setAuthToken(e.target.value)} />
            </>
          )}
          <div style={hint}>The ontology is taken from the remote repo.</div>
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

      <div style={{ display: 'flex', gap: 8, marginTop: 14 }}>
        <button type="button" style={btn(busy || !name, 'primary')} disabled={busy || !name} onClick={submit}>
          {busy ? 'Creating…' : 'Create'}
        </button>
        <button type="button" style={btn(busy)} disabled={busy} onClick={onCancel}>Cancel</button>
      </div>
    </div>
  );
}

const label: React.CSSProperties = { fontSize: 12, color: '#888', marginBottom: 4, marginTop: 12, display: 'block' };
const hint: React.CSSProperties = { fontSize: 12, color: '#666', marginTop: 4 };
const input: React.CSSProperties = { width: '100%', boxSizing: 'border-box', background: '#111', border: '1px solid #333', color: '#eee', padding: '6px 8px', borderRadius: 4, fontSize: 13 };
const progress: React.CSSProperties = { marginTop: 12, padding: 10, background: '#0c0c0c', borderRadius: 4, fontSize: 12, fontFamily: 'monospace', maxHeight: 160, overflow: 'auto' };
const tab = (active: boolean): React.CSSProperties => ({ flex: 1, background: active ? '#1d4ed8' : '#2a2a2a', color: '#eee', border: '1px solid #333', borderRadius: 4, padding: '7px 0', fontSize: 13, cursor: 'pointer' });
const btn = (disabled: boolean, variant: 'primary' | 'secondary' = 'secondary'): React.CSSProperties => ({ background: disabled ? '#222' : variant === 'primary' ? '#1d4ed8' : '#2a2a2a', color: disabled ? '#666' : '#eee', border: '1px solid #333', borderRadius: 4, padding: '6px 14px', fontSize: 13, cursor: disabled ? 'default' : 'pointer' });
