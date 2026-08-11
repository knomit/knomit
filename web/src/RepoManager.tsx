import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import ReactMarkdown from 'react-markdown';
import { api, repoAvailable, brokenLensMember, MAX_LENS_DESCRIPTION_BYTES, MAX_REPO_DESCRIPTION_BYTES, type ArchivedRepo, type RepoInfo, type Lens, type LensReadRef } from './api';
import { RepoStateChip } from './RepoStateChip';
import { CreateRepoForm } from './CreateRepoForm';
import { markdownPlugins, markdownComponents } from './markdown';
import { CreateLensForm } from './CreateLensForm';
import { RemoteCard } from './RemoteStatus';
import { useRemote } from './useRemote';
import { RemoteConnectWizard } from './RemoteConnectWizard';
import { LENS, formatBytes, repoHue, repoHueBg, repoHueBorder, noMouseFocus } from './utils';
import { BookIcon, ArchiveIcon, PlusIcon, GitBranchIcon, LayersIcon, PencilIcon, CopyIcon, HomeIcon } from './icons';
import { ManageOverview } from './ManageOverview';
import { btn, card, cardIconBtn, cardLabel, confirmBox, confirmInput, writeCard } from './manageStyles';
import { SettingsPage } from './SettingsPage';
import type { Section } from './SettingsPage';
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
  // No onClose: leaving Manage is the top bar's job now, not this pane's. The
  // surface has no chrome of its own to dismiss.
  //
  // Optionally carries a rename: {from, to} so the caller can re-point a
  // currently-BROWSED repo (state.repo, tracked outside this pane entirely) at
  // its new name instead of falling back to an arbitrary remaining repo — the
  // same repo, just renamed, is not the same case as one that vanished.
  onChanged: (renamed?: { from: string; to: string }) => void;  // parent re-fetches the repo list
  onBrowse: (ctx: BrowseContext) => void;  // switch the app to browse a repo/lens
  // True while a connect commit is in flight. Withholding the rail is not
  // enough on its own: the app's own exits (Escape, the top bar's step-out)
  // unmount this whole pane, and the wizard's contract is that nothing may
  // unmount it mid-commit. The parent MUST NOT close Manage while it is true.
  onBusyChange?: (busy: boolean) => void;
}

type Selection =
  | { kind: 'overview' }
  // focus names a settings block to land on, set when arriving from an Overview
  // cell so the thing you clicked is what you see.
  | { kind: 'repo'; name: string; focus?: string }
  // A repo's connect flow is a SELECTION, not a surface. It used to be a piece
  // of component state that made this whole pane return early, taking the rail
  // and the repo with it; as a selection it is a sub-page of the repo, the rail
  // survives, and the repo's own row stays lit while you are in it.
  | { kind: 'connect'; name: string }
  | { kind: 'archived' }
  | { kind: 'new' }
  | { kind: 'lens'; name: string }
  | { kind: 'newLens' }
  | null;

export function RepoManager({ open, repos, currentRepo, readOnly, hideRemoteConfig, onChanged, onBrowse, onBusyChange }: Props) {
  const [archived, setArchived] = useState<ArchivedRepo[]>([]);
  const [lenses, setLenses] = useState<Lens[]>([]);
  const [sel, setSel] = useState<Selection>(null);
  const [err, setErr] = useState('');

  // Set by the connect sub-page while its commit is in flight. Selecting
  // anything unmounts that page, and the commit stream has no abort and no
  // undo: the swap-and-rebuild would run on regardless, with nothing left
  // listening for its result. The wizard already withholds its own crumb; the
  // rail is the other exit, and it is this component's to withhold.
  const [connectBusy, setConnectBusy] = useState(false);

  // The detail column is the scrolling element, so switching entities has to
  // reset it. The old boxed pane was rarely taller than its frame and nobody
  // noticed; a settings PAGE is, and without this you land halfway down the
  // next repo — at whatever offset the last one happened to leave behind.
  const detailRef = useRef<HTMLElement>(null);
  const selKey = sel
    ? `${sel.kind}:${'name' in sel ? sel.name : 'id' in sel ? sel.id : ''}:${'focus' in sel ? sel.focus ?? '' : ''}`
    : '';
  // Assigning scrollTop rather than calling scrollTo(): it is an instant jump
  // either way, and jsdom implements the property but not the method.
  //
  // A LAYOUT effect, and that is load-bearing. SettingsPage's `focus` scroll is
  // a passive effect in a DESCENDANT, and React flushes passive effects
  // child-first — so as a passive effect this reset ran second and undid it,
  // silently defeating every Overview cell that asks to land on a block. Layout
  // effects also run child-first, but the whole layout pass precedes the whole
  // passive pass, so the reset lands first and the focus scroll then overrides
  // it. Order matters more than phase here: reset, then aim.
  useLayoutEffect(() => { if (detailRef.current) detailRef.current.scrollTop = 0; }, [selKey]);

  const refresh = () => {
    api.listArchived().then(setArchived).catch(e => setErr(String(e)));
    api.listLenses().then(setLenses).catch(e => setErr(String(e)));
  };

  useEffect(() => {
    if (open) refresh();
  }, [open]);

  // Republished upward unchanged. The cleanup matters as much as the call: if
  // this pane goes away while the flag is up (the error boundary resetting, the
  // app deciding it has no repos left), the parent must not be left holding a
  // lock whose holder is gone.
  useEffect(() => {
    onBusyChange?.(connectBusy);
    return () => { onBusyChange?.(false); };
  }, [connectBusy, onBusyChange]);

  if (!open) return null;

  // Manage lands on Overview: it is the only screen that answers "which of my
  // repositories needs something", and the repo you were browsing is one click
  // away in the rail, marked "viewing". With zero repos there is nothing to
  // summarise and no repo to land on — currentRepo is "" — so the create form
  // is the default instead. That is the first-run and archived-the-last-one
  // state, and it is why this branch comes first.
  const fallback: Selection = repos.length === 0
    ? { kind: 'new' as const }
    : { kind: 'overview' as const };
  const view = sel ?? fallback;

  // No mode header. The rail names the sections, the detail pane names the
  // entity, and the top bar's step-out button is the way back — a fourth
  // statement of "you are in Manage" would be chrome saying nothing new.
  return (
    <div style={surface} data-testid="manage-surface" aria-label="Manage">
      {err && <div style={errBox}>{err}</div>}

      <div style={body}>
          {/* ── Master list ── */}
          {/* Dimmed as a whole while a connect commit runs: every row below is
              disabled, and a rail that looked live but refused every click
              would read as a broken pane rather than a held one. The reason is
              stated where the reader is looking — the connect page's own rail
              note — not repeated here. */}
          <nav style={connectBusy ? { ...listCol, opacity: 0.4 } : listCol}>
            {/* Overview is pinned above the lists it summarises, and is the only
                rail row that is not an entity. Hidden with zero repos: there is
                nothing to summarise, and the create form owns that screen. */}
            {repos.length > 0 && (
              <div style={railTop}>
                <button
                  type="button"
                  data-testid="repomgr-overview"
                  onMouseDown={noMouseFocus}
                  style={listItem(view.kind === 'overview')}
                  disabled={connectBusy}
                  onClick={() => setSel({ kind: 'overview' })}
                >
                  <span style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                    <HomeIcon color="currentColor" size={13} /> Overview
                  </span>
                </button>
              </div>
            )}
            <div style={sectionHeader}>
              <BookIcon color="#7c9" size={13} />
              <span style={sectionTitle}>Repositories</span>
              <button
                type="button"
                data-testid="repomgr-new"
                  onMouseDown={noMouseFocus}
                title="New repository"
                aria-label="New repository"
                style={plusBtn(readOnly, view.kind === 'new')}
                disabled={readOnly || connectBusy}
                onClick={() => setSel({ kind: 'new' })}
              ><PlusIcon color="currentColor" size={14} /></button>
            </div>
            {repos.map(r => (
              <button
                key={r.name}
                type="button"
                data-testid={`repomgr-item-${r.name}`}
                onMouseDown={noMouseFocus}
                // Lit for the repo's connect sub-page too: the reader is still
                // inside that repository, and a rail that went dark mid-flow
                // would be the takeover's context loss in miniature.
                style={listItem((view.kind === 'repo' || view.kind === 'connect') && view.name === r.name)}
                disabled={connectBusy}
                onClick={() => setSel({ kind: 'repo', name: r.name })}
              >
                {/* The repo's own deterministic hue, as in the top-bar switcher,
                    the Overview table and every RepoDot in the detail panes.
                    This rail was the one place a repo appeared WITHOUT it,
                    which left the lenses below looking like the only things
                    with an identity. Lenses share one accent because their
                    identity is "lens"; a repo's is its own. */}
                <span style={{ display: 'flex', alignItems: 'center', gap: 8, minWidth: 0 }}>
                  <RepoDot repo={r.name} />
                  <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{r.name}</span>
                </span>
                {/* A repo with no live store keeps its rail row: this is the one
                    surface that can still act on it, so hiding it here would
                    leave the user reading about a repository with nowhere to
                    go. The chip replaces "viewing", which cannot be true of a
                    repo the browse surface refuses to open. */}
                {!repoAvailable(r)
                  ? <RepoStateChip repo={r} />
                  : r.name === currentRepo && <span style={viewingTag} title="the web UI is currently browsing this repo">viewing</span>}
              </button>
            ))}

            {/* Archived belongs UNDER Repositories, not beside it: an archived
                repo is a repository in a state, not a third kind of thing next
                to repos and lenses.

                One row, no children. An archived repo carries almost nothing —
                a date, an origin, and two buttons — so a rail entry each would
                be a click that buys you three lines. They share ONE page, and
                the contents rail on that page is the per-repo index. Nothing at
                all is rendered when nothing is archived: a dead control is
                worse than an absent one. */}
            {archived.length > 0 && (
              <button
                type="button"
                data-testid="repomgr-archived"
                  onMouseDown={noMouseFocus}
                style={archRow(view.kind === 'archived')}
                disabled={connectBusy}
                onClick={() => setSel({ kind: 'archived' })}
              >
                <ArchiveIcon color={view.kind === 'archived' ? '#c8b89a' : '#7a6a5a'} size={12} />
                <span>Archived</span>
                <span style={archCount}>{archived.length}</span>
              </button>
            )}

            <div style={sectionHeader}>
              <LayersIcon color={LENS.accent} size={13} />
              <span style={sectionTitle}>Lenses</span>
              <button
                type="button"
                data-testid="repomgr-new-lens"
                  onMouseDown={noMouseFocus}
                title="New lens"
                aria-label="New lens"
                style={plusBtn(readOnly, view.kind === 'newLens')}
                disabled={readOnly || connectBusy}
                onClick={() => setSel({ kind: 'newLens' })}
              ><PlusIcon color="currentColor" size={14} /></button>
            </div>
            {lenses.length === 0 && <div style={{ color: '#555', fontSize: 12, padding: '4px 10px' }}>None</div>}
            {lenses.map(l => (
              <button
                key={l.name}
                type="button"
                data-testid={`repomgr-lens-${l.name}`}
                onMouseDown={noMouseFocus}
                style={listItem(view.kind === 'lens' && view.name === l.name)}
                disabled={connectBusy}
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
          <section ref={detailRef} data-testid="manage-detail" style={detailCol}>
            {view.kind === 'overview' && (
              <ManageOverview
                repos={repos}
                lenses={lenses}
                archivedCount={archived.length}
                hideRemoteConfig={hideRemoteConfig}
                readOnly={readOnly}
                onSelectRepo={(name, focus) => setSel({ kind: 'repo', name, focus })}
                onSelectLens={name => setSel({ kind: 'lens', name })}
                onNewRepo={() => setSel({ kind: 'new' })}
                onNewLens={() => setSel({ kind: 'newLens' })}
              />
            )}
            {/* An unavailable repo gets its own pane rather than the settings
                page. RepoDetail's every read (description, agent branch, remote,
                mounts) resolves through the repo endpoints, which answer 409 for
                this repo — so the settings page would render as a wall of
                failures that never says the one thing worth knowing. */}
            {view.kind === 'repo' && !repoAvailable(repos.find(r => r.name === view.name) ?? {}) && (
              <RepoUnavailable repo={repos.find(r => r.name === view.name)!} />
            )}
            {view.kind === 'repo' && repoAvailable(repos.find(r => r.name === view.name) ?? {}) && (
              <RepoDetail
                key={view.name}
                name={view.name}
                lenses={lenses}
                focus={view.focus}
                onSelectLens={n => setSel({ kind: 'lens', name: n })}
                canArchive={!readOnly}
                readOnly={readOnly}
                hideRemoteConfig={hideRemoteConfig}
                onArchived={() => { onChanged(); refresh(); setSel(null); }}
                onConnect={() => setSel({ kind: 'connect', name: view.name })}
                onChanged={onChanged}
                // refresh() picks up the lens list's re-derived member names
                // (Mounted-in reads them off `lenses`, held in THIS pane's
                // state); onChanged(...) is the repo-list reload plus the
                // rename hint that lets the app follow a stale browse
                // selection to the new name (see the Props.onChanged doc).
                onRenamed={newName => { onChanged({ from: view.name, to: newName }); refresh(); setSel({ kind: 'repo', name: newName }); }}
                onBrowse={onBrowse}
                onError={setErr}
              />
            )}
            {/* Both exits land back on the Remote block rather than at the top
                of the settings page: it is the block you left from, and after a
                successful connect it is the one carrying the new state. */}
            {view.kind === 'connect' && (
              <RemoteConnectWizard
                key={view.name}
                repo={view.name}
                onCancel={() => setSel({ kind: 'repo', name: view.name, focus: 'remote' })}
                onDone={() => {
                  onChanged(); refresh();
                  setSel({ kind: 'repo', name: view.name, focus: 'remote' });
                }}
                // Passed as the raw setter, not a closure: this runs from an
                // effect keyed on the value it sets, so an identity that
                // changed every render would re-run it every render.
                onBusyChange={setConnectBusy}
              />
            )}
            {view.kind === 'archived' && (
              <ArchivedPage
                archived={archived}
                readOnly={readOnly}
                activeNames={new Set(repos.map(r => r.name))}
                onRestored={(name) => { onChanged(); refresh(); setSel({ kind: 'repo', name }); }}
                // Purging the LAST one takes the Archived rail row away with it
                // (it renders only while something is archived), so staying on
                // this selection would leave an empty page selected by a row
                // that no longer exists. `archived` is still the pre-purge list
                // here — one left means none after.
                onPurged={() => { refresh(); if (archived.length <= 1) setSel(null); }}
                onError={setErr}
              />
            )}
            {view.kind === 'new' && readOnly && <CreateBlocked what="repository" />}
            {view.kind === 'new' && !readOnly && (
              <CreateRepoForm
                onDone={(name) => { onChanged(); refresh(); setSel({ kind: 'repo', name }); }}
                // With no repos the fallback selection IS this form and Manage is
                // the whole window, so there is nothing to back out TO: clearing
                // the selection would re-render the form unchanged, and leaving
                // the mode would land on a browse surface that does not exist.
                // Omitting onCancel drops the button rather than offering a
                // control that visibly does nothing.
                onCancel={repos.length === 0 ? undefined : () => setSel(null)}
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
                // Same rename hint as RepoDetail's onRenamed (see its comment
                // above): the pane is keyed on `name`, so onChanged carries
                // {from, to} for the app to follow a browsed lens to its new
                // name instead of treating the old one as vanished.
                onRenamed={newName => { onChanged({ from: view.name, to: newName }); refresh(); setSel({ kind: 'lens', name: newName }); }}
                onBrowse={onBrowse}
                onError={setErr}
              />
            )}
            {view.kind === 'newLens' && readOnly && <CreateBlocked what="lens" />}
            {view.kind === 'newLens' && !readOnly && (
              <CreateLensForm
                repos={repos}
                lenses={lenses}
                onDone={(name) => { onChanged(); refresh(); setSel({ kind: 'lens', name }); }}
                onCancel={() => setSel(null)}
                onError={setErr}
              />
            )}
          </section>
      </div>
    </div>
  );
}

/**
 * CreateBlocked stands in for a create form the user cannot submit.
 *
 * The rail's `+` buttons are disabled when read-only, so normally you never
 * reach a create form at all. Two paths get past that: with zero repositories
 * the create form is the FALLBACK selection rather than something you clicked,
 * and a selection made while live survives into a history excursion. Both used
 * to land on a fully live form whose submit would 4xx with no warning.
 *
 * The copy names both causes rather than guessing between them: this pane is
 * given one `readOnly` boolean, and inferring the reason from a neighbouring
 * prop would be a guess that reads as fact.
 */
/**
 * RepoUnavailable is the settings page for a repository that has no live store.
 *
 * It offers no controls. Archive resolves through the live repo map and purge
 * only takes an already-archived repo, so every button this page could carry
 * would 4xx — and a dead control is a worse answer than an honest sentence. The
 * page's whole job is to convert "this repo is here but does nothing" into a
 * specific fact and the one move that fixes it.
 *
 * The three states want different moves, which is exactly why the server sends
 * the reason rather than a bare failure, and why this branches on it instead of
 * printing one apology for all three.
 */
function RepoUnavailable({ repo }: { repo: RepoInfo }) {
  const state = repo.state ?? 'unavailable';
  // Every sentence here has to name something the product can actually do
  // TODAY. Archive resolves through the live repo map and purge only accepts an
  // already-archived repo, so there is no supported way to remove this
  // registration — advice that implied otherwise would send the reader to a
  // 404 from a page that offers no such button anyway.
  const advice: Record<string, string> = {
    missing:
      'Its database file is not where the registry says it is. Put the file back — from a backup, or wherever it '
      + 'was moved to — and restart knomit; the registration is intact and will pick it up.',
    unopenable:
      'The file is there but could not be opened — a corrupt database, or one written by a newer build. '
      + 'The server log for this startup carries the underlying error. Repairing or replacing the file and '
      + 'restarting knomit is what clears this.',
    conflict:
      'Another registered repository already holds this knowledge base. Two local copies would both write the same '
      + 'agent branch and overwrite each other on push, so this one is left closed. Archiving the OTHER copy — the '
      + 'one that did open — and restarting knomit hands this registration its knowledge base back.',
  };
  return (
    <div data-testid={`repo-unavailable-${repo.name}`} style={{ maxWidth: 560, paddingTop: 30 }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
        <RepoDot repo={repo.name} />
        <h3 style={{ margin: 0, fontSize: 17, fontWeight: 600 }}>{repo.name}</h3>
        <RepoStateChip repo={repo} />
      </div>
      <p style={{ fontSize: 12.5, color: '#888', lineHeight: 1.6, marginTop: 12 }}>
        This repository is registered, but knomit has no store open for it, so none of it can be read or written.
        It is listed here — rather than quietly dropped, which is what used to happen — precisely so that this is
        visible.
      </p>
      {/* The server's own words, when it sent any. It knows things this build
          cannot infer (which file, which other repo), so it is quoted rather
          than paraphrased. */}
      {repo.detail && (
        <p data-testid="repo-unavailable-detail" style={{
          fontSize: 12, color: '#c9c9c9', lineHeight: 1.6, marginTop: 12,
          fontFamily: 'var(--k-font-mono)', background: '#131313',
          border: '1px solid #262626', borderRadius: 5, padding: '9px 11px',
        }}>{repo.detail}</p>
      )}
      {advice[state] && (
        <p style={{ fontSize: 12.5, color: '#888', lineHeight: 1.6, marginTop: 12 }}>{advice[state]}</p>
      )}
      {/* Said plainly rather than left to be discovered. Archiving needs a live
          store and purging needs an already-archived repo, so neither route is
          open to this repo — and a reader who is not told that will go hunting
          for a button that is not there. */}
      <p data-testid="repo-unavailable-no-removal" style={{ fontSize: 12, color: '#6a6a6a', lineHeight: 1.6, marginTop: 12 }}>
        Removing the registration itself is not supported yet: archiving needs a store to close, and purging only
        accepts an already-archived repository. Until the file comes back, this row stays.
      </p>
    </div>
  );
}

function CreateBlocked({ what }: { what: 'repository' | 'lens' }) {
  return (
    <div data-testid={`create-blocked-${what}`} style={{ maxWidth: 460, paddingTop: 30 }}>
      <h3 style={{ margin: 0, fontSize: 16 }}>Read-only</h3>
      <p style={{ fontSize: 12.5, color: '#888', lineHeight: 1.6, marginTop: 8 }}>
        No {what} can be created here: either this instance is read-only, or the
        app is anchored in history. If it is the anchor, returning to now lifts
        it.
      </p>
    </div>
  );
}

function RepoDetail({ name, lenses, focus, canArchive, readOnly, hideRemoteConfig, onArchived, onConnect, onChanged, onRenamed, onBrowse, onSelectLens, onError }: {
  name: string; canArchive: boolean; readOnly: boolean; hideRemoteConfig: boolean;
  // Every lens, so the Mounted-in block can be derived rather than fetched —
  // it is the reverse of a lens's read mounts, and the list is already here.
  lenses: Lens[];
  // Block to scroll to on open, set when arriving from an Overview cell so the
  // failing remote is in view rather than at the bottom of a page you must hunt.
  focus?: string;
  onArchived: () => void; onConnect: () => void; onChanged: () => void;
  // Fired with the NEW name once the server confirms the rename. The pane is
  // keyed on `name` by its caller, so this is how the parent learns to stop
  // addressing it by the old one.
  onRenamed: (newName: string) => void;
  onBrowse: (ctx: BrowseContext) => void; onSelectLens: (name: string) => void;
  onError: (m: string) => void;
}) {
  const [agentBranch, setAgentBranch] = useState('');
  const [description, setDescription] = useState('');
  // Owned here, not in DescriptionBody: the controls that set it live in the
  // block heading and the editor they open lives in the block body.
  const [descEditing, setDescEditing] = useState(false);
  const descEditor = useDescriptionEditor({
    markdown: description,
    maxBytes: MAX_REPO_DESCRIPTION_BYTES,
    editing: descEditing,
    onEditing: setDescEditing,
    onSave: async md => {
      const updated = await api.updateRepo(name, { description: md });
      // Trust the server's re-read over the draft: it is what landed.
      setDescription(updated.description ?? '');
    },
  });
  // License is now editable, mirroring the description above it — but its READ
  // view stays <pre>. See the section body for why that must not change.
  const [license, setLicense] = useState('');
  const [licEditing, setLicEditing] = useState(false);
  const licEditor = useDescriptionEditor({
    markdown: license,
    maxBytes: MAX_REPO_DESCRIPTION_BYTES,
    editing: licEditing,
    onEditing: setLicEditing,
    onSave: async text => {
      const updated = await api.updateRepo(name, { license: text });
      setLicense(updated.license ?? '');
    },
  });
  // Destructured to a local, not read as `licEditor.bodyRef` at the JSX site
  // below: the licence read view attaches this ref directly (DescriptionBody
  // never renders it — see the section body for why), and a bare local
  // matches how DescriptionBody itself takes bodyRef off its `editor` prop.
  const { bodyRef: licBodyRef } = licEditor;
  const [rebuilding, setRebuilding] = useState(false);
  const [rebuildMsg, setRebuildMsg] = useState('');
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState<'disconnect' | null>(null);
  // Rename draft + its typed confirmation. Local to the danger zone; cleared
  // when the pane switches repos by the same effect that clears description.
  const [renameTo, setRenameTo] = useState('');
  const [renameConfirm, setRenameConfirm] = useState('');
  const [renaming, setRenaming] = useState(false);
  // The pane owns the remote so the Remote block can render the right thing for
  // each state — Connect when there is none, the card when there is, the error
  // when the read failed. RemoteCard is the display half of that same state.
  const remote = useRemote(name, !hideRemoteConfig);

  useEffect(() => {
    let cancelled = false;
    api.getAgentBranch(name).then(b => { if (!cancelled) setAgentBranch(b); }).catch(() => {});
    setDescription('');
    setLicense('');
    setRenameTo(''); setRenameConfirm('');
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
  // Unlike rebuild's 409, this one is NOT rewritten into invented copy: the
  // server's detail for the "changed during rename" conflict already tells the
  // user the one true thing to do (re-read and retry), and every sibling
  // mutation in this file (archive, disconnect, restore, purge…) already
  // surfaces `String(e)` verbatim rather than guessing at friendlier words.
  const rename = async () => {
    onError(''); setRenaming(true);
    try {
      const updated = await api.renameRepo(name, renameTo);
      setRenameTo(''); setRenameConfirm('');
      // The pane is addressed by name, so it must follow the repo to its new
      // one — leaving it on the old name would show a 404 on the next read.
      onRenamed(updated.name);
    } catch (e) {
      onError(`rename failed: ${String(e)}`);
    } finally {
      setRenaming(false);
    }
  };

  // Blocks, ordered identity → wiring → operations → danger. That ordering is
  // the rule for where a NEW setting goes, which is what a page with no tabs
  // needs in place of a nav to reorganise.
  const sections: Section[] = [];

  // Shown whenever there is something to read OR the user could write one; a
  // read-only repo with no manifest has neither.
  if (description || !readOnly) {
    sections.push({
      id: 'description',
      title: 'Description',
      hint: `README.md, committed to the agent branch · up to ${Math.round(MAX_REPO_DESCRIPTION_BYTES / 1024)} KiB`,
      action: readOnly ? undefined : <DescriptionActions editor={descEditor} label="Edit description" />,
      body: <DescriptionBody editor={descEditor} readOnly={readOnly} />,
    });
  }

  // Shown whenever there is something to read OR the user could write one —
  // the same rule the Description block above uses. A read-only repo with no
  // LICENSE gets no block: nothing to read, nothing to offer.
  if (license || !readOnly) {
    sections.push({
      id: 'license',
      title: 'License',
      hint: `LICENSE at the repo root · up to ${Math.round(MAX_REPO_DESCRIPTION_BYTES / 1024)} KiB`,
      action: readOnly ? undefined : (
        <DescriptionActions editor={licEditor} label={license ? 'Edit license' : 'Add license'} testIdPrefix="repo-license" />
      ),
      body: licEditing ? (
        // The EDITOR is the shared monospace textarea; only the read view
        // below differs from the description block.
        <DescriptionBody editor={licEditor} readOnly={readOnly}
          containerTestId="repo-license-editor" textareaTestId="license-textarea" />
      ) : license ? (
        // PREFORMATTED, NOT MARKDOWN. A licence's single newlines are
        // meaningful and a markdown renderer reflows them away — the MIT text
        // loses every line break. This is the one thing that must not be
        // copied from the Description block, whose read view IS markdown.
        //
        // ref={licBodyRef}: this IS the licence's read view (unlike the
        // description block, whose read view lives inside DescriptionBody), so
        // it — not DescriptionBody's markdown div, which the licence editor
        // never renders while reading — is what useDescriptionEditor's
        // useLayoutEffect must measure. Without this the hook's bodyRef never
        // attaches to anything for licEditor, readHeight.current stays null
        // forever, and the editor falls back to DESC_BODY_MAX on every open —
        // a three-line MIT header expanding to full height.
        <pre ref={licBodyRef} style={licenseText} data-testid="repo-license">{license}</pre>
      ) : (
        <div style={{ fontSize: 12.5, color: '#888' }}>
          No LICENSE at the repo root. knomit stores whatever terms you supply;
          it does not generate them.
        </div>
      ),
    });
  }

  // Agent branch — where this repo's facts are written. Shares the lens
  // write-target's green treatment: green already means "writes land here" in
  // this UI (see writeReadTag), and a repo's agent branch is exactly the same
  // statement as a lens's write target.
  sections.push({
    id: 'agent-branch',
    title: 'Agent branch',
    hint: 'where new facts are written',
    body: (
      <div style={writeCard}>
        <div data-testid="repo-detail-branch" style={{ display: 'flex', alignItems: 'center', gap: 8, fontSize: 13, flexWrap: 'wrap' }}>
          <RepoDot repo={name} />
          <b style={{ color: '#eee' }}>{name}</b>
          <BranchChip branch={agentBranch || '…'} />
          <span style={{ color: '#777', fontSize: 12 }}>— server-authoritative</span>
        </div>
      </div>
    ),
  });

  // The Remote block always exists (unless the server hides remote config), and
  // says which of THREE states it is in. "Not connected" now renders as content
  // with the Connect action on it, rather than as an absent card plus an offer
  // buried in an overflow menu — the block shows the state and carries the
  // action that changes it. A load FAILURE stays distinct from "not connected":
  // "we could not read this" is state, and collapsing the two would invite you
  // to overwrite a remote that is merely unreadable.
  if (!hideRemoteConfig) {
    const connected = remote.loading || remote.origin || remote.err;
    sections.push({
      id: 'remote',
      title: 'Remote',
      hint: 'pull and push against an origin',
      tail: remote.origin ? <span style={{ width: 7, height: 7, borderRadius: '50%', background: remote.err ? '#f88' : '#7c9', display: 'inline-block' }} /> : undefined,
      body: connected ? (
        <>
          <RemoteCard repo={name} agentBranch={agentBranch} readOnly={readOnly}
            state={remote} onConnect={onConnect} onDisconnect={() => setConfirming('disconnect')}
            onChanged={onChanged} />
          {confirming === 'disconnect' && (
            <div style={confirmBox}>
              <div style={{ fontSize: 13, marginBottom: 10 }}>Stop syncing and remove this remote? The repo stays as a local-only knowledge base — no facts are deleted.</div>
              <div style={{ display: 'flex', gap: 8 }}>
                <button type="button" data-testid="disconnect-confirm" style={btn(busy, 'danger')} disabled={busy} onClick={disconnect}>{busy ? 'Disconnecting…' : 'Disconnect'}</button>
                <button type="button" style={btn(busy)} disabled={busy} onClick={() => setConfirming(null)}>Cancel</button>
              </div>
            </div>
          )}
        </>
      ) : (
        <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
          <span style={{ fontSize: 13, color: '#888' }}>Not connected — this repository exists only on this machine.</span>
          {/* The block is headed "Remote" and the line beside it says "Not
              connected", so the object needs no third naming. The ellipsis
              stays: this opens the wizard, it does not connect anything. */}
          <button type="button" data-testid="remote-connect" style={btn(readOnly)} disabled={readOnly} onClick={onConnect}>
            Connect…
          </button>
        </div>
      ),
    });
  }

  // Mounted in — the reverse of a lens's read mounts, derived from the lens
  // list the manager already holds rather than fetched. Write-target
  // memberships lead, because that is where an agent using the lens actually
  // writes; read-only ones follow. No block when nothing references the repo:
  // "mounted in nothing" is the default state of an install with no lenses, and
  // heading it would be noise on every page.
  const mounts = lenses
    .map(l => ({
      name: l.name,
      write: l.write.name === name,
      read: l.reads.find(r => r.name === name),
    }))
    .filter(m => m.write || m.read)
    .sort((a, b) => Number(b.write) - Number(a.write) || a.name.localeCompare(b.name));

  if (mounts.length > 0) {
    sections.push({
      id: 'mounted-in',
      title: 'Mounted in',
      hint: 'lenses that read this repository',
      tail: <span style={{ fontSize: 10.5, color: '#6a6a6a' }}>{mounts.length}</span>,
      body: (
        <div style={card}>
          {mounts.map((m, i) => (
            <div key={m.name} data-testid={`repo-mounted-${m.name}`}
              style={{ display: 'flex', alignItems: 'center', gap: 9, padding: '7px 2px', borderBottom: i === mounts.length - 1 ? 'none' : '1px solid #242424' }}>
              <LayersIcon color={LENS.accent} size={12} />
              {/* The chip opens the LENS, not this repo — you clicked the lens,
                  so that is what you get. */}
              <button type="button" className="k-bare" data-testid={`repo-mounted-open-${m.name}`}
                style={mountedLensLink} onClick={() => onSelectLens(m.name)}>{m.name}</button>
              {m.read?.branch && <BranchChip branch={m.read.branch} />}
              <div style={{ flex: 1 }} />
              {m.write
                ? <span style={writeReadTag}>write target</span>
                : <span style={readTag}>{m.read?.branch ? 'read · pinned' : 'read'}</span>}
            </div>
          ))}
        </div>
      ),
    });
  }

  sections.push({
    id: 'agent-access',
    title: 'Agent access',
    hint: 'how a client connects to this repository',
    body: <ConnectBody kind="repo" name={name} agentBranch={agentBranch} />,
  });

  // Rebuild was an overflow-menu item with no readout beside it. As a block it
  // sits next to the thing it acts on, and the "started" line has somewhere to
  // land that is not floating under the header.
  sections.push({
    id: 'index',
    title: 'Index',
    hint: 'the search and recall index for this branch',
    action: (
      <button type="button" data-testid="repo-rebuild" style={btn(readOnly || rebuilding)} disabled={readOnly || rebuilding} onClick={rebuild}>
        {rebuilding ? 'Rebuilding…' : 'Rebuild'}
      </button>
    ),
    body: (
      <div style={{ fontSize: 12.5, color: '#888' }}>
        {rebuildMsg
          ? <span data-testid="rebuild-status" style={{ color: rebuildMsg.startsWith('✓') ? '#9c9' : '#8af' }}>{rebuildMsg}</span>
          : 'Re-indexing runs in the background; the repo stays readable throughout.'}
      </div>
    ),
  });

  // Every repo is archivable, including
  // the last one — no repo is privileged, and an empty knomit is a valid state
  // (it is how a fresh install starts).
  sections.push({
    id: 'danger',
    title: 'Danger zone',
    danger: true,
    body: (
      <div style={dangerBox}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
          <span style={{ fontSize: 13, color: '#ddd' }}>Archive this repository</span>
          <div style={{ flex: 1 }} />
          <button type="button" data-testid="repo-archive" style={btn(!canArchive || busy, 'danger')} disabled={!canArchive || busy} onClick={archive}>
            Archive
          </button>
        </div>
        <div style={{ fontSize: 11.5, color: '#777', marginTop: 6 }}>
          Recoverable — it moves into Archived under Repositories, and nothing is deleted.
        </div>
        <div style={{ borderTop: '1px solid #3a2020', marginTop: 14, paddingTop: 14 }}>
          <div style={{ fontSize: 13, color: '#ddd', marginBottom: 4 }}>Rename this repository</div>
          {/* Name the actual consequence rather than saying "this is
              dangerous". Nothing is deleted and the rename is reversible; what
              breaks is the URL agents are configured against — NOT their
              in-flight queries, which resolve through the uid, not the name. */}
          <div data-testid="repo-rename-warning" style={{ fontSize: 11.5, color: '#777', marginBottom: 10 }}>
            Agent MCP endpoint URLs contain this repository's name and will stop
            resolving until each agent is reconfigured. Facts, history, lens
            mounts and in-flight agent queries are unaffected.
          </div>
          <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', alignItems: 'center' }}>
            <input
              data-testid="repo-rename-input"
              value={renameTo}
              onChange={e => setRenameTo(e.target.value)}
              placeholder="new-name"
              disabled={readOnly || renaming}
              style={renameInput}
            />
            <input
              data-testid="repo-rename-confirm"
              value={renameConfirm}
              onChange={e => setRenameConfirm(e.target.value)}
              placeholder={`type "${name}" to confirm`}
              disabled={readOnly || renaming}
              style={renameInput}
            />
            <button
              type="button"
              data-testid="repo-rename-submit"
              style={btn(readOnly || renaming || renameConfirm !== name || !renameTo || renameTo === name, 'danger')}
              disabled={readOnly || renaming || renameConfirm !== name || !renameTo || renameTo === name}
              onClick={rename}
            >
              {renaming ? 'Renaming…' : 'Rename'}
            </button>
          </div>
        </div>
      </div>
    ),
  });

  return (
    <div>
      <div style={detailHead}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, minWidth: 0 }}>
          <span style={repoIconBox(name)}><BookIcon color={repoHue(name)} size={16} /></span>
          <div style={{ minWidth: 0 }}>
            <h3 style={{ margin: 0, fontSize: 16 }}>{name}</h3>
            <div style={{ fontSize: 12, color: '#777', marginTop: 1 }}>repository settings</div>
          </div>
        </div>
        {/* Browse is the only action left in the header, and the only place the
            word appears in the whole mode: it leaves Manage AND switches the app
            to this repo, which is what separates it from the top bar's step-out.
            The ⋯ menu is gone — each of its items now sits in the block that
            owns it (Rebuild → Index, Archive → Danger zone, Connect → Remote). */}
        <div style={headActions}>
          <button type="button" data-testid="repo-browse" style={browseBtn} onClick={() => onBrowse({ kind: 'repo', repo: name })}>
            <BookIcon color={repoHue(name)} size={13} /> Browse
          </button>
        </div>
      </div>

      <SettingsPage sections={sections} focus={focus} testid="repo-settings" />
    </div>
  );
}

// useDescriptionEditor owns the draft behind a repo's README or a lens's note.
//
// It is a hook rather than state inside DescriptionBody because the two halves
// of this editor are rendered by two different components: the text sits in the
// block's body, its controls sit in the block's heading. Both need one draft,
// and the only place that renders both is the caller. See DescriptionActions
// for why the controls are up there.
export function useDescriptionEditor({ markdown, maxBytes, editing, onEditing, onSave }: {
  markdown: string;
  // Byte cap the server enforces for THIS destination — a repo's README.md and
  // a lens's note share this editor but not their limits.
  maxBytes: number;
  editing: boolean;
  onEditing: (editing: boolean) => void;
  onSave: (md: string) => Promise<void>;
}) {
  // null = untouched, so the editor falls through to `markdown` and the pencil
  // that opens it needs no seeding step. Closing the editor clears it back to
  // null, which is also what makes a Cancel discard the draft.
  const [draft, setDraft] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');

  const text = draft ?? markdown;
  const cancel = () => { setDraft(null); setErr(''); onEditing(false); };
  const save = async () => {
    setBusy(true); setErr('');
    try { await onSave(text); setDraft(null); setErr(''); onEditing(false); }
    catch (e) { setErr(String(e)); }
    finally { setBusy(false); }
  };

  // The cap is a byte count server-side, so measure bytes: a draft of em-dashes
  // and smart quotes hits the limit at a third of its character count. Only
  // shown once the draft is within sight of the cap — a counter on every edit
  // is noise for the 200-byte case, but a lens's 4 KiB is close enough to a
  // page of notes that silence would let the user write past it and lose the
  // Save to a 422.
  const bytes = new TextEncoder().encode(text).length;
  const over = bytes > maxBytes;
  const showCount = bytes > maxBytes * 0.8;

  // Reading and writing are the same box, so the pencil swaps what is in it and
  // resizes nothing. The rendered body is content-sized within its band, so the
  // editor cannot name a height of its own — it would be right for one README
  // and wrong for the next. Instead the read height is measured after every
  // read render and handed to the textarea, which is border-box like the body
  // it replaces, so the outer box is identical.
  //
  // The empty description is the one case with nothing to measure, and nobody
  // writes a README from scratch in the 20px a placeholder line would give.
  // HTMLElement, not HTMLDivElement: the description block attaches this to a
  // <div> (DescriptionBody's markdown render), but the licence block attaches
  // it directly to its <pre> read view — DescriptionBody never renders the
  // licence's read view, so there is no <div> for licEditor to measure.
  const bodyRef = useRef<HTMLElement>(null);
  const readHeight = useRef<number | null>(null);
  useLayoutEffect(() => {
    if (!editing && bodyRef.current) readHeight.current = bodyRef.current.offsetHeight;
  });
  const height = markdown ? (readHeight.current ?? DESC_BODY_MAX) : DESC_BODY_BLANK;

  return {
    markdown, maxBytes, editing, text, setDraft, busy, err, save, cancel,
    bytes, over, showCount, bodyRef, height,
    edit: () => onEditing(true),
  };
}
export type DescriptionEditor = ReturnType<typeof useDescriptionEditor>;

// DescriptionActions is the description block's heading control: a pencil while
// reading, Save and Cancel while writing.
//
// Save and Cancel used to sit under the textarea, with a hint line above them.
// That is the conventional place and it was wrong here, because it makes the
// act of clicking the pencil push every block below the description ~55px down
// the page — the reader's eye is on the text, and the text is what moves. The
// heading already reserves a right-aligned slot, and swapping a control for two
// controls inside a slot of fixed height moves nothing at all.
//
// The hint line did not need a new home: it said "Markdown · committed to
// README.md on the agent branch" directly under a heading that already reads
// "README.md, committed to the agent branch". The byte counter did, and it is
// here, where it can appear and disappear without reflowing the page either.
export function DescriptionActions({ editor, label, testIdPrefix = 'repo-description' }: {
  editor: DescriptionEditor; label: string;
  // Lets a second block (the licence) reuse this control without colliding
  // testids with the description block it sits beside on the same page.
  testIdPrefix?: string;
}) {
  const { editing, busy, over, bytes, maxBytes, showCount, save, cancel } = editor;
  if (!editing) {
    return (
      <button type="button" className="k-bare" data-testid={`${testIdPrefix}-edit`}
        title={label} aria-label={label}
        style={cardIconBtn()} onClick={editor.edit}>
        <PencilIcon color="#888" size={13} />
      </button>
    );
  }
  return (
    <>
      {showCount && (
        <span data-testid={`${testIdPrefix}-count`}
          style={{ fontSize: 11, color: over ? '#f88' : '#888', whiteSpace: 'nowrap' }}>
          {bytes.toLocaleString()} / {maxBytes.toLocaleString()} bytes
        </span>
      )}
      <button type="button" data-testid={`${testIdPrefix}-save`} style={descBtn(busy || over, 'primary')} disabled={busy || over}
        title={over ? `too long by ${(bytes - maxBytes).toLocaleString()} bytes` : undefined} onClick={save}>
        {busy ? 'Saving…' : 'Save'}
      </button>
      <button type="button" data-testid={`${testIdPrefix}-cancel`} style={descBtn(busy)} disabled={busy} onClick={cancel}>Cancel</button>
    </>
  );
}

// DescriptionBody renders a repo's or lens's description as markdown, or the
// raw markdown in a textarea while it is being edited. Reading and writing
// share one component because they are one box: same width, same height, same
// position on the page.
//
// It used to live behind a disclosure. In a boxed dialog that was right: the
// pane's whole budget was about two cards deep, so a document had to fold away.
// Now the page is the window and a README is the most-read thing on it, so it
// renders open and the fold is gone. The rendered body still scrolls within a
// bounded band — a long manifest must not push the wiring blocks off the page —
// and the editor is a plain textarea over the raw markdown, with no rich-text
// layer that could rewrite what gets committed.
export function DescriptionBody({ editor, readOnly, containerTestId = 'repo-description', textareaTestId = 'repo-description-input' }: {
  editor: DescriptionEditor; readOnly: boolean;
  // Same reason as DescriptionActions' testIdPrefix: the licence block reuses
  // this component for its EDITOR only (its read view is <pre>, never this),
  // and needs its own textarea identity — `data-testid="license-textarea"` —
  // without colliding with the description block's, which sits on the page
  // at the same time.
  containerTestId?: string; textareaTestId?: string;
}) {
  const { markdown, editing, text, busy, err, setDraft, bodyRef, height } = editor;
  return (
    <div data-testid={containerTestId}>
      {editing ? (
        <>
          <textarea
            data-testid={textareaTestId}
            value={text}
            disabled={busy}
            onChange={e => setDraft(e.target.value)}
            style={{ ...descTextarea, height }}
            spellCheck={false}
          />
          {/* The one thing still allowed to grow the block, because a failed
              save is worth a shove: it says the text you are looking at is not
              what is stored. */}
          {err && <div style={{ fontSize: 12, color: '#f88', marginTop: 6 }}>{err}</div>}
        </>
      ) : markdown ? (
        // Full width: nothing shares the row, so a README that scrolls uses the
        // whole column instead of wrapping early around a 13px glyph.
        <div ref={bodyRef} className="k-prose"
          style={{ minHeight: DESC_BODY_MIN, maxHeight: DESC_BODY_MAX, overflowY: 'auto', color: '#bbb', fontSize: 13, lineHeight: 1.6 }}>
          <ReactMarkdown remarkPlugins={markdownPlugins} components={markdownComponents}>{markdown}</ReactMarkdown>
        </div>
      ) : (
        // "in the heading" because the pencil no longer sits on this row — it
        // moved to the block heading's right edge, too far for a bare "the
        // pencil" to point at anything the eye finds locally.
        <div style={{ fontSize: 13, color: '#666' }}>
          No description yet.{!readOnly && ' Use the pencil in the heading to write one in markdown.'}
        </div>
      )}
    </div>
  );
}

// ArchivedPage is every archived repo on ONE page, with the contents rail as
// its index. An archived repo carries three lines — when it was archived, what
// its origin was, and two buttons — so a rail entry each would have been a
// click that buys almost nothing, and a detail pane per repo would have been
// mostly empty. Restoring one is also usually a comparison ("which of these two
// was it?"), which a list answers and a pane cannot.
function ArchivedPage({ archived, readOnly, activeNames, onRestored, onPurged, onError }: {
  archived: ArchivedRepo[]; readOnly: boolean; activeNames: Set<string>;
  onRestored: (name: string) => void; onPurged: () => void; onError: (m: string) => void;
}) {
  const sections: Section[] = archived.map(info => ({
    id: `archived-${info.id}`,
    title: info.name,
    hint: `archived ${new Date(info.archivedAt).toLocaleString()}`,
    body: (
      <ArchivedDetail
        key={info.id}
        info={info}
        readOnly={readOnly}
        activeNames={activeNames}
        onRestored={onRestored}
        onPurged={onPurged}
        onError={onError}
      />
    ),
  }));

  return (
    <div>
      <div style={detailHead}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, minWidth: 0 }}>
          <span style={archIconBox}><ArchiveIcon color="#a08c6a" size={16} /></span>
          <div style={{ minWidth: 0 }}>
            <h3 style={{ margin: 0, fontSize: 16 }}>Archived</h3>
            <div style={{ fontSize: 12, color: '#777', marginTop: 1 }}>
              {archived.length} repositor{archived.length === 1 ? 'y' : 'ies'} · restorable, nothing is deleted
            </div>
          </div>
        </div>
        {/* No Browse: an archived repo is not a surface you can read. */}
      </div>
      <SettingsPage sections={sections} testid="archived-settings" />
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
  const size = formatBytes(info.sizeBytes);

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
    <div data-testid={`archived-body-${info.id}`}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
        <span style={{ fontSize: 12.5, color: '#8a8a8a' }}>
          origin <span style={{ fontFamily: 'var(--k-font-mono)', fontSize: 11.5, color: info.origin ? '#9aa' : '#666' }}>{info.origin || 'none'}</span>
        </span>
        {/* What purging this would give back. An archived database keeps its
            full size on disk under a filename derived from its uid, so there is
            no directory the user could have looked in to work this out — which
            is why the server sends it and why it belongs beside the Purge
            button rather than on a page nobody opens. */}
        {size && (
          <span data-testid={`archived-size-${info.id}`} style={{ fontSize: 12.5, color: '#8a8a8a' }}>
            on disk <span style={{ fontFamily: 'var(--k-font-mono)', fontSize: 11.5, color: '#9aa' }}>{size}</span>
          </span>
        )}
        <div style={{ flex: 1 }} />
        {confirming === null && (
          <div style={{ display: 'flex', gap: 8 }}>
            <button type="button" data-testid={`archived-restore-${info.id}`} style={btn(readOnly || busy)} disabled={readOnly || busy} onClick={beginRestore}>Restore</button>
            <button type="button" data-testid={`archived-purge-${info.id}`} style={btn(readOnly || busy, 'danger')} disabled={readOnly || busy} onClick={() => { setPurgeText(''); setConfirming('purge'); }}>Purge…</button>
          </div>
        )}
      </div>

      {confirming === 'restore' && (
        <div style={confirmBox}>
          <div style={{ fontSize: 13, marginBottom: 8 }}>“{info.name}” is already active. Restore under a new name:</div>
          <input autoFocus data-testid={`restore-name-input-${info.id}`} style={confirmInput} value={renameTo} placeholder="new repo name"
            onChange={e => setRenameTo(e.target.value)} />
          <div style={{ display: 'flex', gap: 8, marginTop: 10 }}>
            <button type="button" data-testid={`restore-confirm-${info.id}`} style={btn(busy || !renameTo, 'primary')} disabled={busy || !renameTo} onClick={() => doRestore(renameTo)}>Restore</button>
            <button type="button" style={btn(busy)} disabled={busy} onClick={() => setConfirming(null)}>Cancel</button>
          </div>
        </div>
      )}

      {confirming === 'purge' && (
        <div style={confirmBox}>
          <div style={{ fontSize: 13, marginBottom: 8, color: '#f88' }}>
            This permanently deletes the archived repo and its history. Type <b>{info.name}</b> to confirm:
          </div>
          <input autoFocus data-testid={`purge-confirm-input-${info.id}`} style={confirmInput} value={purgeText} placeholder={info.name}
            onChange={e => setPurgeText(e.target.value)} />
          <div style={{ display: 'flex', gap: 8, marginTop: 10 }}>
            <button type="button" data-testid={`purge-confirm-${info.id}`} style={btn(busy || purgeText !== info.name, 'danger')} disabled={busy || purgeText !== info.name} onClick={doPurge}>Confirm purge</button>
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
function LensDetail({ lens: initial, name, repos, readOnly, onDeleted, onSaved, onRenamed, onBrowse, onError }: {
  lens?: Lens; name: string; repos: RepoInfo[]; readOnly: boolean;
  onDeleted: () => void; onSaved: () => void;
  // Fired with the NEW name once the server confirms the rename — same
  // contract as RepoDetail's onRenamed. The pane is keyed on `name` by its
  // caller, so this is how the parent learns to stop addressing it by the old
  // one.
  onRenamed: (newName: string) => void;
  onBrowse: (ctx: BrowseContext) => void; onError: (m: string) => void;
}) {
  const [lens, setLens] = useState<Lens | undefined>(initial);
  // Owned here for the same reason as a repo's descEditing: the Note block's
  // controls are in its heading, its editor in its body.
  const [noteEditing, setNoteEditing] = useState(false);
  const noteEditor = useDescriptionEditor({
    markdown: lens?.description ?? '',
    maxBytes: MAX_LENS_DESCRIPTION_BYTES,
    editing: noteEditing,
    onEditing: setNoteEditing,
    onSave: async md => {
      const updated = await api.updateLens(name, { description: md });
      setLens(updated);
      onSaved();
    },
  });
  const [writeBranch, setWriteBranch] = useState('');
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState(false);
  // Rename draft + its typed confirmation. Local to the danger zone; cleared
  // when the pane switches lenses by the same effect that resets editReads.
  const [renameTo, setRenameTo] = useState('');
  const [renameConfirm, setRenameConfirm] = useState('');
  const [renaming, setRenaming] = useState(false);
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
    setRenameTo(''); setRenameConfirm('');
    // Always refresh the full detail — the list view can omit the description,
    // and getLens returns the canonical reads set.
    let cancelled = false;
    api.getLens(name).then(l => { if (!cancelled) setLens(l); }).catch(() => {});
    return () => { cancelled = true; };
  }, [name, initial]);

  // The write repo's NAME — what this screen labels rows with and what the
  // branch endpoints take. Membership itself is uid-keyed; `save` below is the
  // only place that needs that spelling.
  const write = lens?.write.name ?? '';
  const reads = lens?.reads ?? [];
  // The mount that makes this lens unreadable, or null. See the Browse button.
  const brokenMount = lens ? brokenLensMember(lens, repos) : null;
  // Name → registry uid for every repo this screen can mount, taken from the
  // same listing the rows are drawn from.
  const uidByName = new Map(repos.filter(r => r.uid).map(r => [r.name, r.uid as string]));

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

  // Mirrors RepoDetail's rename: the server's detail for a 409 (name taken by
  // another lens, or by a repo — they share one namespace) already tells the
  // user the one true thing to do, so it is surfaced verbatim like every
  // sibling mutation in this file rather than rewritten into invented copy.
  const rename = async () => {
    onError(''); setRenaming(true);
    try {
      const updated = await api.renameLens(name, renameTo);
      setRenameTo(''); setRenameConfirm('');
      // The pane is addressed by name, so it must follow the lens to its new
      // one — leaving it on the old name would show a 404 on the next read.
      onRenamed(updated.name);
    } catch (e) {
      onError(`rename failed: ${String(e)}`);
    } finally {
      setRenaming(false);
    }
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
    for (const r of reads) if (r.name !== write) { seed[r.name] = r.branch ?? ''; loadBranchData(r.name); }
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
    // Rows are kept by name; the wire takes uids. A repo the listing gave no
    // uid for cannot be identified to the server, and a 400 naming a uid the
    // reader never saw explains nothing — say which repo instead.
    const missing = Object.keys(editReads).filter(repo => repo !== write && !uidByName.get(repo));
    if (missing.length > 0) {
      onError(`save failed: cannot identify ${missing.join(', ')} — reload and try again.`);
      setBusy(false);
      return;
    }
    const readList: LensReadRef[] = Object.entries(editReads)
      .filter(([repo]) => repo !== write)
      .map(([repo, branch]) => {
        const uid = uidByName.get(repo) as string;
        return branch.trim() ? { uid, branch: branch.trim() } : { uid };
      });
    try {
      const updated = await api.updateLens(name, { reads: readList });
      setLens(updated);
      setEditReads(null);
      onSaved();
    } catch (e) { onError(`save failed: ${String(e)}`); }
    finally { setBusy(false); }
  };

  const sections: Section[] = [];

  if (lens?.description || !readOnly) {
    sections.push({
      id: 'note',
      title: 'Note',
      hint: `saved with the lens · up to ${Math.round(MAX_LENS_DESCRIPTION_BYTES / 1024)} KiB`,
      action: readOnly ? undefined : <DescriptionActions editor={noteEditor} label="Edit note" />,
      body: <DescriptionBody editor={noteEditor} readOnly={readOnly} />,
    });
  }

  // Write target — the one repo new facts land in. Same green card as a repo's
  // Agent branch: both answer "where do new facts go".
  sections.push({
    id: 'write-target',
    title: 'Write target',
    hint: 'every fact written through this lens lands here',
    body: (
      <div style={writeCard}>
        <div data-testid="lens-detail-write" style={{ display: 'flex', alignItems: 'center', gap: 8, fontSize: 13, flexWrap: 'wrap' }}>
          <RepoDot repo={write} />
          <b style={{ color: '#eee' }}>{write || '…'}</b>
          <BranchChip branch={reads.find(r => r.name === write)?.branch || writeBranch || 'agent branch'} />
        </div>
        <div style={{ fontSize: 11.5, color: '#777', marginTop: 6 }}>
          Fixed when the lens was created — a lens that changed where it writes would strand its own history.
        </div>
      </div>
    ),
  });

  const readsLocked = readOnly || busy || !!editReads;

  sections.push({
    id: 'read-mounts',
    title: 'Read mounts',
    hint: 'the union an agent sees, resolved top → bottom',
    tail: <span style={{ fontSize: 10.5, color: '#6a6a6a' }}>{reads.length}</span>,
    action: (
      <button type="button" className="k-bare" data-testid="lens-edit"
        title="Edit read mounts" aria-label="Edit read mounts"
        style={cardIconBtn(readsLocked)} disabled={readsLocked} onClick={beginEdit}>
        <PencilIcon color={readsLocked ? '#555' : '#888'} size={13} />
      </button>
    ),
    body: (
      <>
        {/* The union, in server order. The write repo shows here (tagged
            write · read), never as a separate line.

            Rows are NUMBERED because resolution order is load-bearing — the
            union resolves top to bottom and the first mount holding a path
            wins — and nothing in this UI said so before. Every row carries its
            branch, not only the pinned ones, so "follows the agent branch" and
            "pinned to a fixed branch" read as a difference rather than as an
            absence. */}
        <div style={card}>
          {reads.length === 0 && <div style={{ color: '#555', fontSize: 13 }}>None</div>}
          {reads.map((r, i) => {
            const isWrite = r.name === write;
            const pinned = !!r.branch && !isWrite;
            return (
              // Keyed by uid: a rename relabels the row rather than remounting
              // it, and two mounts can never share a uid.
              <div key={r.uid} data-testid={`lens-detail-read-${r.name}`}
                style={{ display: 'flex', alignItems: 'center', gap: 9, padding: '8px 2px', borderBottom: i === reads.length - 1 ? 'none' : '1px solid #242424' }}>
                <span style={mountOrdinal}>{i + 1}</span>
                <RepoDot repo={r.name} />
                <span style={{ fontSize: 13, color: '#eee', minWidth: 70 }}>{r.name}</span>
                <BranchChip branch={r.branch || (isWrite ? writeBranch : '') || 'agent branch'} />
                <div style={{ flex: 1 }} />
                {isWrite
                  ? <span style={writeReadTag}>write · read</span>
                  : <span style={readTag}>{pinned ? 'read · pinned' : 'read'}</span>}
              </div>
            );
          })}
        </div>
        {reads.some(r => r.branch && r.name !== write) && (
          <div style={{ fontSize: 11.5, color: '#777', marginTop: 8 }}>
            A pinned mount reads a fixed branch and never follows that repo's agent branch — the only way a lens shows stale content on purpose.
          </div>
        )}
      </>
    ),
  });

  sections.push({
    id: 'agent-access',
    title: 'Agent access',
    hint: 'how a client connects to this lens',
    body: <ConnectBody kind="lens" name={name} />,
  });

  sections.push({
    id: 'danger',
    title: 'Danger zone',
    danger: true,
    body: (
      <div style={dangerBox}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
          <span style={{ fontSize: 13, color: '#ddd' }}>Delete this lens</span>
          <div style={{ flex: 1 }} />
          <button type="button" data-testid="lens-delete" style={btn(readOnly || busy, 'danger')} disabled={readOnly || busy} onClick={() => setConfirming(true)}>
            Delete
          </button>
        </div>
        <div style={{ fontSize: 11.5, color: '#777', marginTop: 6 }}>
          The repositories it reads are not affected — only the lens that groups them.
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
        <div style={{ borderTop: '1px solid #3a2020', marginTop: 14, paddingTop: 14 }}>
          <div style={{ fontSize: 13, color: '#ddd', marginBottom: 4 }}>Rename this lens</div>
          {/* Differs from the repo warning: a lens has no history or mounts of
              its OWN to lose, and its identity (uid) is stable across the
              rename — the MCP binding pin is lens:<uid>, never the name (RFC
              §7.3) — so the one real consequence is the endpoint URL. */}
          <div data-testid="lens-rename-warning" style={{ fontSize: 11.5, color: '#777', marginBottom: 10 }}>
            Agent MCP endpoint URLs contain this lens's name and will stop resolving
            until each agent is reconfigured. The lens keeps its identity, so its
            mounts, and any in-flight agent queries against it, are unaffected.
          </div>
          <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap', alignItems: 'center' }}>
            <input
              data-testid="lens-rename-input"
              value={renameTo}
              onChange={e => setRenameTo(e.target.value)}
              placeholder="new-name"
              disabled={readOnly || renaming}
              style={renameInput}
            />
            <input
              data-testid="lens-rename-confirm"
              value={renameConfirm}
              onChange={e => setRenameConfirm(e.target.value)}
              placeholder={`type "${name}" to confirm`}
              disabled={readOnly || renaming}
              style={renameInput}
            />
            <button
              type="button"
              data-testid="lens-rename-submit"
              style={btn(readOnly || renaming || renameConfirm !== name || !renameTo || renameTo === name, 'danger')}
              disabled={readOnly || renaming || renameConfirm !== name || !renameTo || renameTo === name}
              onClick={rename}
            >
              {renaming ? 'Renaming…' : 'Rename'}
            </button>
          </div>
        </div>
      </div>
    ),
  });

  return (
    <div>
      <div style={detailHead}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 10, minWidth: 0 }}>
          <span style={lensIconBox}><LayersIcon color={LENS.accent} size={16} /></span>
          <div style={{ minWidth: 0 }}>
            <h3 style={{ margin: 0, fontSize: 16 }}>{name}</h3>
            <div style={{ fontSize: 12, color: '#777', marginTop: 1 }}>
              lens settings · {reads.length} mount{reads.length === 1 ? '' : 's'} · writes to {write || '…'}
            </div>
          </div>
        </div>
        {/* Same header grammar as RepoDetail: Browse alone. Delete moved to the
            Danger zone block, which is the last thing the ⋯ menu held. */}
        <div style={headActions}>
          {/* A lens binds all of its members or none, so one mount without a
              live store makes every read endpoint under the lens answer 503.
              GET /lenses/{lens} still answers 200 for it — this button is the
              only thing standing between the user and a surface that fails on
              arrival. It reports which mount, because that is what they have to
              repair, and the editor below is where they can. */}
          <button type="button" data-testid="lens-browse" style={browseBtn}
            disabled={brokenMount !== null}
            title={brokenMount === null ? undefined : `This lens cannot be read: its mount "${brokenMount}" has no store.`}
            onClick={() => onBrowse({ kind: 'lens', name })}>
            <LayersIcon color={LENS.accent} size={13} /> Browse
          </button>
        </div>
      </div>

      {/* Edit mode expands directly under the mounts it edits, so the card you
          are changing stays in view above the editor. It is pulled into view so
          its Save/Cancel are never left below the fold. */}
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
            // A repo with no live store cannot be MOUNTED — the server answers
            // 422 `repo not found: "<ksuid>"`, naming a uid the reader has
            // never seen — so its checkbox is disabled while it is off.
            //
            // But it is still RENDERED, and still toggleable while it is ON,
            // and that asymmetry is the whole point. beginEdit seeds editReads
            // from the lens's current reads by name and save re-sends the WHOLE
            // set, so filtering a broken repo out of this list would hide the
            // one row that has to be unchecked: the mount that is breaking the
            // lens. Every save would then re-send it and 422 forever, with no
            // control on screen capable of repairing it.
            const mountable = repoAvailable(r);
            const toggleable = mountable || on;
            return (
              <div key={r.name} style={{ ...editRow(on), opacity: toggleable ? 1 : 0.6 }}>
                <button type="button" data-testid={`lens-read-${r.name}`} style={editCheckbox(on)}
                  disabled={busy || !toggleable}
                  title={toggleable ? undefined : r.detail || `${r.name} has no store (${r.state}) and cannot be mounted.`}
                  onClick={() => toggleRead(r.name)} aria-label={r.name} aria-pressed={on}>
                  {on && <CheckMark color={LENS.text} />}
                </button>
                <RepoDot repo={r.name} />
                <span style={{ fontSize: 13, color: on ? '#eee' : '#aaa', minWidth: 76 }}>{r.name}</span>
                {!mountable && <RepoStateChip repo={r} />}
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

      <SettingsPage sections={sections} testid="lens-settings" />
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

// ConnectBody renders the "Agent access" block for a repo or a lens. It
// covers BOTH client families, because they wire up differently:
//   • Claude Code uses the `knomit-bridge claude init` scaffolding (skills +
//     hooks + .mcp.json).
//   • Claude Cowork, Claude Desktop, and any other stdio MCP client just
//     register knomit-bridge as an mcpServers entry — no `claude init`.
// The scope arg is --lens <name> for a lens, --repo <name> for a repo.
function ConnectBody({ kind, name, agentBranch }: { kind: 'repo' | 'lens'; name: string; agentBranch?: string }) {
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
    <div data-testid={`${kind}-connect`}>
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
    </div>
  );
}

// ── styles ──
// surface is the whole Manage mode: it fills the window below the top bar
// rather than floating over it. The old `overlay`/`panel` pair boxed the detail
// pane at roughly 650×540 — a README, a LICENSE, the remote, the agent branch
// and two connect snippets all queuing for that column is what this refactor
// exists to undo. No border, no radius, no shadow: it is not a card on top of
// the app, it IS the app right now.
const surface: React.CSSProperties = {
  flex: 1, minHeight: 0, display: 'flex', flexDirection: 'column',
  background: '#141414', color: '#eee',
};
const errBox: React.CSSProperties = { background: '#311', border: '1px solid #533', padding: 10, margin: '10px 18px 0', borderRadius: 4, fontSize: 13, flexShrink: 0 };
const body: React.CSSProperties = { display: 'flex', flex: 1, minHeight: 0 };
const listCol: React.CSSProperties = { width: 236, flexShrink: 0, borderRight: '1px solid #222', padding: 10, overflowY: 'auto' };
const detailCol: React.CSSProperties = { flex: 1, padding: 20, overflowY: 'auto' };
const detailHead: React.CSSProperties = { display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start' };
const sectionHeader: React.CSSProperties = { display: 'flex', alignItems: 'center', gap: 7, padding: '6px 8px 5px', marginTop: 6, borderBottom: '1px solid #242424' };
const sectionTitle: React.CSSProperties = { flex: 1, fontSize: 11, fontWeight: 600, letterSpacing: '0.08em', textTransform: 'uppercase', color: '#9a9a9a' };
const viewingTag: React.CSSProperties = { fontSize: 10, color: '#7c9', letterSpacing: '0.04em' };

// railTop fences the Overview row off from the entity lists below it: it is the
// one row in this column that is not a thing you own, so it gets a rule rather
// than sitting flush with the repositories.
const railTop: React.CSSProperties = {
  paddingBottom: 8, marginBottom: 4, borderBottom: '1px solid #242424',
};
const listItem = (active: boolean): React.CSSProperties => ({
  width: '100%', display: 'flex', justifyContent: 'space-between', alignItems: 'center',
  background: active ? '#22303a' : 'transparent', color: active ? '#eee' : '#bbb',
  border: 'none', borderRadius: 4, padding: '7px 10px', fontSize: 13, cursor: 'pointer', textAlign: 'left',
});
// archRow is the Archived entry. Shaped like listItem and NOT like
// sectionHeader: the section headers in this rail are inert, so a
// header-styled control would read as an empty section rather than something
// you can open. The archive glyph is kept for left-edge alignment — every
// sibling row starts with a mark, and a bare word at row height leaves the
// column ragged. Its selected tint is WARM rather than the live rows' blue, so
// the archive never reads as a live repository.
const archRow = (active: boolean): React.CSSProperties => ({
  width: '100%', display: 'flex', alignItems: 'center', gap: 8,
  background: active ? '#2a2620' : 'transparent', color: active ? '#e2d8c6' : '#8a8a8a',
  border: 'none', borderRadius: 4, padding: '7px 10px', fontSize: 12.5,
  cursor: 'pointer', textAlign: 'left',
});
const archCount: React.CSSProperties = {
  marginLeft: 'auto', fontSize: 11, color: '#5a5a5a', fontVariantNumeric: 'tabular-nums',
};
const plusBtn = (disabled: boolean, active: boolean): React.CSSProperties => ({
  display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
  width: 22, height: 22, borderRadius: 4,
  background: active ? '#1d4ed8' : 'transparent', color: disabled ? '#555' : active ? '#fff' : '#9a9a9a',
  border: '1px solid ' + (active ? '#1d4ed8' : '#333'), cursor: disabled ? 'default' : 'pointer', padding: 0,
});
// headActions is the page header's button cluster — just Browse now that every
// other action sits in the block that owns it. It never wraps under the title.
const headActions: React.CSSProperties = { display: 'flex', alignItems: 'center', gap: 8, flexShrink: 0 };
// The description block's height band. MAX caps the rendered scroller — a long
// manifest must not push the wiring blocks off the page — and MIN keeps a
// two-line description from rendering in a box too small to edit in. The editor
// inherits both by measuring the body it replaces, so the band governs reading
// and writing alike and the pencil never resizes anything. BLANK is the one
// height the band does not set: an empty description has no body to measure.
const DESC_BODY_MIN = 120;
const DESC_BODY_MAX = 360;
const DESC_BODY_BLANK = 240;
// descBtn is Save/Cancel sized to sit in a block heading. The height is exactly
// cardIconBtn's, so the heading row is the same height whether it holds a pencil
// or these two — which is the whole point of putting them there.
const descBtn = (disabled: boolean, variant: 'primary' | 'secondary' = 'secondary'): React.CSSProperties => ({
  ...btn(disabled, variant),
  display: 'inline-flex', alignItems: 'center',
  height: 24, padding: '0 10px', fontSize: 12,
});
// descTextarea edits raw markdown, so it is monospaced — a repo's README.md is
// a document, not a caption. Its height is NOT here: DescriptionBody sets it
// from the rendered body's measured height so the pencil never resizes the box.
const descTextarea: React.CSSProperties = {
  // `block`, not the default inline-block: as an inline box it sits on a text
  // baseline and its line box adds ~6px of descender under it, which is exactly
  // 6px of page movement when it replaces the rendered body.
  width: '100%', display: 'block', boxSizing: 'border-box', resize: 'vertical',
  background: '#0c0c0c', border: '1px solid #333', borderRadius: 5, color: '#ddd',
  padding: '9px 11px', fontSize: 12.5, lineHeight: 1.6,
  fontFamily: 'var(--k-font-mono)',
};
// licenseText renders LICENSE preformatted, not as markdown — a licence's
// single newlines are meaningful, and a markdown renderer reflows them away.
const licenseText: React.CSSProperties = {
  margin: 0, whiteSpace: 'pre-wrap', wordBreak: 'break-word',
  fontFamily: 'var(--k-font-mono)', fontSize: 11.5, lineHeight: 1.6,
  color: '#a0a0a8', maxHeight: 320, overflowY: 'auto',
};
// dangerBox tints the destructive block apart from every other body on the
// page. It is a box where the others are bare precisely because it should read
// as a fenced-off area rather than one more setting.
const dangerBox: React.CSSProperties = {
  background: '#161111', border: '1px solid #3a2626', borderRadius: 6, padding: '11px 13px',
};
// Monospace: a repo name is an identifier that appears in URLs, so it is typed
// and compared character by character.
const renameInput: React.CSSProperties = {
  background: '#141414', border: '1px solid #3a2020', borderRadius: 4,
  color: '#ddd', fontFamily: 'var(--k-font-mono)', fontSize: 12,
  padding: '5px 8px', minWidth: 170, outline: 'none',
};
// mountOrdinal numbers a lens's read mounts. The union resolves top to bottom,
// so the position IS information — this is a rank, not a bullet.
// mountedLensLink is the lens name inside a repo's Mounted-in block. Styled as
// a link rather than a chip because it navigates — to the lens, not the repo.
const mountedLensLink: React.CSSProperties = {
  background: 'none', border: 'none', padding: 0, cursor: 'pointer',
  fontSize: 13, color: '#c9c5f6', textAlign: 'left', minWidth: 70,
};
const mountOrdinal: React.CSSProperties = {
  width: 12, flexShrink: 0, fontFamily: 'var(--k-font-mono)', fontSize: 10, color: '#4a4a4a',
};

// browseBtn is the header's "Browse" button, shared by the lens and repo pages.
//
// It was a filled LENS.accent pill — right for a small pane where it was one of
// two controls, wrong on a full page, where a saturated fill made the quietest
// action on screen the loudest thing on it. It was also a LENS colour on a repo
// page, which said the wrong thing twice.
//
// It reads as a button in the page's own vocabulary now — the same bordered
// dark shape as Rebuild, Restore and Connect — and carries the entity's colour
// in its ICON instead, so it still says which thing it goes to. It does not
// need a fill to be found: it is the only control in the header.
const browseBtn: React.CSSProperties = {
  display: 'flex', alignItems: 'center', gap: 6, flexShrink: 0,
  background: '#242424', color: '#e6e6e6', border: '1px solid #3a3a3a', borderRadius: 5,
  padding: '6px 12px', fontSize: 13, cursor: 'pointer',
};
// archIconBox mirrors lensIconBox / repoIconBox in the warm archive tint.
const archIconBox: React.CSSProperties = {
  width: 30, height: 30, borderRadius: 7, flexShrink: 0,
  background: '#211d18', border: '1px solid #453a2a',
  display: 'flex', alignItems: 'center', justifyContent: 'center',
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
