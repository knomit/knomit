import { createPortal } from 'react-dom';
import { useEffect, useRef, useState } from 'react';
import ReactMarkdown from 'react-markdown';
import { api, type ArchivedRepo, type RepoInfo, type Lens, type LensRead } from './api';
import { CreateRepoForm } from './CreateRepoForm';
import { CreateLensForm } from './CreateLensForm';
import { RemoteStatus } from './RemoteStatus';
import { RemoteConnectWizard } from './RemoteConnectWizard';
import { LENS, repoHue } from './utils';
import { BookIcon, ArchiveIcon, PlusIcon, GitBranchIcon, LayersIcon, PencilIcon, TrashIcon, CopyIcon } from './icons';

// BrowseContext names the surface a Browse action should switch the app to:
// a whole repo, or a lens's read union. Task 12 consumes this via SET_CONTEXT;
// here RepoManager only fires it through the onBrowse callback.
export type BrowseContext =
  | { kind: 'repo'; repo: string }
  | { kind: 'lens'; name: string };

interface Props {
  open: boolean;
  repos: RepoInfo[];
  currentRepo: string;
  readOnly: boolean;
  hideRemoteConfig: boolean;
  onClose: () => void;
  onChanged: () => void;             // parent re-fetches the repo list
  onBrowse: (ctx: BrowseContext) => void;  // switch the app to browse a repo/lens
}

type Selection =
  | { kind: 'repo'; name: string }
  | { kind: 'archived'; id: string }
  | { kind: 'new' }
  | { kind: 'lens'; name: string }
  | { kind: 'newLens' }
  | null;

export function RepoManager({ open, repos, currentRepo, readOnly, hideRemoteConfig, onClose, onChanged, onBrowse }: Props) {
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
          <h2 style={{ margin: 0, fontSize: 18 }}>Manage</h2>
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
              <LayersIcon color={LENS.accent} size={13} />
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
                <span style={{ display: 'flex', alignItems: 'center', gap: 7 }}>
                  <span style={{ width: 7, height: 7, borderRadius: '50%', background: LENS.accent, flexShrink: 0 }} />
                  {l.name}
                </span>
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
                onBrowse={onBrowse}
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
                repos={repos}
                readOnly={readOnly}
                onDeleted={() => { refresh(); setSel(null); }}
                onSaved={refresh}
                onBrowse={onBrowse}
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

function RepoDetail({ name, canArchive, readOnly, hideRemoteConfig, onArchived, onConnect, onChanged, onBrowse, onError }: {
  name: string; canArchive: boolean; readOnly: boolean; hideRemoteConfig: boolean;
  onArchived: () => void; onConnect: () => void; onChanged: () => void;
  onBrowse: (ctx: BrowseContext) => void; onError: (m: string) => void;
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
        <button type="button" data-testid="repo-browse" style={browseBtn} onClick={() => onBrowse({ kind: 'repo', repo: name })}>
          <BookIcon color={LENS.text} size={13} /> Browse
        </button>
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
// pins), a connect snippet, plus edit/delete actions. The lens object is passed
// from the parent's list (already fetched); getLens refreshes the full detail
// (e.g. to pull the description, which the list view may omit).
//
// The edit-mounts UI is built inline with the same style language as
// CreateLensForm (checkbox rows, LENS tokens) rather than importing its row
// component: that form's rows are tightly coupled to its own reads/branchData
// state, so extraction would force a risky refactor for no shared behavior.
function LensDetail({ lens: initial, name, repos, readOnly, onDeleted, onSaved, onBrowse, onError }: {
  lens?: Lens; name: string; repos: RepoInfo[]; readOnly: boolean;
  onDeleted: () => void; onSaved: () => void; onBrowse: (ctx: BrowseContext) => void; onError: (m: string) => void;
}) {
  const [lens, setLens] = useState<Lens | undefined>(initial);
  const [writeBranch, setWriteBranch] = useState('');
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [copied, setCopied] = useState(false);
  // Edit mode: reads maps a mounted read repo → its branch pin ('' = default).
  // The write repo is never a key (it is read implicitly). null = not editing.
  const [editReads, setEditReads] = useState<Record<string, string> | null>(null);
  const [branchNames, setBranchNames] = useState<Record<string, string[]>>({});

  useEffect(() => {
    setLens(initial);
    setEditReads(null);
    // Always refresh the full detail — the list view can omit the description,
    // and getLens returns the canonical reads set.
    let cancelled = false;
    api.getLens(name).then(l => { if (!cancelled) setLens(l); }).catch(() => {});
    return () => { cancelled = true; };
  }, [name, initial]);

  const write = lens?.write ?? '';
  const reads = lens?.reads ?? [];

  // The write repo's default branch drives the write-target chip and the
  // write row's fallback label when no explicit pin is set.
  useEffect(() => {
    if (!write) return;
    let cancelled = false;
    api.getAgentBranch(write).then(b => { if (!cancelled) setWriteBranch(b); }).catch(() => {});
    return () => { cancelled = true; };
  }, [write]);

  const del = async () => {
    onError(''); setBusy(true);
    try { await api.deleteLens(name); onDeleted(); }
    catch (e) { onError(`delete failed: ${String(e)}`); }
    finally { setBusy(false); setConfirming(false); }
  };

  const copyInit = async () => {
    try {
      await navigator.clipboard.writeText(`knomit init --lens ${name}`);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch { /* clipboard unavailable — nothing to recover */ }
  };

  // ── edit mode ──
  const loadBranches = (repo: string) => {
    if (branchNames[repo]) return;
    api.listBranchNames(repo).then(names => setBranchNames(prev => ({ ...prev, [repo]: names }))).catch(() => {});
  };
  const beginEdit = () => {
    // Seed from the current reads, dropping the write repo (read implicitly).
    const seed: Record<string, string> = {};
    for (const r of reads) if (r.repo !== write) seed[r.repo] = r.branch ?? '';
    setEditReads(seed);
  };
  const toggleRead = (repo: string) => {
    setEditReads(prev => {
      if (!prev) return prev;
      const next = { ...prev };
      if (repo in next) delete next[repo];
      else { next[repo] = ''; loadBranches(repo); }
      return next;
    });
  };
  const setBranch = (repo: string, branch: string) => {
    setEditReads(prev => (prev ? { ...prev, [repo]: branch } : prev));
  };
  const save = async () => {
    if (!editReads) return;
    onError(''); setBusy(true);
    const readList: LensRead[] = Object.entries(editReads)
      .filter(([repo]) => repo !== write)
      .map(([repo, branch]) => branch.trim() ? { repo, branch: branch.trim() } : { repo });
    try {
      const updated = await api.updateLens(name, { reads: readList });
      setLens(updated);
      setEditReads(null);
      onSaved();
    } catch (e) { onError(`save failed: ${String(e)}`); }
    finally { setBusy(false); }
  };

  return (
    <div>
      <div style={detailHead}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
          <span style={lensIconBox}><LayersIcon color={LENS.accent} size={16} /></span>
          <div>
            <h3 style={{ margin: 0, fontSize: 16 }}>{name}</h3>
            <div style={{ fontSize: 12, color: '#777', marginTop: 1 }}>
              lens · {reads.length} mount{reads.length === 1 ? '' : 's'} · writes to {write || '…'}
            </div>
          </div>
        </div>
        <button type="button" data-testid="lens-browse" style={browseBtn} onClick={() => onBrowse({ kind: 'lens', name })}>
          <LayersIcon color={LENS.text} size={13} /> Browse
        </button>
      </div>

      {/* Write target — the one repo new facts land in. */}
      <div style={{ ...descBox, background: '#1a2a1a', borderColor: '#2a4a2a' }}>
        <div style={{ ...descLabel, color: '#6a9a6a' }}>Write target</div>
        <div data-testid="lens-detail-write" style={{ display: 'flex', alignItems: 'center', gap: 8, fontSize: 13, flexWrap: 'wrap' }}>
          <RepoDot repo={write} />
          <b style={{ color: '#eee' }}>{write || '…'}</b>
          <BranchChip branch={reads.find(r => r.repo === write)?.branch || writeBranch || 'agent branch'} />
          <span style={{ color: '#777', fontSize: 12 }}>— all new facts land here</span>
        </div>
      </div>

      {/* Read mounts — the resolved union, in server order. The write repo shows
          here (tagged write · read), never as a separate line. */}
      <div style={descBox}>
        <div style={{ display: 'flex', justifyContent: 'space-between' }}>
          <div style={descLabel}>Read mounts (union)</div>
          <span style={{ fontSize: 11, color: '#777' }}>resolved top → bottom</span>
        </div>
        {reads.length === 0 && <div style={{ color: '#555', fontSize: 13 }}>None</div>}
        {reads.map((r, i) => {
          const isWrite = r.repo === write;
          return (
            <div key={`${r.repo}-${i}`} data-testid={`lens-detail-read-${r.repo}`}
              style={{ display: 'flex', alignItems: 'center', gap: 9, padding: '8px 2px', borderBottom: i === reads.length - 1 ? 'none' : '1px solid #242424' }}>
              <RepoDot repo={r.repo} />
              <span style={{ fontSize: 13, color: '#eee', minWidth: 70 }}>{r.repo}</span>
              <BranchChip branch={r.branch || (isWrite ? writeBranch : '') || 'agent branch'} />
              <div style={{ flex: 1 }} />
              {isWrite
                ? <span style={writeReadTag}>write · read</span>
                : <span style={readTag}>read</span>}
            </div>
          );
        })}
      </div>

      {/* Connect an agent — the init command + MCP endpoint. */}
      <div style={descBox}>
        <div style={descLabel}>Connect an agent</div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 8, background: '#0c0c0c', border: '1px solid #222', borderRadius: 5, padding: '7px 10px' }}>
          <code style={{ fontFamily: 'var(--k-font-mono)', fontSize: 12, color: '#7c9', flex: 1 }}>knomit init --lens {name}</code>
          <button type="button" data-testid="lens-copy" title="Copy" aria-label="Copy init command" style={copyBtn} onClick={copyInit}>
            <CopyIcon color={copied ? '#7c9' : '#888'} size={13} />
          </button>
        </div>
        <div style={{ fontSize: 12, color: '#666', marginTop: 6 }}>
          MCP endpoint <code style={{ fontFamily: 'var(--k-font-mono)', color: '#aaa' }}>/api/v1/lenses/{name}/mcp</code>
        </div>
      </div>

      {lens?.description && (
        <div data-testid="lens-description" style={descBox}>
          <div style={descLabel}>Description</div>
          <div style={{ color: '#bbb', fontSize: 13, lineHeight: 1.6 }}>
            <ReactMarkdown>{lens.description}</ReactMarkdown>
          </div>
        </div>
      )}

      {/* Edit mode: toggle read mounts and pin branches, reusing the lens form's
          checkbox-row language. The write repo is a locked, always-on row. */}
      {editReads && (
        <div style={{ ...descBox, borderColor: LENS.border }}>
          <div style={descLabel}>Edit read mounts</div>
          {write && (
            <div style={editRow(true)}>
              <span style={editCheckbox(true)}><CheckMark color={LENS.text} /></span>
              <RepoDot repo={write} />
              <span style={{ fontSize: 13, color: '#eee', minWidth: 76 }}>{write}</span>
              <span style={writeReadTag}>write · always read</span>
            </div>
          )}
          {repos.filter(r => r.name !== write).map(r => {
            const on = r.name in editReads;
            const others = (branchNames[r.name] ?? []).filter(n => n !== writeBranch);
            return (
              <div key={r.name} style={editRow(on)}>
                <button type="button" data-testid={`lens-read-${r.name}`} style={editCheckbox(on)} disabled={busy}
                  onClick={() => toggleRead(r.name)} aria-label={r.name} aria-pressed={on}>
                  {on && <CheckMark color={LENS.text} />}
                </button>
                <RepoDot repo={r.name} />
                <span style={{ fontSize: 13, color: on ? '#eee' : '#aaa', minWidth: 76 }}>{r.name}</span>
                <div style={{ flex: 1 }} />
                {on && (
                  <select data-testid={`lens-branch-${r.name}`}
                    style={{ background: '#111', border: '1px solid #333', color: '#eee', padding: '5px 7px', borderRadius: 4, fontSize: 12, maxWidth: 200 }}
                    value={editReads[r.name]} disabled={busy} onChange={e => setBranch(r.name, e.target.value)}>
                    <option value="">agent branch (default)</option>
                    {others.map(n => <option key={n} value={n}>{n}</option>)}
                  </select>
                )}
              </div>
            );
          })}
          <div style={{ display: 'flex', gap: 8, marginTop: 12 }}>
            <button type="button" data-testid="lens-edit-save" style={btn(busy, 'primary')} disabled={busy} onClick={save}>
              {busy ? 'Saving…' : 'Save mounts'}
            </button>
            <button type="button" data-testid="lens-edit-cancel" style={btn(busy)} disabled={busy} onClick={() => setEditReads(null)}>Cancel</button>
          </div>
        </div>
      )}

      {!confirming && !editReads && (
        <div style={{ display: 'flex', gap: 8, marginTop: 16 }}>
          <button type="button" data-testid="lens-edit" style={{ ...btn(readOnly || busy), display: 'flex', alignItems: 'center', gap: 6 }}
            disabled={readOnly || busy} onClick={beginEdit}>
            <PencilIcon color={readOnly || busy ? '#666' : '#bbb'} size={13} /> Edit mounts
          </button>
          <button type="button" data-testid="lens-delete" style={{ ...btn(readOnly || busy, 'danger'), display: 'flex', alignItems: 'center', gap: 6 }}
            disabled={readOnly || busy} onClick={() => setConfirming(true)}>
            <TrashIcon color={readOnly || busy ? '#666' : '#f88'} size={13} /> Delete
          </button>
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

// RepoDot is the deterministic per-repo hue swatch (mirrors CreateLensForm's Dot).
const RepoDot = ({ repo }: { repo: string }) => (
  <span style={{ width: 8, height: 8, borderRadius: '50%', background: repoHue(repo || '?'), flexShrink: 0 }} />
);

// BranchChip renders a blue git-branch chip with a mono branch label.
const BranchChip = ({ branch }: { branch: string }) => (
  <span style={{ display: 'flex', alignItems: 'center', gap: 4, fontSize: 11.5, color: '#8af' }}>
    <GitBranchIcon color="#8af" size={11} />
    <span style={{ fontFamily: 'var(--k-font-mono)' }}>{branch}</span>
  </span>
);

// CheckMark is the small inline check glyph for a filled edit checkbox.
const CheckMark = ({ color }: { color: string }) => (
  <svg width={11} height={11} viewBox="0 0 24 24" fill="none" stroke={color} strokeWidth="3" strokeLinecap="round" strokeLinejoin="round">
    <polyline points="20 6 9 17 4 12" />
  </svg>
);

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

// browseBtn is the lens-accent "Browse" pill, shared by the lens and repo detail
// panes (design handoff: LENS.accent fill, LENS.text text, 13px/600, radius 5).
const browseBtn: React.CSSProperties = {
  display: 'flex', alignItems: 'center', gap: 6, flexShrink: 0,
  background: LENS.accent, color: LENS.text, border: 'none', borderRadius: 5,
  padding: '7px 12px', fontSize: 13, fontWeight: 600, cursor: 'pointer',
};
const lensIconBox: React.CSSProperties = {
  width: 30, height: 30, borderRadius: 7, flexShrink: 0,
  background: LENS.bg, border: '1px solid ' + LENS.border,
  display: 'flex', alignItems: 'center', justifyContent: 'center',
};
const copyBtn: React.CSSProperties = { background: 'none', border: 'none', padding: 2, cursor: 'pointer', display: 'flex', alignItems: 'center' };
const writeReadTag: React.CSSProperties = { fontSize: 10, color: '#7c9', background: '#1a2e1a', border: '1px solid #2a4a2a', padding: '1px 7px', borderRadius: 3, letterSpacing: '0.03em' };
const readTag: React.CSSProperties = { fontSize: 10, color: '#777', border: '1px solid #333', padding: '1px 7px', borderRadius: 3, letterSpacing: '0.03em' };
// editRow / editCheckbox mirror CreateLensForm's row/checkbox (LENS tokens).
const editRow = (on: boolean): React.CSSProperties => ({
  display: 'flex', alignItems: 'center', gap: 10, padding: '8px 10px', borderRadius: 6, marginBottom: 4,
  background: on ? LENS.soft : '#0f0f0f', border: '1px solid ' + (on ? LENS.border : '#242424'),
});
const editCheckbox = (on: boolean): React.CSSProperties => ({
  width: 16, height: 16, borderRadius: 4, flexShrink: 0, padding: 0,
  display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
  background: on ? LENS.accent : 'transparent', border: '1.5px solid ' + (on ? LENS.accent : '#444'),
  cursor: 'pointer',
});
