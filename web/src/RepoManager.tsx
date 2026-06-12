import { createPortal } from 'react-dom';
import { useEffect, useState } from 'react';
import { api, type ArchivedRepo, type RepoInfo } from './api';
import { CreateRepoForm } from './CreateRepoForm';
import { OriginForm } from './OriginForm';

interface Props {
  open: boolean;
  repos: RepoInfo[];
  currentRepo: string;
  readOnly: boolean;
  onClose: () => void;
  onChanged: () => void;                       // parent re-fetches the repo list
  onSelect: (name: string) => void;            // switch the active repo + close
  onConnectAdvanced: (repo: string) => void;   // close manager, open ConnectRemoteModal
}

type Selection =
  | { kind: 'repo'; name: string }
  | { kind: 'archived'; id: string }
  | { kind: 'new' }
  | null;

export function RepoManager({ open, repos, currentRepo, readOnly, onClose, onChanged, onSelect, onConnectAdvanced }: Props) {
  const [archived, setArchived] = useState<ArchivedRepo[]>([]);
  const [sel, setSel] = useState<Selection>(null);
  const [err, setErr] = useState('');

  const refresh = () => api.listArchived().then(setArchived).catch(e => setErr(String(e)));

  useEffect(() => {
    if (open) refresh();
  }, [open]);

  if (!open) return null;

  // The active selection defaults to the current repo until the user picks
  // something else (derived, not stored, so opening always lands somewhere).
  const view = sel ?? { kind: 'repo' as const, name: currentRepo };
  const selected = archived.find(a => view.kind === 'archived' && a.id === view.id);

  return createPortal(
    <div style={overlay} role="dialog" aria-label="Repo Manager">
      <div style={panel}>
        <header style={head}>
          <h2 style={{ margin: 0, fontSize: 18 }}>Repositories</h2>
          <button type="button" style={closeBtn} onClick={onClose} aria-label="Close">✕</button>
        </header>
        {err && <div style={errBox}>{err}</div>}

        <div style={body}>
          {/* ── Master list ── */}
          <nav style={listCol}>
            <div style={listLabel}>Active</div>
            {repos.map(r => (
              <button
                key={r.name}
                type="button"
                data-testid={`repomgr-item-${r.name}`}
                style={listItem(view.kind === 'repo' && view.name === r.name)}
                onClick={() => setSel({ kind: 'repo', name: r.name })}
              >
                <span>{r.name}</span>
                {r.name === currentRepo && <span style={currentDot} title="active repo">●</span>}
              </button>
            ))}

            <div style={listLabel}>Archived</div>
            {archived.length === 0 && <div style={{ color: '#666', fontSize: 12, padding: '4px 10px' }}>None</div>}
            {archived.map(a => (
              <button
                key={a.id}
                type="button"
                data-testid={`repomgr-archived-${a.id}`}
                style={listItem(view.kind === 'archived' && view.id === a.id)}
                onClick={() => setSel({ kind: 'archived', id: a.id })}
              >
                {a.name}
              </button>
            ))}

            <div style={{ borderTop: '1px solid #222', marginTop: 10, paddingTop: 10 }}>
              <button
                type="button"
                data-testid="repomgr-new"
                style={newBtn(readOnly, view.kind === 'new')}
                disabled={readOnly}
                onClick={() => setSel({ kind: 'new' })}
              >+ New repository</button>
            </div>
          </nav>

          {/* ── Detail pane ── */}
          <section style={detailCol}>
            {view.kind === 'repo' && (
              <RepoDetail
                key={view.name}
                name={view.name}
                isCurrent={view.name === currentRepo}
                canArchive={!readOnly && view.name !== 'trunk' && repos.length > 1}
                readOnly={readOnly}
                onSwitch={() => onSelect(view.name)}
                onArchived={() => { onChanged(); refresh(); setSel(null); }}
                onConnectAdvanced={onConnectAdvanced}
                onError={setErr}
              />
            )}
            {view.kind === 'archived' && selected && (
              <ArchivedDetail
                key={selected.id}
                info={selected}
                readOnly={readOnly}
                activeNames={new Set(repos.map(r => r.name))}
                onRestored={(name) => { onChanged(); refresh(); setSel({ kind: 'repo', name }); }}
                onPurged={() => { refresh(); setSel(null); }}
                onError={setErr}
              />
            )}
            {view.kind === 'new' && (
              <CreateRepoForm
                onDone={(name) => { onChanged(); refresh(); setSel({ kind: 'repo', name }); }}
                onCancel={() => setSel({ kind: 'repo', name: currentRepo })}
              />
            )}
          </section>
        </div>
      </div>
    </div>,
    document.body,
  );
}

function RepoDetail({ name, isCurrent, canArchive, readOnly, onSwitch, onArchived, onConnectAdvanced, onError }: {
  name: string; isCurrent: boolean; canArchive: boolean; readOnly: boolean;
  onSwitch: () => void; onArchived: () => void; onConnectAdvanced: (repo: string) => void; onError: (m: string) => void;
}) {
  const [agentBranch, setAgentBranch] = useState('');
  const [rebuilding, setRebuilding] = useState(false);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let cancelled = false;
    api.getAgentBranch(name).then(b => { if (!cancelled) setAgentBranch(b); }).catch(() => {});
    return () => { cancelled = true; };
  }, [name]);

  const rebuild = async () => {
    onError(''); setRebuilding(true);
    try {
      const branch = agentBranch || await api.getAgentBranch(name);
      await api.rebuild(name, branch);
    } catch (e) { onError(`rebuild failed: ${String(e)}`); }
    finally { setRebuilding(false); }
  };
  const archive = async () => {
    onError(''); setBusy(true);
    try { await api.archiveRepo(name); onArchived(); }
    catch (e) { onError(`archive failed: ${String(e)}`); }
    finally { setBusy(false); }
  };

  return (
    <div>
      <div style={detailHead}>
        <div>
          <h3 style={{ margin: 0, fontSize: 16 }}>{name}{isCurrent && <span style={currentBadge}>active</span>}</h3>
          <div style={{ fontFamily: 'monospace', fontSize: 12, color: '#777', marginTop: 2 }}>{agentBranch || '…'}</div>
        </div>
      </div>
      <div style={{ display: 'flex', gap: 8, marginTop: 14, flexWrap: 'wrap' }}>
        <button type="button" style={btn(readOnly || rebuilding)} disabled={readOnly || rebuilding} onClick={rebuild}>
          {rebuilding ? 'Rebuilding…' : '⟳ Rebuild index'}
        </button>
        <button type="button" style={btn(!canArchive || busy, 'danger')} disabled={!canArchive || busy} onClick={archive}
          title={name === 'trunk' ? 'the default repo cannot be archived' : undefined}>
          ⌦ Archive
        </button>
        {!isCurrent && (
          <button type="button" style={btn(false)} onClick={onSwitch}>Switch to this repo</button>
        )}
      </div>
      <OriginForm repo={name} readOnly={readOnly} onConnectAdvanced={onConnectAdvanced} />
    </div>
  );
}

function ArchivedDetail({ info, readOnly, activeNames, onRestored, onPurged, onError }: {
  info: ArchivedRepo; readOnly: boolean; activeNames: Set<string>;
  onRestored: (name: string) => void; onPurged: () => void; onError: (m: string) => void;
}) {
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState<'restore' | 'purge' | null>(null);
  const [renameTo, setRenameTo] = useState('');
  const [purgeText, setPurgeText] = useState('');

  const nameTaken = activeNames.has(info.name);

  const beginRestore = () => {
    if (nameTaken) { setRenameTo(''); setConfirming('restore'); return; }
    void doRestore('');
  };
  const doRestore = async (newName: string) => {
    onError(''); setBusy(true);
    try {
      const res = await api.restoreRepo(info.id, newName);
      onRestored(res.name);
    } catch (e) { onError(`restore failed: ${String(e)}`); }
    finally { setBusy(false); setConfirming(null); }
  };
  const doPurge = async () => {
    onError(''); setBusy(true);
    try { await api.purgeRepo(info.id); onPurged(); }
    catch (e) { onError(`purge failed: ${String(e)}`); }
    finally { setBusy(false); setConfirming(null); }
  };

  return (
    <div>
      <h3 style={{ margin: 0, fontSize: 16 }}>{info.name}</h3>
      <div style={{ fontSize: 12, color: '#777', marginTop: 4 }}>archived {new Date(info.archivedAt).toLocaleString()}</div>
      <div style={{ fontSize: 13, color: '#aaa', marginTop: 8 }}>origin: {info.origin || '(none)'}</div>

      {confirming === null && (
        <div style={{ display: 'flex', gap: 8, marginTop: 16 }}>
          <button type="button" data-testid="archived-restore" style={btn(readOnly || busy)} disabled={readOnly || busy} onClick={beginRestore}>↺ Restore</button>
          <button type="button" data-testid="archived-purge" style={btn(readOnly || busy, 'danger')} disabled={readOnly || busy} onClick={() => { setPurgeText(''); setConfirming('purge'); }}>🗑 Purge</button>
        </div>
      )}

      {confirming === 'restore' && (
        <div style={confirmBox}>
          <div style={{ fontSize: 13, marginBottom: 8 }}>“{info.name}” is already active. Restore under a new name:</div>
          <input autoFocus data-testid="restore-name-input" style={confirmInput} value={renameTo} placeholder="new repo name"
            onChange={e => setRenameTo(e.target.value)} />
          <div style={{ display: 'flex', gap: 8, marginTop: 10 }}>
            <button type="button" data-testid="restore-confirm" style={btn(busy || !renameTo, 'primary')} disabled={busy || !renameTo} onClick={() => doRestore(renameTo)}>Restore</button>
            <button type="button" style={btn(busy)} disabled={busy} onClick={() => setConfirming(null)}>Cancel</button>
          </div>
        </div>
      )}

      {confirming === 'purge' && (
        <div style={confirmBox}>
          <div style={{ fontSize: 13, marginBottom: 8, color: '#f88' }}>
            This permanently deletes the archived repo and its history. Type <b>{info.name}</b> to confirm:
          </div>
          <input autoFocus data-testid="purge-confirm-input" style={confirmInput} value={purgeText} placeholder={info.name}
            onChange={e => setPurgeText(e.target.value)} />
          <div style={{ display: 'flex', gap: 8, marginTop: 10 }}>
            <button type="button" data-testid="purge-confirm" style={btn(busy || purgeText !== info.name, 'danger')} disabled={busy || purgeText !== info.name} onClick={doPurge}>Confirm purge</button>
            <button type="button" style={btn(busy)} disabled={busy} onClick={() => setConfirming(null)}>Cancel</button>
          </div>
        </div>
      )}
    </div>
  );
}

// ── styles ──
const overlay: React.CSSProperties = { position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.6)', zIndex: 1000, display: 'flex', justifyContent: 'center', alignItems: 'center', padding: 24 };
const panel: React.CSSProperties = { background: '#161616', border: '1px solid #333', borderRadius: 8, width: 'min(900px, 96vw)', height: 'min(620px, 90vh)', color: '#eee', display: 'flex', flexDirection: 'column' };
const head: React.CSSProperties = { display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '14px 18px', borderBottom: '1px solid #222' };
const closeBtn: React.CSSProperties = { background: 'none', border: 'none', color: '#aaa', fontSize: 16, cursor: 'pointer' };
const errBox: React.CSSProperties = { background: '#311', border: '1px solid #533', padding: 10, margin: '10px 18px 0', borderRadius: 4, fontSize: 13 };
const body: React.CSSProperties = { display: 'flex', flex: 1, minHeight: 0 };
const listCol: React.CSSProperties = { width: 230, flexShrink: 0, borderRight: '1px solid #222', padding: 10, overflowY: 'auto' };
const detailCol: React.CSSProperties = { flex: 1, padding: 20, overflowY: 'auto' };
const listLabel: React.CSSProperties = { fontSize: 11, color: '#666', textTransform: 'uppercase', padding: '8px 10px 4px' };
const detailHead: React.CSSProperties = { display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start' };
const currentDot: React.CSSProperties = { color: '#6cf', fontSize: 10 };
const currentBadge: React.CSSProperties = { marginLeft: 8, fontSize: 10, color: '#6cf', border: '1px solid #245', borderRadius: 3, padding: '1px 5px', verticalAlign: 'middle' };

const listItem = (active: boolean): React.CSSProperties => ({
  width: '100%', display: 'flex', justifyContent: 'space-between', alignItems: 'center',
  background: active ? '#22303a' : 'transparent', color: active ? '#eee' : '#bbb',
  border: 'none', borderRadius: 4, padding: '7px 10px', fontSize: 13, cursor: 'pointer', textAlign: 'left',
});
const newBtn = (disabled: boolean, active: boolean): React.CSSProperties => ({
  width: '100%', background: active ? '#1d4ed8' : '#2a2a2a', color: disabled ? '#666' : '#eee',
  border: '1px solid #333', borderRadius: 4, padding: '7px 10px', fontSize: 13, cursor: disabled ? 'default' : 'pointer',
});
const btn = (disabled: boolean, variant: 'primary' | 'secondary' | 'danger' = 'secondary'): React.CSSProperties => ({
  background: disabled ? '#222' : variant === 'primary' ? '#1d4ed8' : variant === 'danger' ? '#7f1d1d' : '#2a2a2a',
  color: disabled ? '#666' : '#eee', border: '1px solid #333', borderRadius: 4, padding: '6px 12px', fontSize: 13, cursor: disabled ? 'default' : 'pointer',
});
const confirmBox: React.CSSProperties = { marginTop: 16, padding: 14, background: '#111', border: '1px solid #333', borderRadius: 6 };
const confirmInput: React.CSSProperties = { width: '100%', boxSizing: 'border-box', background: '#0c0c0c', border: '1px solid #333', color: '#eee', padding: '6px 8px', borderRadius: 4, fontSize: 13 };
