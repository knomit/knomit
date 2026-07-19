import { createPortal } from 'react-dom';
import { useEffect, useRef, useState } from 'react';
import ReactMarkdown from 'react-markdown';
import { api, type ArchivedRepo, type RepoInfo, type Lens } from './api';
import { CreateRepoForm } from './CreateRepoForm';
import { CreateLensForm } from './CreateLensForm';
import { RemoteStatus } from './RemoteStatus';
import { RemoteConnectWizard } from './RemoteConnectWizard';
import { BookIcon, ArchiveIcon, PlusIcon, GitBranchIcon } from './icons';

interface Props {
  open: boolean;
  repos: RepoInfo[];
  currentRepo: string;
  readOnly: boolean;
  hideRemoteConfig: boolean;
  onClose: () => void;
  onChanged: () => void;             // parent re-fetches the repo list
}

type Selection =
  | { kind: 'repo'; name: string }
  | { kind: 'archived'; id: string }
  | { kind: 'new' }
  | { kind: 'lens'; name: string }
  | { kind: 'newLens' }
  | null;

export function RepoManager({ open, repos, currentRepo, readOnly, hideRemoteConfig, onClose, onChanged }: Props) {
  const [archived, setArchived] = useState<ArchivedRepo[]>([]);
  const [lenses, setLenses] = useState<Lens[]>([]);
  const [sel, setSel] = useState<Selection>(null);
  const [connecting, setConnecting] = useState<string | null>(null);
  const [err, setErr] = useState('');

  const refresh = () => {
    api.listArchived().then(setArchived).catch(e => setErr(String(e)));
    api.listLenses().then(setLenses).catch(e => setErr(String(e)));
  };

  useEffect(() => {
    if (open) refresh();
  }, [open]);

  if (!open) return null;

  // Connect wizard takes over the whole dialog — its own header/footer.
  if (connecting) {
    return createPortal(
      <div style={overlay} role="dialog" aria-label="Connect remote">
        <div style={panel}>
          <RemoteConnectWizard
            repo={connecting}
            onCancel={() => setConnecting(null)}
            onDone={() => { setConnecting(null); onChanged(); refresh(); }}
          />
        </div>
      </div>,
      document.body,
    );
  }

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
            <div style={sectionHeader}>
              <BookIcon color="#7c9" size={13} />
              <span style={sectionTitle}>Repositories</span>
              <button
                type="button"
                data-testid="repomgr-new"
                title="New repository"
                aria-label="New repository"
                style={plusBtn(readOnly, view.kind === 'new')}
                disabled={readOnly}
                onClick={() => setSel({ kind: 'new' })}
              ><PlusIcon color="currentColor" size={14} /></button>
            </div>
            {repos.map(r => (
              <button
                key={r.name}
                type="button"
                data-testid={`repomgr-item-${r.name}`}
                style={listItem(view.kind === 'repo' && view.name === r.name)}
                onClick={() => setSel({ kind: 'repo', name: r.name })}
              >
                <span>{r.name}</span>
                {r.name === currentRepo && <span style={viewingTag} title="the web UI is currently browsing this repo">viewing</span>}
              </button>
            ))}

            <div style={sectionHeader}>
              <ArchiveIcon color="#8a7" size={13} />
              <span style={sectionTitle}>Archived</span>
            </div>
            {archived.length === 0 && <div style={{ color: '#555', fontSize: 12, padding: '4px 10px' }}>None</div>}
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

            <div style={sectionHeader}>
              <GitBranchIcon color="#9a8" size={13} />
              <span style={sectionTitle}>Lenses</span>
              <button
                type="button"
                data-testid="repomgr-new-lens"
                title="New lens"
                aria-label="New lens"
                style={plusBtn(readOnly, view.kind === 'newLens')}
                disabled={readOnly}
                onClick={() => setSel({ kind: 'newLens' })}
              ><PlusIcon color="currentColor" size={14} /></button>
            </div>
            {lenses.length === 0 && <div style={{ color: '#555', fontSize: 12, padding: '4px 10px' }}>None</div>}
            {lenses.map(l => (
              <button
                key={l.name}
                type="button"
                data-testid={`repomgr-lens-${l.name}`}
                style={listItem(view.kind === 'lens' && view.name === l.name)}
                onClick={() => setSel({ kind: 'lens', name: l.name })}
              >
                {l.name}
              </button>
            ))}
          </nav>

          {/* ── Detail pane ── */}
          <section style={detailCol}>
            {view.kind === 'repo' && (
              <RepoDetail
                key={view.name}
                name={view.name}
                canArchive={!readOnly && view.name !== 'core' && repos.length > 1}
                readOnly={readOnly}
                hideRemoteConfig={hideRemoteConfig}
                onArchived={() => { onChanged(); refresh(); setSel(null); }}
                onConnect={() => setConnecting(view.name)}
                onChanged={onChanged}
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
            {view.kind === 'lens' && (
              <LensDetail
                key={view.name}
                lens={lenses.find(l => l.name === view.name)}
                name={view.name}
                readOnly={readOnly}
                onDeleted={() => { refresh(); setSel(null); }}
                onError={setErr}
              />
            )}
            {view.kind === 'newLens' && (
              <CreateLensForm
                repos={repos}
                lenses={lenses}
                onDone={(name) => { refresh(); setSel({ kind: 'lens', name }); }}
                onCancel={() => setSel({ kind: 'repo', name: currentRepo })}
                onError={setErr}
              />
            )}
          </section>
        </div>
      </div>
    </div>,
    document.body,
  );
}

function RepoDetail({ name, canArchive, readOnly, hideRemoteConfig, onArchived, onConnect, onChanged, onError }: {
  name: string; canArchive: boolean; readOnly: boolean; hideRemoteConfig: boolean;
  onArchived: () => void; onConnect: () => void; onChanged: () => void; onError: (m: string) => void;
}) {
  const [agentBranch, setAgentBranch] = useState('');
  const [description, setDescription] = useState('');
  const [rebuilding, setRebuilding] = useState(false);
  const [rebuildMsg, setRebuildMsg] = useState('');
  const [busy, setBusy] = useState(false);
  const mounted = useRef(true);

  useEffect(() => {
    mounted.current = true;
    let cancelled = false;
    api.getAgentBranch(name).then(b => { if (!cancelled) setAgentBranch(b); }).catch(() => {});
    setDescription('');
    api.getRepo(name).then(r => { if (!cancelled) setDescription(r.description ?? ''); }).catch(() => {});
    return () => { cancelled = true; mounted.current = false; };
  }, [name]);

  // Rebuild is a fire-and-forget background job (TaskHub) — the POST returns as
  // soon as it's queued, the re-index runs server-side. We confirm it started;
  // a 409 means one is already running.
  const rebuild = async () => {
    onError(''); setRebuilding(true); setRebuildMsg('');
    try {
      const branch = agentBranch || await api.getAgentBranch(name);
      await api.rebuild(name, branch);
      setRebuildMsg('✓ Rebuild started — re-indexing in the background.');
      setTimeout(() => { if (mounted.current) setRebuildMsg(''); }, 6000);
    } catch (e) {
      const msg = String(e);
      setRebuildMsg('');
      onError(/409|conflict/i.test(msg) ? 'A rebuild is already running for this repo.' : `rebuild failed: ${msg}`);
    } finally {
      if (mounted.current) setRebuilding(false);
    }
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
          <h3 style={{ margin: 0, fontSize: 16 }}>{name}</h3>
          <div style={{ fontFamily: 'var(--k-font-mono)', fontSize: 12, color: '#777', marginTop: 2 }}>{agentBranch || '…'}</div>
        </div>
      </div>
      {description && <RepoDescription markdown={description} />}
      <div style={{ display: 'flex', gap: 8, marginTop: 14, flexWrap: 'wrap' }}>
        <button type="button" style={btn(readOnly || rebuilding)} disabled={readOnly || rebuilding} onClick={rebuild}>
          {rebuilding ? 'Rebuilding…' : '⟳ Rebuild index'}
        </button>
        <button type="button" style={btn(!canArchive || busy, 'danger')} disabled={!canArchive || busy} onClick={archive}
          title={name === 'core' ? 'the default repo cannot be archived' : undefined}>
          ⌦ Archive
        </button>
      </div>
      {rebuildMsg && (
        <div data-testid="rebuild-status" style={{ fontSize: 12, color: rebuildMsg.startsWith('✓') ? '#9c9' : '#8af', marginTop: 8 }}>{rebuildMsg}</div>
      )}
      {!hideRemoteConfig && (
        <RemoteStatus repo={name} agentBranch={agentBranch} readOnly={readOnly} onConnect={onConnect} onChanged={onChanged} />
      )}
    </div>
  );
}

// RepoDescription renders the repo's kb.md (the API "description") as markdown.
// It is clamped to a few lines by default; if the content overflows, a toggle
// expands it into a fixed-height scrollable panel so the whole manifest is
// readable without taking over the detail pane.
function RepoDescription({ markdown }: { markdown: string }) {
  const [expanded, setExpanded] = useState(false);
  const [overflows, setOverflows] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  // Measure overflow only while collapsed: clientHeight is the clamp height,
  // so scrollHeight > clientHeight means there's more to show. Skip while
  // expanded (the panel scrolls) so `overflows` keeps the toggle visible.
  useEffect(() => {
    if (expanded) return;
    const el = ref.current;
    if (el) setOverflows(el.scrollHeight > el.clientHeight + 1);
  }, [markdown, expanded]);

  return (
    <div data-testid="repo-description" style={descBox}>
      <div style={descLabel}>Description</div>
      <div style={{ position: 'relative' }}>
        <div
          ref={ref}
          style={{
            maxHeight: expanded ? 360 : 132,
            overflowY: expanded ? 'auto' : 'hidden',
            color: '#bbb', fontSize: 13, lineHeight: 1.6,
          }}
        >
          <ReactMarkdown>{markdown}</ReactMarkdown>
        </div>
        {!expanded && overflows && <div style={descFade} />}
      </div>
      {(overflows || expanded) && (
        <button
          type="button"
          data-testid="repo-description-toggle"
          style={descToggle}
          onClick={() => setExpanded(e => !e)}
        >
          {expanded ? 'Show less' : 'Show more'}
        </button>
      )}
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

// LensDetail shows a lens's write target and its read mounts (with branch
// pins), plus a delete button. The lens object is passed from the parent's
// list (already fetched); getLens is used only as a fallback refresh.
function LensDetail({ lens: initial, name, readOnly, onDeleted, onError }: {
  lens?: Lens; name: string; readOnly: boolean;
  onDeleted: () => void; onError: (m: string) => void;
}) {
  const [lens, setLens] = useState<Lens | undefined>(initial);
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState(false);

  useEffect(() => {
    setLens(initial);
    // Fetch full detail if the list entry was thin (e.g. reads missing).
    if (!initial || !initial.reads) {
      let cancelled = false;
      api.getLens(name).then(l => { if (!cancelled) setLens(l); }).catch(() => {});
      return () => { cancelled = true; };
    }
  }, [name, initial]);

  const del = async () => {
    onError(''); setBusy(true);
    try { await api.deleteLens(name); onDeleted(); }
    catch (e) { onError(`delete failed: ${String(e)}`); }
    finally { setBusy(false); setConfirming(false); }
  };

  const reads = lens?.reads ?? [];

  return (
    <div>
      <div style={detailHead}>
        <div>
          <h3 style={{ margin: 0, fontSize: 16 }}>{name}</h3>
          <div style={{ fontSize: 12, color: '#777', marginTop: 2 }}>lens</div>
        </div>
      </div>

      <div style={descBox}>
        <div style={descLabel}>Write</div>
        <div data-testid="lens-detail-write" style={{ color: '#bbb', fontSize: 13 }}>{lens?.write ?? '…'}</div>
      </div>

      <div style={descBox}>
        <div style={descLabel}>Reads</div>
        {reads.length === 0 && <div style={{ color: '#555', fontSize: 13 }}>None</div>}
        {reads.map((r, i) => (
          <div key={`${r.repo}-${i}`} data-testid={`lens-detail-read-${r.repo}`} style={{ color: '#bbb', fontSize: 13, marginTop: i ? 4 : 0 }}>
            {r.repo}
            {r.branch && <span style={{ fontFamily: 'var(--k-font-mono)', color: '#888' }}> · {r.branch}</span>}
            {r.source && <span style={{ color: '#888' }}> ({r.source})</span>}
          </div>
        ))}
      </div>

      {!confirming && (
        <div style={{ display: 'flex', gap: 8, marginTop: 16 }}>
          <button type="button" data-testid="lens-delete" style={btn(readOnly || busy, 'danger')} disabled={readOnly || busy}
            onClick={() => setConfirming(true)}>🗑 Delete lens</button>
        </div>
      )}
      {confirming && (
        <div style={confirmBox}>
          <div style={{ fontSize: 13, marginBottom: 8, color: '#f88' }}>Delete lens “{name}”? The underlying repos are not affected.</div>
          <div style={{ display: 'flex', gap: 8 }}>
            <button type="button" data-testid="lens-delete-confirm" style={btn(busy, 'danger')} disabled={busy} onClick={del}>Confirm delete</button>
            <button type="button" style={btn(busy)} disabled={busy} onClick={() => setConfirming(false)}>Cancel</button>
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
const detailHead: React.CSSProperties = { display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start' };
const sectionHeader: React.CSSProperties = { display: 'flex', alignItems: 'center', gap: 7, padding: '6px 8px 5px', marginTop: 6, borderBottom: '1px solid #242424' };
const sectionTitle: React.CSSProperties = { flex: 1, fontSize: 11, fontWeight: 600, letterSpacing: '0.08em', textTransform: 'uppercase', color: '#9a9a9a' };
const viewingTag: React.CSSProperties = { fontSize: 10, color: '#7c9', letterSpacing: '0.04em' };

const listItem = (active: boolean): React.CSSProperties => ({
  width: '100%', display: 'flex', justifyContent: 'space-between', alignItems: 'center',
  background: active ? '#22303a' : 'transparent', color: active ? '#eee' : '#bbb',
  border: 'none', borderRadius: 4, padding: '7px 10px', fontSize: 13, cursor: 'pointer', textAlign: 'left',
});
const plusBtn = (disabled: boolean, active: boolean): React.CSSProperties => ({
  display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
  width: 22, height: 22, borderRadius: 4,
  background: active ? '#1d4ed8' : 'transparent', color: disabled ? '#555' : active ? '#fff' : '#9a9a9a',
  border: '1px solid ' + (active ? '#1d4ed8' : '#333'), cursor: disabled ? 'default' : 'pointer', padding: 0,
});
const btn = (disabled: boolean, variant: 'primary' | 'secondary' | 'danger' = 'secondary'): React.CSSProperties => ({
  background: disabled ? '#222' : variant === 'primary' ? '#1d4ed8' : variant === 'danger' ? '#7f1d1d' : '#2a2a2a',
  color: disabled ? '#666' : '#eee', border: '1px solid #333', borderRadius: 4, padding: '6px 12px', fontSize: 13, cursor: disabled ? 'default' : 'pointer',
});
const descBox: React.CSSProperties = { marginTop: 14, padding: '10px 12px', background: '#111', border: '1px solid #2a2a2a', borderRadius: 6 };
const descLabel: React.CSSProperties = { fontSize: 10, textTransform: 'uppercase', letterSpacing: 1.5, color: '#555', marginBottom: 6 };
const descFade: React.CSSProperties = { position: 'absolute', left: 0, right: 0, bottom: 0, height: 36, background: 'linear-gradient(transparent, #111)', pointerEvents: 'none' };
const descToggle: React.CSSProperties = { marginTop: 8, background: 'none', border: 'none', color: '#8af', fontSize: 12, cursor: 'pointer', padding: 0 };
const confirmBox: React.CSSProperties = { marginTop: 16, padding: 14, background: '#111', border: '1px solid #333', borderRadius: 6 };
const confirmInput: React.CSSProperties = { width: '100%', boxSizing: 'border-box', background: '#0c0c0c', border: '1px solid #333', color: '#eee', padding: '6px 8px', borderRadius: 4, fontSize: 13 };
