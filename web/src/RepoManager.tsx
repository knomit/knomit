import { createPortal } from 'react-dom';
import { useEffect, useRef, useState } from 'react';
import ReactMarkdown from 'react-markdown';
import { api, MAX_LENS_DESCRIPTION_BYTES, MAX_REPO_DESCRIPTION_BYTES, type ArchivedRepo, type RepoInfo, type Lens, type LensRead } from './api';
import { CreateRepoForm } from './CreateRepoForm';
import { markdownPlugins, markdownComponents } from './markdown';
import { CreateLensForm } from './CreateLensForm';
import { RemoteCard } from './RemoteStatus';
import { useRemote } from './useRemote';
import { RemoteConnectWizard } from './RemoteConnectWizard';
import { LENS, repoHue, repoHueBg, repoHueBorder } from './utils';
import { BookIcon, ArchiveIcon, PlusIcon, GitBranchIcon, LayersIcon, PencilIcon, CopyIcon, MoreVerticalIcon, ChevronDownIcon } from './icons';
import { btn, card, cardIconBtn, cardLabel, confirmBox, confirmInput, writeCard, writeCardLabel } from './manageStyles';
import type { BrowseContext } from './state';

// BrowseContext names the surface a Browse action should switch the app to:
// a whole repo, or a lens's read union. The canonical definition lives in
// state.ts (SET_CONTEXT consumes it); re-exported here for existing importers.
export type { BrowseContext };

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
                onDeleted={() => { onChanged(); refresh(); setSel(null); }}
                onSaved={() => { onChanged(); refresh(); }}
                onBrowse={onBrowse}
                onError={setErr}
              />
            )}
            {view.kind === 'newLens' && (
              <CreateLensForm
                repos={repos}
                lenses={lenses}
                onDone={(name) => { onChanged(); refresh(); setSel({ kind: 'lens', name }); }}
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
  // license is read-only: set once from the GET response, never written back.
  const [license, setLicense] = useState('');
  const [rebuilding, setRebuilding] = useState(false);
  const [rebuildMsg, setRebuildMsg] = useState('');
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState<'disconnect' | null>(null);
  // The detail pane owns the remote so its ⋯ menu can offer Connect vs
  // Reconnect/Disconnect; RemoteCard is the display half of the same state.
  const remote = useRemote(name, !hideRemoteConfig);

  useEffect(() => {
    let cancelled = false;
    api.getAgentBranch(name).then(b => { if (!cancelled) setAgentBranch(b); }).catch(() => {});
    setDescription('');
    setLicense('');
    api.getRepo(name).then(r => {
      if (cancelled) return;
      setDescription(r.description ?? '');
      setLicense(r.license ?? '');
    }).catch(() => {});
    return () => { cancelled = true; };
  }, [name]);

  // The "rebuild started" banner clears itself after a beat. Owning the timer
  // in an effect rather than a setTimeout guarded by a mounted ref means
  // unmounting cancels it for free — and keeps refs out of the render path.
  useEffect(() => {
    if (!rebuildMsg.startsWith('✓')) return;
    const id = setTimeout(() => setRebuildMsg(''), 6000);
    return () => clearTimeout(id);
  }, [rebuildMsg]);

  // Rebuild is a fire-and-forget background job (TaskHub) — the POST returns as
  // soon as it's queued, the re-index runs server-side. We confirm it started;
  // a 409 means one is already running.
  const rebuild = async () => {
    onError(''); setRebuilding(true); setRebuildMsg('');
    try {
      const branch = agentBranch || await api.getAgentBranch(name);
      await api.rebuild(name, branch);
      setRebuildMsg('✓ Rebuild started — re-indexing in the background.');
    } catch (e) {
      const msg = String(e);
      setRebuildMsg('');
      onError(/409|conflict/i.test(msg) ? 'A rebuild is already running for this repo.' : `rebuild failed: ${msg}`);
    } finally {
      setRebuilding(false);
    }
  };
  const archive = async () => {
    onError(''); setBusy(true);
    try { await api.archiveRepo(name); onArchived(); }
    catch (e) { onError(`archive failed: ${String(e)}`); }
    finally { setBusy(false); }
  };
  const disconnect = async () => {
    onError(''); setBusy(true);
    try { await api.deleteOrigin(name); remote.reload(); setConfirming(null); onChanged(); }
    catch (e) { onError(`disconnect failed: ${String(e)}`); }
    finally { setBusy(false); }
  };

  // The ⋯ menu holds WHOLE-REPO actions. Actions that edit one card's data
  // (reconnect/disconnect the remote) live on that card instead. "Connect a
  // remote" is here rather than as a permanent button because an unconnected
  // repo renders no Remote card at all — there is no remote state to show.
  //
  // remote.err is a THIRD state, distinct from "not connected": the request
  // failed, so we do not know whether a remote exists. Offering Connect there
  // would invite the user to overwrite a remote that is merely unreadable.
  const menuItems: MenuItem[] = [{
    label: rebuilding ? 'Rebuilding…' : 'Rebuild index', testid: 'repo-rebuild',
    disabled: readOnly || rebuilding, onSelect: rebuild,
  }];
  if (!hideRemoteConfig && !remote.loading && !remote.origin && !remote.err) {
    menuItems.push({ label: 'Connect a remote…', testid: 'remote-connect', disabled: readOnly, onSelect: onConnect });
  }
  menuItems.push({ separator: true });
  menuItems.push({
    label: 'Archive', testid: 'repo-archive', danger: true, disabled: !canArchive || busy,
    hint: name === 'core' ? 'the default repo cannot be archived' : undefined,
    onSelect: archive,
  });

  return (
    <div>
      <div style={detailHead}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, minWidth: 0 }}>
          <span style={repoIconBox(name)}><BookIcon color={repoHue(name)} size={16} /></span>
          <div style={{ minWidth: 0 }}>
            <h3 style={{ margin: 0, fontSize: 16 }}>{name}</h3>
            <div style={{ fontSize: 12, color: '#777', marginTop: 1 }}>repository</div>
          </div>
        </div>
        <div style={headActions}>
          <button type="button" data-testid="repo-browse" style={browseBtn} onClick={() => onBrowse({ kind: 'repo', repo: name })}>
            <BookIcon color={LENS.text} size={13} /> Browse
          </button>
          <ActionMenu testid="repo-menu" label={`Actions for ${name}`} items={menuItems} />
        </div>
      </div>

      {rebuildMsg && (
        <div data-testid="rebuild-status" style={{ fontSize: 12, color: rebuildMsg.startsWith('✓') ? '#9c9' : '#8af', marginTop: 10 }}>{rebuildMsg}</div>
      )}

      {confirming === 'disconnect' && (
        <div style={confirmBox}>
          <div style={{ fontSize: 13, marginBottom: 10 }}>Stop syncing and remove this remote? The repo stays as a local-only knowledge base — no facts are deleted.</div>
          <div style={{ display: 'flex', gap: 8 }}>
            <button type="button" data-testid="disconnect-confirm" style={btn(busy, 'danger')} disabled={busy} onClick={disconnect}>{busy ? 'Disconnecting…' : 'Disconnect'}</button>
            <button type="button" style={btn(busy)} disabled={busy} onClick={() => setConfirming(null)}>Cancel</button>
          </div>
        </div>
      )}

      {/* ── Information, most-load-bearing first: where writes go, then what
          this repo is wired to. Reference material collapses below. ── */}

      {/* Agent branch — where this repo's facts are written. Shares the lens
          write-target's green treatment: green already means "writes land
          here" in this UI (see writeReadTag), and a repo's agent branch is
          exactly the same statement as a lens's write target. */}
      <div style={writeCard}>
        <div style={writeCardLabel}>Agent branch</div>
        <div data-testid="repo-detail-branch" style={{ display: 'flex', alignItems: 'center', gap: 8, fontSize: 13, flexWrap: 'wrap' }}>
          <RepoDot repo={name} />
          <b style={{ color: '#eee' }}>{name}</b>
          <BranchChip branch={agentBranch || '…'} />
          <span style={{ color: '#777', fontSize: 12 }}>— new facts are written here</span>
        </div>
      </div>

      {/* No remote → no card. The pane shows state; the ⋯ menu offers to
          create the state that isn't there yet. A load FAILURE still gets a
          card: "we could not read this" is state, and silently rendering it as
          "not connected" hides a broken remote behind an empty pane. */}
      {!hideRemoteConfig && (remote.loading || remote.origin || remote.err) && (
        <RemoteCard repo={name} agentBranch={agentBranch} readOnly={readOnly}
          state={remote} onConnect={onConnect} onDisconnect={() => setConfirming('disconnect')}
          onChanged={onChanged} />
      )}

      {/* Shown whenever there is something to read OR the user could write
          one; a read-only repo with no manifest has neither. */}
      {(description || !readOnly) && (
        <DescriptionCard
          markdown={description}
          readOnly={readOnly}
          saveHint="committed to README.md on the agent branch"
          maxBytes={MAX_REPO_DESCRIPTION_BYTES}
          onSave={async md => {
            const updated = await api.updateRepo(name, { description: md });
            // Trust the server's re-read over the draft: it is what landed.
            setDescription(updated.description ?? '');
          }}
        />
      )}

      {license && (
        <Disclosure label="License" hint="LICENSE at the repo root" testid="repo-license-toggle" bodyTestid="repo-license">
          {/* Preformatted, NOT markdown: a licence's single newlines are
              meaningful, and a markdown renderer reflows them away. */}
          <pre style={licenseText} data-testid="repo-license-text">{license}</pre>
        </Disclosure>
      )}

      <ConnectPanel kind="repo" name={name} agentBranch={agentBranch} />
    </div>
  );
}

// Disclosure is the collapsed-by-default card used for reference material
// (description, connect snippets). The detail pane's job is to answer "what is
// this repo/lens wired to" at a glance; anything you only read once belongs
// behind a header you can open, not in the default vertical budget.
function Disclosure({ label, hint, testid, bodyTestid, action, open: openProp, onOpenChange, children }: {
  label: string; hint?: string; testid: string; bodyTestid?: string;
  // action renders beside the toggle (never inside it — buttons cannot nest).
  action?: React.ReactNode;
  // Optionally controlled, so an owner can force it open (e.g. clicking Edit
  // on a collapsed card should reveal the editor, not just arm it).
  open?: boolean; onOpenChange?: (open: boolean) => void;
  children: React.ReactNode;
}) {
  const [openState, setOpenState] = useState(false);
  const open = openProp ?? openState;
  const setOpen = (next: boolean) => { setOpenState(next); onOpenChange?.(next); };
  const boxRef = useRef<HTMLDivElement>(null);

  // A disclosure near the bottom of the pane would otherwise expand below the
  // fold, leaving the body it just revealed half off-screen. Pull the whole
  // card into view once the expanded content has laid out. 'nearest' scrolls
  // only as far as needed, so an already-visible card never jumps.
  useEffect(() => {
    if (!open) return;
    const id = requestAnimationFrame(() => boxRef.current?.scrollIntoView({ block: 'nearest', behavior: 'smooth' }));
    return () => cancelAnimationFrame(id);
  }, [open]);

  return (
    <div ref={boxRef} style={card}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
        <button
          type="button"
          className="k-bare"
          data-testid={testid}
          aria-expanded={open}
          style={{ ...disclosureHead, flex: 1 }}
          onClick={() => setOpen(!open)}
        >
          <span style={{ display: 'flex', alignItems: 'center', gap: 7 }}>
            <span style={{ display: 'flex', transform: open ? 'none' : 'rotate(-90deg)', transition: 'transform 120ms' }}>
              <ChevronDownIcon color="#888" size={12} />
            </span>
            <span style={{ ...cardLabel, marginBottom: 0 }}>{label}</span>
          </span>
          {hint && <span style={{ fontSize: 11, color: '#666' }}>{hint}</span>}
        </button>
        {action}
      </div>
      {open && <div data-testid={bodyTestid} style={{ marginTop: 10 }}>{children}</div>}
    </div>
  );
}

// DescriptionCard renders a repo's or lens's description as markdown behind a
// disclosure, and lets you edit it in place. Reading and writing share one
// component because the two differ only in where the text lands — saveHint
// names that destination, since "edit this text" and "commit a file into the
// repo's git history" are very different acts and the UI should say which.
//
// The rendered body scrolls at a fixed height so a long manifest can never take
// over the detail pane; the editor is a plain textarea over the raw markdown
// (no rich-text layer that could rewrite what gets committed).
function DescriptionCard({ markdown, readOnly, saveHint, maxBytes, onSave }: {
  markdown: string; readOnly: boolean; saveHint: string;
  // Byte cap the server enforces for THIS destination — a repo's README.md and
  // a lens's note share this editor but not their limits.
  maxBytes: number;
  onSave: (md: string) => Promise<void>;
}) {
  const [open, setOpen] = useState(false);
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');

  // Editing a collapsed card must reveal the editor, not merely arm it.
  const beginEdit = () => { setDraft(markdown); setErr(''); setEditing(true); setOpen(true); };
  const cancel = () => { setEditing(false); setErr(''); };
  const save = async () => {
    setBusy(true); setErr('');
    try { await onSave(draft); setEditing(false); }
    catch (e) { setErr(String(e)); }
    finally { setBusy(false); }
  };

  // The cap is a byte count server-side, so measure bytes: a draft of em-dashes
  // and smart quotes hits the limit at a third of its character count. Only
  // shown once the draft is within sight of the cap — a counter on every edit
  // is noise for the 200-byte case, but a lens's 4 KiB is close enough to a
  // page of notes that silence would let the user write past it and lose the
  // Save to a 422.
  const bytes = new TextEncoder().encode(draft).length;
  const over = bytes > maxBytes;
  const showCount = bytes > maxBytes * 0.8;

  return (
    <Disclosure
      label="Description"
      hint={markdown ? undefined : 'none yet'}
      testid="repo-description-toggle"
      bodyTestid="repo-description"
      open={open}
      onOpenChange={setOpen}
      action={!readOnly && !editing && (
        <button type="button" className="k-bare" data-testid="repo-description-edit"
          title="Edit description" aria-label="Edit description"
          style={cardIconBtn} onClick={beginEdit}>
          <PencilIcon color="#888" size={13} />
        </button>
      )}
    >
      {editing ? (
        <>
          <textarea
            data-testid="repo-description-input"
            value={draft}
            disabled={busy}
            onChange={e => setDraft(e.target.value)}
            style={descTextarea}
            spellCheck={false}
          />
          <div style={{ display: 'flex', justifyContent: 'space-between', gap: 8, fontSize: 11, color: '#666', marginTop: 6 }}>
            <span>Markdown · {saveHint}</span>
            {showCount && (
              <span data-testid="repo-description-count" style={{ color: over ? '#f88' : '#888', whiteSpace: 'nowrap' }}>
                {bytes.toLocaleString()} / {maxBytes.toLocaleString()} bytes
              </span>
            )}
          </div>
          {err && <div style={{ fontSize: 12, color: '#f88', marginTop: 6 }}>{err}</div>}
          <div style={{ display: 'flex', gap: 8, marginTop: 10 }}>
            <button type="button" data-testid="repo-description-save" style={btn(busy || over, 'primary')} disabled={busy || over}
              title={over ? `too long by ${(bytes - maxBytes).toLocaleString()} bytes` : undefined} onClick={save}>
              {busy ? 'Saving…' : 'Save'}
            </button>
            <button type="button" data-testid="repo-description-cancel" style={btn(busy)} disabled={busy} onClick={cancel}>Cancel</button>
          </div>
        </>
      ) : markdown ? (
        <div className="k-prose" style={{ maxHeight: 360, overflowY: 'auto', color: '#bbb', fontSize: 13, lineHeight: 1.6 }}>
          <ReactMarkdown remarkPlugins={markdownPlugins} components={markdownComponents}>{markdown}</ReactMarkdown>
        </div>
      ) : (
        <div style={{ fontSize: 13, color: '#666' }}>
          No description yet.{!readOnly && ' Use the pencil to write one in markdown.'}
        </div>
      )}
    </Disclosure>
  );
}

// MenuItem is one row of an ActionMenu, or a rule between groups.
type MenuItem =
  | { separator: true }
  | { separator?: false; label: string; testid?: string; danger?: boolean; disabled?: boolean; hint?: string; onSelect: () => void };

// ActionMenu is the ⋯ overflow next to the detail pane's primary buttons. It
// holds the rare and destructive actions (archive, disconnect, delete) that
// previously sat as loose buttons at three different scroll depths.
function ActionMenu({ items, label, testid }: { items: MenuItem[]; label: string; testid: string }) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);

  // Close on any click outside the menu. Escape is handled on the container
  // (not document) so it cannot race the app-wide Escape handler in App.tsx.
  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', onDown);
    return () => document.removeEventListener('mousedown', onDown);
  }, [open]);

  return (
    <div
      ref={ref}
      style={{ position: 'relative' }}
      onKeyDown={e => { if (e.key === 'Escape' && open) { e.stopPropagation(); setOpen(false); } }}
    >
      <button
        type="button"
        data-testid={testid}
        aria-label={label}
        aria-haspopup="menu"
        aria-expanded={open}
        style={menuTrigger(open)}
        onClick={() => setOpen(o => !o)}
      >
        <MoreVerticalIcon color={open ? '#eee' : '#aaa'} size={15} />
      </button>
      {open && (
        <div role="menu" style={menuPanel}>
          {items.map((it, i) => it.separator
            ? <div key={`sep-${i}`} style={menuSeparator} />
            : (
              <button
                key={it.label}
                type="button"
                role="menuitem"
                data-testid={it.testid}
                disabled={it.disabled}
                title={it.hint}
                style={menuItemStyle(!!it.disabled, !!it.danger)}
                onClick={() => { setOpen(false); it.onSelect(); }}
              >
                {it.label}
              </button>
            ))}
        </div>
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
  // Edit mode: reads maps a mounted read repo → its branch pin ('' = default).
  // The write repo is never a key (it is read implicitly). null = not editing.
  const [editReads, setEditReads] = useState<Record<string, string> | null>(null);
  const [branchNames, setBranchNames] = useState<Record<string, string[]>>({});
  // agentBranches caches each read repo's OWN agent branch, so the per-row
  // dropdown can offer it as the "(default)" option and filter it out of the
  // explicit-pin list — never the write repo's agent branch (a different repo).
  const [agentBranches, setAgentBranches] = useState<Record<string, string>>({});
  const editRef = useRef<HTMLDivElement>(null);

  // Opening the editor reveals content below the mounts card; scroll it in so
  // Save/Cancel are reachable without hunting for them.
  useEffect(() => {
    if (!editReads) return;
    const id = requestAnimationFrame(() => editRef.current?.scrollIntoView({ block: 'nearest', behavior: 'smooth' }));
    return () => cancelAnimationFrame(id);
  }, [editReads]);

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

  // ── edit mode ──
  // loadBranchData fetches a read repo's selectable branch names AND its own
  // agent branch (the default pin) together, cached so each is fetched once.
  const loadBranchData = (repo: string) => {
    if (branchNames[repo]) return;
    api.listBranchNames(repo).then(names => setBranchNames(prev => ({ ...prev, [repo]: names }))).catch(() => {});
    api.getAgentBranch(repo).then(b => setAgentBranches(prev => ({ ...prev, [repo]: b }))).catch(() => {});
  };
  const beginEdit = () => {
    // Seed from the current reads, dropping the write repo (read implicitly),
    // and preload each mounted repo's branch data for its dropdown.
    const seed: Record<string, string> = {};
    for (const r of reads) if (r.repo !== write) { seed[r.repo] = r.branch ?? ''; loadBranchData(r.repo); }
    setEditReads(seed);
  };
  const toggleRead = (repo: string) => {
    setEditReads(prev => {
      if (!prev) return prev;
      const next = { ...prev };
      if (repo in next) delete next[repo];
      else { next[repo] = ''; loadBranchData(repo); }
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
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, minWidth: 0 }}>
          <span style={lensIconBox}><LayersIcon color={LENS.accent} size={16} /></span>
          <div style={{ minWidth: 0 }}>
            <h3 style={{ margin: 0, fontSize: 16 }}>{name}</h3>
            <div style={{ fontSize: 12, color: '#777', marginTop: 1 }}>
              lens · {reads.length} mount{reads.length === 1 ? '' : 's'} · writes to {write || '…'}
            </div>
          </div>
        </div>
        {/* Same header grammar as RepoDetail: Browse, then the ⋯ overflow for
            whole-lens actions. Editing the mounts is a card-local action and
            lives on the Read mounts card itself. */}
        <div style={headActions}>
          <button type="button" data-testid="lens-browse" style={browseBtn} onClick={() => onBrowse({ kind: 'lens', name })}>
            <LayersIcon color={LENS.text} size={13} /> Browse
          </button>
          <ActionMenu
            testid="lens-menu"
            label={`Actions for ${name}`}
            items={[{ label: 'Delete lens', testid: 'lens-delete', danger: true, disabled: readOnly || busy, onSelect: () => setConfirming(true) }]}
          />
        </div>
      </div>

      {confirming && (
        <div style={confirmBox}>
          <div style={{ fontSize: 13, marginBottom: 8, color: '#f88' }}>Delete lens “{name}”? The underlying repos are not affected.</div>
          <div style={{ display: 'flex', gap: 8 }}>
            <button type="button" data-testid="lens-delete-confirm" style={btn(busy, 'danger')} disabled={busy} onClick={del}>Confirm delete</button>
            <button type="button" style={btn(busy)} disabled={busy} onClick={() => setConfirming(false)}>Cancel</button>
          </div>
        </div>
      )}

      {/* Write target — the one repo new facts land in. Same green card as a
          repo's Agent branch: both answer "where do new facts go". */}
      <div style={writeCard}>
        <div style={writeCardLabel}>Write target</div>
        <div data-testid="lens-detail-write" style={{ display: 'flex', alignItems: 'center', gap: 8, fontSize: 13, flexWrap: 'wrap' }}>
          <RepoDot repo={write} />
          <b style={{ color: '#eee' }}>{write || '…'}</b>
          <BranchChip branch={reads.find(r => r.repo === write)?.branch || writeBranch || 'agent branch'} />
          <span style={{ color: '#777', fontSize: 12 }}>— all new facts land here</span>
        </div>
      </div>

      {/* Read mounts — the resolved union, in server order. The write repo shows
          here (tagged write · read), never as a separate line. */}
      <div style={card}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 8 }}>
          <div style={{ ...cardLabel, marginBottom: 0 }}>Read mounts (union)</div>
          <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
            <span style={{ fontSize: 11, color: '#777' }}>resolved top → bottom</span>
            <button type="button" className="k-bare" data-testid="lens-edit"
              title="Edit read mounts" aria-label="Edit read mounts"
              style={cardIconBtn} disabled={readOnly || busy || !!editReads} onClick={beginEdit}>
              <PencilIcon color={readOnly || busy || !!editReads ? '#555' : '#888'} size={13} />
            </button>
          </div>
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

      {/* Edit mode expands directly under the mounts it edits, so the card you
          are changing stays in view above the editor. Like a disclosure, it is
          pulled into view so its Save/Cancel are never left below the fold. */}
      {editReads && (
        <div ref={editRef} style={{ ...card, borderColor: LENS.border }}>
          <div style={cardLabel}>Edit read mounts</div>
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
            const others = (branchNames[r.name] ?? []).filter(n => n !== agentBranches[r.name]);
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

      {(lens?.description || !readOnly) && (
        <DescriptionCard
          markdown={lens?.description ?? ''}
          readOnly={readOnly}
          saveHint="saved with the lens"
          maxBytes={MAX_LENS_DESCRIPTION_BYTES}
          onSave={async md => {
            const updated = await api.updateLens(name, { description: md });
            setLens(updated);
            onSaved();
          }}
        />
      )}

      <ConnectPanel kind="lens" name={name} />
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

// ConnectPanel renders the "Connect an agent" card for a repo or a lens. It
// covers BOTH client families, because they wire up differently:
//   • Claude Code uses the `knomit-bridge claude init` scaffolding (skills +
//     hooks + .mcp.json).
//   • Claude Cowork, Claude Desktop, and any other stdio MCP client just
//     register knomit-bridge as an mcpServers entry — no `claude init`.
// The scope arg is --lens <name> for a lens, --repo <name> for a repo.
function ConnectPanel({ kind, name, agentBranch }: { kind: 'repo' | 'lens'; name: string; agentBranch?: string }) {
  const [copied, setCopied] = useState<'cc' | 'mcp' | null>(null);
  const arg = kind === 'lens' ? '--lens' : '--repo';
  const initCmd = `knomit-bridge claude init ${arg} ${name}`;
  const mcpJson = `{
  "mcpServers": {
    "knomit": {
      "command": "knomit-bridge",
      "args": ["${arg}", "${name}"]
    }
  }
}`;
  const endpoint = kind === 'lens'
    ? `/api/v1/lenses/${name}/mcp`
    : `/api/v1/repos/${name}/branches/${agentBranch || '<agent-branch>'}/mcp`;

  const copy = (text: string, which: 'cc' | 'mcp') => {
    navigator.clipboard.writeText(text)
      .then(() => { setCopied(which); setTimeout(() => setCopied(null), 1500); })
      .catch(() => { /* clipboard unavailable — nothing to recover */ });
  };

  return (
    <Disclosure label="Connect an agent" hint="copy a command or MCP config" testid={`${kind}-connect-toggle`}>
      {/* Claude Code — the init scaffolding. */}
      <div style={connectClient}>Claude Code<span style={connectHint}>scaffolds skills + hooks</span></div>
      <div style={codeRow}>
        <code style={codeText}>{initCmd}</code>
        <button type="button" data-testid={`${kind}-copy`} title="Copy" aria-label="Copy Claude Code command"
          style={copyBtn} onClick={() => copy(initCmd, 'cc')}>
          <CopyIcon color={copied === 'cc' ? '#7c9' : '#888'} size={13} />
        </button>
      </div>

      {/* Claude Cowork / Desktop / other MCP clients — raw mcpServers wiring. */}
      <div style={{ ...connectClient, marginTop: 12 }}>Cowork · Desktop · other MCP clients<span style={connectHint}>add to mcpServers config</span></div>
      <div style={{ ...codeRow, alignItems: 'flex-start' }}>
        <pre style={{ ...codeText, margin: 0, whiteSpace: 'pre', overflowX: 'auto' }}>{mcpJson}</pre>
        <button type="button" data-testid={`${kind}-copy-mcp`} title="Copy" aria-label="Copy mcpServers config"
          style={copyBtn} onClick={() => copy(mcpJson, 'mcp')}>
          <CopyIcon color={copied === 'mcp' ? '#7c9' : '#888'} size={13} />
        </button>
      </div>

      <div style={{ fontSize: 12, color: '#666', marginTop: 8 }}>
        MCP endpoint <code style={{ fontFamily: 'var(--k-font-mono)', color: '#aaa' }}>{endpoint}</code>
      </div>
    </Disclosure>
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
// headActions is the detail-pane header's button cluster: primary action,
// Browse, then the ⋯ overflow. It never wraps under the title.
const headActions: React.CSSProperties = { display: 'flex', alignItems: 'center', gap: 8, flexShrink: 0 };
// descTextarea edits raw markdown, so it is monospaced and generously tall —
// a repo's README.md is a document, not a caption.
const descTextarea: React.CSSProperties = {
  width: '100%', boxSizing: 'border-box', minHeight: 240, resize: 'vertical',
  background: '#0c0c0c', border: '1px solid #333', borderRadius: 5, color: '#ddd',
  padding: '9px 11px', fontSize: 12.5, lineHeight: 1.6,
  fontFamily: 'var(--k-font-mono)',
};
const disclosureHead: React.CSSProperties = {
  display: 'flex', alignItems: 'center', justifyContent: 'space-between', width: '100%',
  background: 'none', border: 'none', padding: 0, cursor: 'pointer', textAlign: 'left',
};
// licenseText renders LICENSE preformatted, not as markdown — a licence's
// single newlines are meaningful, and a markdown renderer reflows them away.
const licenseText: React.CSSProperties = {
  margin: 0, whiteSpace: 'pre-wrap', wordBreak: 'break-word',
  fontFamily: 'var(--k-font-mono)', fontSize: 11.5, lineHeight: 1.6,
  color: '#a0a0a8', maxHeight: 320, overflowY: 'auto',
};
const menuTrigger = (open: boolean): React.CSSProperties => ({
  display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
  width: 30, height: 30, borderRadius: 5, padding: 0,
  background: open ? '#2a2a2a' : 'transparent', border: '1px solid ' + (open ? '#444' : '#333'),
  cursor: 'pointer',
});
const menuPanel: React.CSSProperties = {
  position: 'absolute', top: 'calc(100% + 6px)', right: 0, zIndex: 10, minWidth: 190,
  background: '#1c1c1c', border: '1px solid #383838', borderRadius: 6, padding: 4,
  boxShadow: '0 8px 24px rgba(0,0,0,0.5)',
};
const menuSeparator: React.CSSProperties = { height: 1, background: '#2e2e2e', margin: '4px 6px' };
const menuItemStyle = (disabled: boolean, danger: boolean): React.CSSProperties => ({
  display: 'block', width: '100%', textAlign: 'left',
  background: 'none', border: 'none', borderRadius: 4, padding: '7px 10px', fontSize: 13,
  color: disabled ? '#5a5a5a' : danger ? '#f88' : '#ddd',
  cursor: disabled ? 'default' : 'pointer',
});

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
// repoIconBox mirrors lensIconBox but tinted in the repo's own deterministic
// hue (RepoDot's palette), so a repo detail reads as structurally identical to
// a lens detail while staying colour-coded per repo.
const repoIconBox = (repo: string): React.CSSProperties => ({
  width: 30, height: 30, borderRadius: 7, flexShrink: 0,
  background: repoHueBg(repo), border: '1px solid ' + repoHueBorder(repo),
  display: 'flex', alignItems: 'center', justifyContent: 'center',
});
// connectClient labels a client family inside the Connect panel; connectHint is
// the muted "· what it does" trailer.
const connectClient: React.CSSProperties = { fontSize: 12, color: '#ccc', fontWeight: 600, marginBottom: 6, display: 'flex', alignItems: 'center', gap: 7 };
const connectHint: React.CSSProperties = { fontSize: 11, color: '#666', fontWeight: 400 };
// codeRow / codeText: the dark snippet box shared by both Connect blocks.
const codeRow: React.CSSProperties = { display: 'flex', alignItems: 'center', gap: 8, background: '#0c0c0c', border: '1px solid #222', borderRadius: 5, padding: '7px 10px' };
const codeText: React.CSSProperties = { fontFamily: 'var(--k-font-mono)', fontSize: 12, color: '#7c9', flex: 1, lineHeight: 1.5 };
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
