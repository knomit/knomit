import { createPortal } from 'react-dom';
import { useEffect, useState } from 'react';
import { api, type ArchivedRepo, type RepoInfo } from './api';
import { CreateRepoForm } from './CreateRepoForm';

interface Props {
  open: boolean;
  repos: RepoInfo[];
  currentRepo: string;
  readOnly: boolean;
  onClose: () => void;
  onChanged: () => void;
  onSelect: (name: string) => void;
}

export function RepoManager({ open, repos, currentRepo, readOnly, onClose, onChanged, onSelect }: Props) {
  const [archived, setArchived] = useState<ArchivedRepo[]>([]);
  const [creating, setCreating] = useState(false);
  const [err, setErr] = useState('');

  const activeNames = new Set(repos.map(r => r.name));

  const refresh = () => {
    api.listArchived().then(setArchived).catch(e => setErr(String(e)));
  };
  useEffect(() => { if (open) refresh(); }, [open]);

  if (!open) return null;

  const archive = async (name: string) => {
    setErr('');
    try { await api.archiveRepo(name); onChanged(); refresh(); }
    catch (e) { setErr(String(e)); }
  };
  const restore = async (a: ArchivedRepo) => {
    setErr('');
    let newName = '';
    if (activeNames.has(a.name)) {
      newName = window.prompt(`"${a.name}" is taken. New name:`) ?? '';
      if (!newName) return;
    }
    try { await api.restoreRepo(a.id, newName); onChanged(); refresh(); }
    catch (e) { setErr(String(e)); }
  };
  const purge = async (a: ArchivedRepo) => {
    if (window.prompt(`Type "${a.name}" to permanently purge`) !== a.name) return;
    setErr('');
    try { await api.purgeRepo(a.id); refresh(); }
    catch (e) { setErr(String(e)); }
  };

  return createPortal(
    <div style={overlay} role="dialog" aria-label="Repo Manager">
      <div style={panel}>
        <header style={head}>
          <h2 style={{ margin: 0, fontSize: 18 }}>Repositories</h2>
          <button type="button" style={btn(false)} onClick={onClose}>Close</button>
        </header>
        {err && <div style={errBox}>{err}</div>}

        {creating ? (
          <CreateRepoForm
            onDone={() => { setCreating(false); onChanged(); refresh(); }}
            onCancel={() => setCreating(false)}
          />
        ) : (
          <button type="button" style={btn(readOnly, 'primary')} disabled={readOnly} onClick={() => setCreating(true)}>
            + New repository
          </button>
        )}

        <section>
          <h3 style={h3}>Active</h3>
          {repos.map(r => (
            <div key={r.name} style={row}>
              <button type="button" style={nameBtn(r.name === currentRepo)} onClick={() => onSelect(r.name)}>{r.name}</button>
              <button
                type="button"
                style={btn(readOnly || r.name === 'trunk' || repos.length <= 1, 'danger')}
                disabled={readOnly || r.name === 'trunk' || repos.length <= 1}
                onClick={() => archive(r.name)}
              >Archive</button>
            </div>
          ))}
        </section>

        <section>
          <h3 style={h3}>Archived</h3>
          {archived.length === 0 && <div style={{ color: '#777', fontSize: 13 }}>None</div>}
          {archived.map(a => (
            <div key={a.id} style={row}>
              <span>{a.name}</span>
              <span style={{ color: '#777', fontSize: 12 }}>{a.archivedAt}</span>
              <button type="button" style={btn(readOnly)} disabled={readOnly} onClick={() => restore(a)}>Restore</button>
              <button type="button" style={btn(readOnly, 'danger')} disabled={readOnly} onClick={() => purge(a)}>Purge</button>
            </div>
          ))}
        </section>
      </div>
    </div>,
    document.body,
  );
}

const overlay: React.CSSProperties = { position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.6)', zIndex: 1000, display: 'flex', justifyContent: 'center', alignItems: 'flex-start', overflow: 'auto', padding: 24 };
const panel: React.CSSProperties = { background: '#161616', border: '1px solid #333', borderRadius: 8, padding: 20, width: 'min(820px, 95vw)', color: '#eee' };
const head: React.CSSProperties = { display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 12 };
const h3: React.CSSProperties = { fontSize: 13, color: '#888', textTransform: 'uppercase', marginTop: 20 };
const row: React.CSSProperties = { display: 'flex', gap: 12, alignItems: 'center', padding: '8px 0', borderBottom: '1px solid #222' };
const errBox: React.CSSProperties = { background: '#311', border: '1px solid #533', padding: 10, borderRadius: 4, margin: '8px 0', fontSize: 13 };
const nameBtn = (current: boolean): React.CSSProperties => ({ flex: 1, textAlign: 'left', background: 'none', border: 'none', color: current ? '#6cf' : '#eee', fontSize: 14, cursor: 'pointer' });
const btn = (disabled: boolean, variant: 'primary' | 'secondary' | 'danger' = 'secondary'): React.CSSProperties => ({
  background: disabled ? '#222' : variant === 'primary' ? '#1d4ed8' : variant === 'danger' ? '#7f1d1d' : '#2a2a2a',
  color: disabled ? '#666' : '#eee', border: '1px solid #333', borderRadius: 4, padding: '6px 12px', fontSize: 13, cursor: disabled ? 'default' : 'pointer',
});
