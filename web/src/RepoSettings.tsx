import { useEffect, useState } from 'react';
import { api, isTerminalCreateState, RepoIndexingError, MAX_REPO_DESCRIPTION_BYTES, MAX_ARCHIVE_NOTE_BYTES, type Lens, type RepoCreateStatus } from './api';
import { createFlag } from './useRepoCreates';
import { CreateProgress } from './CreateProgress';
import { RemoteCard } from './RemoteStatus';
import { useRemote } from './useRemote';
import { LENS, repoHue } from './utils';
import { BookIcon, LayersIcon } from './icons';
import { btn, card, confirmBox, confirmInput, writeCard } from './manageStyles';
import { ExperimentsPanel } from './ExperimentsPanel';
import { PushedBranchesPanel } from './PushedBranchesPanel';
import { SettingsPage } from './SettingsPage';
import type { Section } from './SettingsPage';
import type { BrowseContext } from './state';
import { detailHead, headActions, dangerBox, renameInput, browseBtn, repoIconBox, writeReadTag, readTag } from './settingsStyles';
import { RepoDot, BranchChip, ConnectBody } from './settingsParts';
import { useDescriptionEditor, DescriptionActions, DescriptionBody } from './DescriptionEditor';

export function RepoSettings({ name, lenses, focus, canArchive, serverReadOnly, hideRemoteConfig, createJob, onCancelCreate, onArchived, onConnect, onChanged, onRenamed, onBrowse, onEnterBranch, currentBranch, onSelectLens, onError }: {
  name: string; canArchive: boolean; hideRemoteConfig: boolean;
  /** The instance accepts no mutations: every control on this page is off. */
  serverReadOnly: boolean;
  /** The create still working on this repo, if any. Puts the page in CREATING
   *  MODE — see the `creating` flag below. */
  createJob?: RepoCreateStatus | null;
  onCancelCreate?: () => void;
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
  onEnterBranch?: (branch: string) => void;
  currentBranch?: string;
  onError: (m: string) => void;
}) {
  const [agentBranch, setAgentBranch] = useState('');
  // '' for an ordinary repo; 'subscribe' means it follows a remote branch
  // read-only and has no agent branch of its own. null until this repo's
  // details answer — NOT '', which would claim "ordinary" before we know.
  const [repoMode, setRepoMode] = useState<'subscribe' | '' | null>(null);
  // Why this repo has no usable ontology ('' when it has one): from the
  // single GET, so the page states it even when the list row is stale.
  const [ontologyError, setOntologyError] = useState('');
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
  // A LICENSE that exists but is too large for the server to hand back (see
  // RepoDetails.license_oversize). Mutually exclusive with `license` being
  // non-empty. While this is true the block must render neither Add nor Edit
  // — opening a blank editor over a file the server never read is exactly
  // what let a Save silently destroy the original (see WriteLicense's
  // ErrLicenseTooLargeToReplace guard on the server, which now refuses that
  // write too).
  const [licenseOversize, setLicenseOversize] = useState(false);
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
  // The detail of a detach refused because the repo is indexing; see disconnect.
  const [detachIndexing, setDetachIndexing] = useState('');
  // The refusal's cancel-index link, followed by "Cancel indexing".
  const [cancelHref, setCancelHref] = useState('');
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
    setLicenseOversize(false);
    setRenameTo(''); setRenameConfirm('');
    setRepoMode(null);
    setOntologyError('');
    api.getRepo(name).then(r => {
      if (cancelled) return;
      setRepoMode(r.mode ?? '');
      setOntologyError(r.ontology_error ?? '');
      setDescription(r.description ?? '');
      setLicense(r.license ?? '');
      setLicenseOversize(!!r.license_oversize);
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
  // soon as it's queued, the re-index runs server-side. We confirm it started.
  // There is no "already running" refusal to translate: a click that lands on
  // an identical running rebuild is absorbed into it (200, same job envelope as
  // a new job's 201), and either way a rebuild is now running — which is what
  // the banner says.
  const rebuild = async () => {
    onError(''); setRebuilding(true); setRebuildMsg('');
    try {
      const branch = agentBranch || await api.getAgentBranch(name);
      await api.rebuild(name, branch);
      setRebuildMsg('✓ Rebuild started — re-indexing in the background.');
    } catch (e) {
      setRebuildMsg('');
      onError(`rebuild failed: ${String(e)}`);
    } finally {
      setRebuilding(false);
    }
  };
  // Archive asks first, for an optional note: the reason is kept with the
  // archive and shown on the Archived page, where a list of names alone does
  // not say why any of them is there.
  const [archiving, setArchiving] = useState(false);
  const [archiveNote, setArchiveNote] = useState('');
  const archiveNoteBytes = new TextEncoder().encode(archiveNote.trim()).length;
  const archive = async () => {
    onError(''); setBusy(true);
    try { await api.archiveRepo(name, archiveNote); onArchived(); }
    catch (e) { onError(`archive failed: ${String(e)}`); }
    finally { setBusy(false); }
  };
  // A detach is refused while the repo's index job runs. That refusal is the
  // one with a way through, so it stays in the confirm box — the server's own
  // detail plus "Cancel indexing" — instead of joining the generic error line;
  // once the cancel lands, Disconnect is simply pressed again.
  const disconnect = async () => {
    onError(''); setDetachIndexing(''); setBusy(true);
    try { await api.deleteOrigin(name); remote.reload(); setConfirming(null); onChanged(); }
    catch (e) {
      if (e instanceof RepoIndexingError) { setDetachIndexing(e.message); setCancelHref(e.cancelHref); }
      else onError(`disconnect failed: ${String(e)}`);
    }
    finally { setBusy(false); }
  };
  const cancelIndexing = async () => {
    onError(''); setBusy(true);
    try { await api.cancelIndex(cancelHref); setDetachIndexing(''); setCancelHref(''); onChanged(); }
    catch (e) { onError(`cancel indexing failed: ${String(e)}`); }
    finally { setBusy(false); }
  };
  // A rename's 409 is NOT rewritten into invented copy: the server's detail
  // for the "changed during rename" conflict already tells the user the one
  // true thing to do (re-read and retry), and every sibling mutation in this
  // file (archive, disconnect, restore, purge…) already
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

  // CREATING MODE. The repository exists and is readable, but the job that
  // made it is still working — indexing, then activating sync — and after a
  // late cancel it is on its way out again.
  //
  // This is ONE PAGE gated by state, not a second page beside this one. A
  // create used to get a bespoke view with its own header and its own idea of
  // what a repository looks like; the user's paradigm is the opposite —
  // "reuse the exact same UI paradigm, EXCEPT mark the repo as creating". So
  // the reader gets the repository page they already know, with the progress
  // on top and the parts that cannot honestly be offered yet withheld.
  //
  // WHAT IS WITHHELD AND WHY: every edit control (a README written into a repo
  // whose index is mid-build, or whose delete is pending, is a write with
  // nowhere to land), Rebuild (the index is already being built), the Remote
  // block (sync activation has not happened yet — the origin the page would
  // show is not yet the origin the repo has), and the Danger zone (rename and
  // archive on a half-made repository, one of which is a delete racing
  // another delete). Everything that is only READ stays: description,
  // licence, agent branch, agent access, the index chip, and Browse — the
  // repo is readable from the index phase on, which is the whole point.
  const creating = !!createJob && !isTerminalCreateState(createJob.state);
  // THIS repo's own read-only-ness: a subscription follows a remote branch and
  // owns no content, so nothing IN it can change — README and LICENSE commit to
  // an agent branch it does not have, and the server fails every such write.
  // Its registry config is another matter: rename, archive, rebuild and the
  // upstream stay open. Read from this repo's details, never from the app's
  // browse state, which describes whatever repo is BROWSED — not necessarily
  // this one (#324).
  const subscribed = repoMode === 'subscribe';
  // Until the mode is known, the page cannot tell a subscription from an
  // ordinary repo, so everything a subscription withholds is withheld: an
  // affordance that appears and then vanishes is one the reader may already
  // have clicked into a 409/503. A failed details read leaves it unknown, and
  // so locked — that page has no content to edit anyway.
  const modeUnknown = repoMode === null;
  // Content edits are locked by creating exactly as they are by read-only, so
  // every control that already asks "may I write?" needs no new question.
  const lockEdits = serverReadOnly || modeUnknown || subscribed || creating;

  if (creating && createJob) {
    sections.push({
      id: 'create',
      title: 'Being created',
      hint: 'this repository is still being made; some of its settings are unavailable until it finishes',
      body: (
        <div>
          <CreateProgress status={createJob} />
          <div style={{ marginTop: 12 }}>
            <button type="button" data-testid="create-cancel-button"
              style={btn(createJob.state === 'cancelling')}
              disabled={createJob.state === 'cancelling'}
              onClick={onCancelCreate}>
              {createJob.state === 'cancelling' ? 'Cancelling…' : 'Cancel create'}
            </button>
          </div>
        </div>
      ),
    });
  }

  // Shown whenever there is something to read OR the user could write one; a
  // read-only repo with no manifest has neither.
  if (description || !lockEdits) {
    sections.push({
      id: 'description',
      title: 'Description',
      hint: `README.md, committed to the agent branch · up to ${Math.round(MAX_REPO_DESCRIPTION_BYTES / 1024)} KiB`,
      action: lockEdits ? undefined : <DescriptionActions editor={descEditor} label="Edit description" />,
      body: <DescriptionBody editor={descEditor} readOnly={lockEdits} />,
    });
  }

  // Shown whenever there is something to read OR the user could write one —
  // the same rule the Description block above uses. A read-only repo with no
  // LICENSE gets no block: nothing to read, nothing to offer. An oversize
  // LICENSE counts as "something to read" even though its content did not
  // come down the wire — there IS a file, and the block must say so.
  if (license || licenseOversize || !lockEdits) {
    sections.push({
      id: 'license',
      title: 'License',
      hint: `LICENSE at the repo root · up to ${Math.round(MAX_REPO_DESCRIPTION_BYTES / 1024)} KiB`,
      // No control at all when oversize: neither "Edit" (there is nothing
      // here to seed the textarea with) nor "Add" (there already IS a
      // LICENSE — offering "Add" would imply there is not, and a blank draft
      // saved over it is exactly the silent-destruction bug this state
      // exists to prevent; the server refuses the write too, but the control
      // must not be there to invite it).
      action: lockEdits || licenseOversize ? undefined : (
        <DescriptionActions editor={licEditor} label={license ? 'Edit license' : 'Add license'} testIdPrefix="repo-license" />
      ),
      body: licenseOversize ? (
        <div data-testid="repo-license-oversize" style={{ fontSize: 12.5, color: '#888' }}>
          A LICENSE is present at the repo root but is too large to display or
          edit here.
        </div>
      ) : licEditing ? (
        // The EDITOR is the shared monospace textarea; only the read view
        // below differs from the description block.
        <DescriptionBody editor={licEditor} readOnly={lockEdits}
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
    // A subscription has no agent branch, so the heading has to name what it
    // DOES have: the branch it reads. Same block, same place — the question
    // "which branch is this repo about?" has an answer either way.
    title: subscribed ? 'Read branch' : 'Agent branch',
    hint: subscribed ? 'this repo follows a remote branch read-only' : 'where new facts are written',
    body: (
      <div style={writeCard}>
        <div data-testid="repo-detail-branch" style={{ display: 'flex', alignItems: 'center', gap: 8, fontSize: 13, flexWrap: 'wrap' }}>
          <RepoDot repo={name} />
          <b style={{ color: '#eee' }}>{name}</b>
          <BranchChip branch={agentBranch || '…'} />
          {subscribed
            ? <span data-testid="repo-readonly-badge" style={{ color: '#c9a', fontSize: 12 }}>read-only</span>
            : <span style={{ color: '#777', fontSize: 12 }}>— server-authoritative</span>}
        </div>
        {/* A repo with no usable ontology mounts and reads normally, so this
            is the one place that says why every write to it is refused. The
            server's message is quoted: it names the file and the fault. */}
        {ontologyError && (
          <div data-testid="repo-ontology-error" style={{ marginTop: 10, fontSize: 12.5, color: '#e2c07a', lineHeight: 1.6 }}>
            Read-only: this repository has no usable ontology, so every write to it is refused until one is
            committed on this branch.
            <div style={{
              marginTop: 6, fontSize: 12, color: '#c9c9c9', fontFamily: 'var(--k-font-mono)',
              background: '#131313', border: '1px solid #262626', borderRadius: 5, padding: '7px 10px',
            }}>{ontologyError}</div>
          </div>
        )}
      </div>
    ),
  });

  // Experiments sit immediately below the branch block, because that is the
  // question they answer: the block above says where new facts are written,
  // and this one is how you write somewhere else for a while. Not shown while
  // creating — the repo has no agent branch to fork from yet.
  if (!creating) {
    sections.push({
      id: 'experiments',
      title: 'Experiments',
      hint: 'work on a private branch, then commit or throw it away',
      body: (
        <ExperimentsPanel
          repo={name}
          currentBranch={currentBranch ?? ''}
          agentBranch={agentBranch}
          onEnterBranch={onEnterBranch ?? (() => {})}
          disabledReason={subscribed
            ? 'This repository is a subscription: it follows a remote branch read-only and has no agent branch to fork an experiment from.'
            : undefined}
        />
      ),
    });
  }

  // Pushed branches sit under Experiments: both are other branches whose facts
  // can be brought into the agent branch. A subscription has no agent branch
  // and so nothing can be merged into it; the block is absent rather than
  // disabled, because no peer can push to one either. Withheld while the mode
  // is unknown, like everything else a subscription lacks.
  if (!creating && !subscribed && !modeUnknown && agentBranch) {
    sections.push({
      id: 'pushed-branches',
      title: 'Pushed branches',
      hint: 'branches other instances pushed here; merge one into the agent branch',
      body: (
        <PushedBranchesPanel
          repo={name}
          agentBranch={agentBranch}
          onEnterBranch={onEnterBranch ?? (() => {})}
        />
      ),
    });
  }

  // The Remote block always exists (unless the server hides remote config), and
  // says which of THREE states it is in. "Not connected" now renders as content
  // with the Connect action on it, rather than as an absent card plus an offer
  // buried in an overflow menu — the block shows the state and carries the
  // action that changes it. A load FAILURE stays distinct from "not connected":
  // "we could not read this" is state, and collapsing the two would invite you
  // to overwrite a remote that is merely unreadable.
  // Not while creating: sync activation is the LAST thing a remote create
  // does, so until the job ends the origin this block would show is not yet
  // the origin the repository has.
  if (!hideRemoteConfig && !creating) {
    const connected = remote.loading || remote.origin || remote.err;
    sections.push({
      id: 'remote',
      title: 'Remote',
      // A subscription pulls and never pushes; saying otherwise here is the one
      // place the UI would still claim it writes upstream.
      hint: subscribed ? 'pulls from an origin; never pushes' : 'pull and push against an origin',
      tail: remote.origin ? <span style={{ width: 7, height: 7, borderRadius: '50%', background: remote.err ? '#f88' : '#7c9', display: 'inline-block' }} /> : undefined,
      body: connected ? (
        <>
          {/* agentBranch here means "this machine's agent branch", which drives
              the degenerate-upstream warning and the upstream chooser's filter
              (RemoteStatus). A subscription has none — and its agentBranch has
              fallen back to the READ branch, which equals the origin's branch,
              so passing it through would claim the repo is push-only when it is
              exactly the opposite. */}
          <RemoteCard repo={name} agentBranch={subscribed ? '' : agentBranch} readOnly={serverReadOnly} lockOrigin={subscribed || modeUnknown}
            state={remote} onConnect={onConnect} onDisconnect={() => setConfirming('disconnect')}
            onChanged={onChanged} />
          {confirming === 'disconnect' && (
            <div style={confirmBox}>
              <div style={{ fontSize: 13, marginBottom: 10 }}>Stop syncing and remove this remote? The repo stays as a local-only knowledge base — no facts are deleted.</div>
              {detachIndexing && (
                <div data-testid="disconnect-indexing" style={{ fontSize: 13, color: '#e2c07a', marginBottom: 10 }}>
                  Repo is indexing — {detachIndexing}.
                </div>
              )}
              <div style={{ display: 'flex', gap: 8 }}>
                <button type="button" data-testid="disconnect-confirm" style={btn(busy, 'danger')} disabled={busy} onClick={disconnect}>{busy ? 'Disconnecting…' : 'Disconnect'}</button>
                {detachIndexing && (
                  <button type="button" data-testid="disconnect-cancel-indexing" style={btn(busy)} disabled={busy} onClick={cancelIndexing}>Cancel indexing</button>
                )}
                <button type="button" style={btn(busy)} disabled={busy} onClick={() => { setConfirming(null); setDetachIndexing(''); }}>Cancel</button>
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
          <button type="button" data-testid="remote-connect" style={btn(serverReadOnly)} disabled={serverReadOnly} onClick={onConnect}>
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
    // No Rebuild while creating: the index is already being built, and a
    // second build queued behind it answers a question nobody asked.
    action: creating ? undefined : (
      <button type="button" data-testid="repo-rebuild" style={btn(serverReadOnly || rebuilding)} disabled={serverReadOnly || rebuilding} onClick={rebuild}>
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
  //
  // Withheld while creating: rename and archive on a repository that is still
  // being made are operations against a moving target, and archive during a
  // cancel is a delete racing another delete.
  if (!creating) sections.push({
    id: 'danger',
    title: 'Danger zone',
    danger: true,
    body: (
      <div style={dangerBox}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
          <span style={{ fontSize: 13, color: '#ddd' }}>Archive this repository</span>
          <div style={{ flex: 1 }} />
          {!archiving && (
            <button type="button" data-testid="repo-archive" style={btn(!canArchive || busy, 'danger')} disabled={!canArchive || busy}
              onClick={() => { setArchiveNote(''); setArchiving(true); }}>
              Archive…
            </button>
          )}
        </div>
        <div style={{ fontSize: 11.5, color: '#777', marginTop: 6 }}>
          Recoverable — it moves into Archived under Repositories, and nothing is deleted.
        </div>
        {archiving && (
          <div style={confirmBox}>
            <div style={{ fontSize: 13, marginBottom: 8 }}>Why are you archiving it? (optional, shown in Archived)</div>
            <input autoFocus data-testid="repo-archive-note" style={confirmInput} value={archiveNote}
              placeholder="e.g. moved to the team knowledge base" onChange={e => setArchiveNote(e.target.value)} />
            {archiveNoteBytes > MAX_ARCHIVE_NOTE_BYTES && (
              <div style={{ fontSize: 11.5, color: '#f88', marginTop: 6 }}>At most {MAX_ARCHIVE_NOTE_BYTES} bytes are kept.</div>
            )}
            <div style={{ display: 'flex', gap: 8, marginTop: 10 }}>
              <button type="button" data-testid="repo-archive-confirm"
                style={btn(busy || archiveNoteBytes > MAX_ARCHIVE_NOTE_BYTES, 'danger')}
                disabled={busy || archiveNoteBytes > MAX_ARCHIVE_NOTE_BYTES} onClick={archive}>Archive</button>
              <button type="button" style={btn(busy)} disabled={busy} onClick={() => setArchiving(false)}>Cancel</button>
            </div>
          </div>
        )}
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
              disabled={serverReadOnly || renaming}
              style={renameInput}
            />
            <input
              data-testid="repo-rename-confirm"
              value={renameConfirm}
              onChange={e => setRenameConfirm(e.target.value)}
              placeholder={`type "${name}" to confirm`}
              disabled={serverReadOnly || renaming}
              style={renameInput}
            />
            <button
              type="button"
              data-testid="repo-rename-submit"
              style={btn(serverReadOnly || renaming || renameConfirm !== name || !renameTo || renameTo === name, 'danger')}
              disabled={serverReadOnly || renaming || renameConfirm !== name || !renameTo || renameTo === name}
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
            {/* The flag goes exactly where the subtitle goes — the only
                difference between this header and an ordinary repository's. */}
            <div data-testid="repo-detail-subtitle" style={{ fontSize: 12, color: '#777', marginTop: 1 }}>
              {creating && createJob ? createFlag(createJob.state) : 'repository settings'}
            </div>
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

// licenseText renders LICENSE preformatted, not as markdown — a licence's
// single newlines are meaningful, and a markdown renderer reflows them away.
const licenseText: React.CSSProperties = {
  margin: 0, whiteSpace: 'pre-wrap', wordBreak: 'break-word',
  fontFamily: 'var(--k-font-mono)', fontSize: 11.5, lineHeight: 1.6,
  color: '#a0a0a8', maxHeight: 320, overflowY: 'auto',
};

// mountOrdinal numbers a lens's read mounts. The union resolves top to bottom,
// so the position IS information — this is a rank, not a bullet.
// mountedLensLink is the lens name inside a repo's Mounted-in block. Styled as
// a link rather than a chip because it navigates — to the lens, not the repo.
const mountedLensLink: React.CSSProperties = {
  background: 'none', border: 'none', padding: 0, cursor: 'pointer',
  fontSize: 13, color: '#c9c5f6', textAlign: 'left', minWidth: 70,
};
