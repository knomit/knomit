import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react';
import { api, repoAvailable, type ArchivedRepo, type RepoInfo, type Lens } from './api';
import { RepoStateChip } from './RepoStateChip';
import { RepoIndexChip } from './RepoIndexChip';
import { RepoOntologyChip } from './RepoOntologyChip';
import { PendingCreateRow } from './PendingCreateRow';
import { useRepoCreates, refreshRepoCreates, pendingCreates, createFlag, activeCreateByRepo } from './useRepoCreates';
import { CreateRepoWizard } from './CreateRepoWizard';
import { CreateLensForm } from './CreateLensForm';
import { RemoteConnectWizard } from './RemoteConnectWizard';
import { LENS, noMouseFocus } from './utils';
import { BookIcon, ArchiveIcon, PlusIcon, LayersIcon, TreeIcon, BroadcastIcon, ScrollIcon, GlobeIcon, BotIcon, EyeIcon } from './icons';
import { ManageOverview } from './ManageOverview';
import { ManageSessions } from './ManageSessions';
import { ManageFleet } from './ManageFleet';
import { ManageLogs } from './ManageLogs';
import { ManageOAuth } from './ManageOAuth';
import { useClientSessionChanges } from './useClientSessionChanges';
import type { BrowseContext } from './state';
import { RepoSettings } from './RepoSettings';
import { LensSettings } from './LensSettings';
import { ArchivedSettings } from './ArchivedSettings';
import { UnavailableSettings } from './UnavailableSettings';
import { PendingCreateSettings } from './PendingCreateSettings';
import { CreateBlocked } from './CreateBlocked';
import { RepoDot } from './settingsParts';

// BrowseContext names the surface a Browse action should switch the app to:
// a whole repo, or a lens's read union. The canonical definition lives in
// state.ts (SET_CONTEXT consumes it); re-exported here for existing importers.
export type { BrowseContext };

interface Props {
  open: boolean;
  repos: RepoInfo[];
  currentRepo: string;
  /**
   * The INSTANCE accepts no mutations (demo mode) — the only read-only this
   * pane takes from outside. Required so every render has to answer it.
   *
   * Deliberately NOT the app's isReadOnly(state): that folds in the BROWSED
   * repo's subscription, branch and history anchor, and nothing here is about
   * the browsed repo — creating repos and lenses, the server log, archived
   * repos, lenses, and every repo's settings page are instance-scoped or
   * scoped to the entity on screen (#324). A repo's own read-only-ness is its
   * mode, which RepoSettings reads from that repo's details.
   *
   * When per-caller permissions reach the UI, they belong here: the server's
   * counterpart is writeGate (internal/web/readonly.go).
   */
  serverReadOnly: boolean;
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
  /** Switch the app to a BRANCH of the current repo — how entering an
   *  experiment works. Optional so a test harness can mount without it. */
  onEnterBranch?: (branch: string) => void;
  /** The branch the app is currently showing, so an experiment row can say
   *  "you are here". Empty before the status bootstrap resolves one. */
  currentBranch?: string;
  // True while a connect commit is in flight. Withholding the rail is not
  // enough on its own: the app's own exits (Escape, the top bar's step-out)
  // unmount this whole pane, and the wizard's contract is that nothing may
  // unmount it mid-commit. The parent MUST NOT close Manage while it is true.
  onBusyChange?: (busy: boolean) => void;
}

type Selection =
  | { kind: 'overview' }
  // Sessions has no entity to hang off — MCP clients cut across every repo and
  // lens — which is why it is a server PAGE in the tab strip and why the rail
  // is absent while it is open. See isServerPage below.
  | { kind: 'sessions' }
  // This instance's fleet membership (F09): server-level like Sessions, placed
  // between Knowledge and Sessions in the strip.
  | { kind: 'fleet' }
  // Logs is the server's own output: the same shape of page as Sessions, and
  // the second member of isServerPage.
  | { kind: 'logs' }
  // OAuth requests waiting for the operator (F19 phase 3b): server-level like
  // Sessions and Logs, and present only where the server has an OAuth issuer.
  | { kind: 'oauth' }
  // focus names a settings block to land on, set when arriving from an Overview
  // cell so the thing you clicked is what you see.
  | { kind: 'repo'; name: string; focus?: string }
  // A create in flight, opened from a pending row. It is a SELECTION and not
  // the wizard: the wizard is a form for describing a create that has not
  // started, and re-entering it to watch one that has would ask the user to
  // re-answer questions already answered. This page only observes.
  | { kind: 'create'; id: string }
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

// isServerPage answers "is this pane about the SERVER, or about the entities
// you own?" — and that question, not which axis was clicked last, is what
// decides whether the rail renders beside it.
//
// A future server-level tab (Audit/Logs, auth, jobs) adds its kind HERE and
// nowhere else. Overview is deliberately NOT one: it is a tab, but its content
// is the entity summary, so the rail is contextual there and both lead to the
// same repo and lens pages.
// See kb/decisions/web/manage/rail-only-for-entity-pages.
function isServerPage(v: Selection): boolean {
  return v?.kind === 'sessions' || v?.kind === 'logs' || v?.kind === 'oauth' || v?.kind === 'fleet';
}

export function RepoManager({ open, repos, currentRepo, currentBranch, serverReadOnly, hideRemoteConfig, onChanged, onBrowse, onEnterBranch, onBusyChange }: Props) {
  const [archived, setArchived] = useState<ArchivedRepo[]>([]);
  const [lenses, setLenses] = useState<Lens[]>([]);
  const [sel, setSel] = useState<Selection>(null);
  const [err, setErr] = useState('');

  // Creates in flight, shown in the rail as rows of their own.
  //
  // A create whose repo has ALREADY landed is dropped: the real row is below
  // it by then, and two rows for one name would read as two repositories. The
  // job record outlives the create by design (a client that lost the id must
  // still be able to find the outcome), so this list has to end each entry's
  // rail life itself rather than wait for the server to forget it.
  const creates = useRepoCreates();
  const railCreates = pendingCreates(creates, repos);
  // The jobs whose repo ALREADY EXISTS and is still being worked on. These are
  // exactly the ones pendingCreates drops, and dropping them was right for the
  // row (two rows for one name read as two repositories) but wrong for the
  // flag: from m.Add onwards the repo is listable, so the rail drew it as an
  // ordinary repository while it was still indexing — or, after a late cancel,
  // while it was being deleted underneath the reader. Flag the repo instead.
  const activeCreates = activeCreateByRepo(creates);

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
  // Layout, not passive, for the same reason the wizard's own relay is: this is
  // the middle hop, and a passive one here would reinstate exactly the window
  // the wizard's change closes. Pinned by App.manage.test.tsx.
  useLayoutEffect(() => {
    onBusyChange?.(connectBusy);
    return () => { onBusyChange?.(false); };
  }, [connectBusy, onBusyChange]);

  // Live-session count for the Sessions tab AND for the Overview line, which
  // takes it as a prop rather than fetching its own — one reader of this
  // number per tab.
  //
  // Read when Manage opens, on window focus, and whenever the server pushes a
  // change (the subscription below). While the Sessions page is open it
  // piggybacks on that page instead (onLiveCount below), so exactly one
  // sessions stream exists per tab in Manage mode. null means "not known"
  // (never arrived, or the call failed) and renders no badge.
  //
  // Declared up here with the other hooks, NOT beside the tab strip it feeds:
  // everything below `if (!open) return null` is past an early return, and a
  // hook there would run conditionally.
  const [liveSessions, setLiveSessions] = useState<number | null>(null);
  // The count comes from a page the server BOUNDS. When it was cut the number
  // is a lower bound, and the badge has to say so — a bare "500" beside a table
  // that admits it is showing 500 of more would be the one dishonest number on
  // the screen.
  const [liveTruncated, setLiveTruncated] = useState(false);
  // The fallback selection is never 'sessions', so the explicit selection is
  // the whole answer.
  const sessionsOpen = sel?.kind === 'sessions';
  // No repos means no tab strip, so there is nothing to badge — and the
  // zero-repo screen is the create form, which should not be making calls
  // about a server the reader has not populated yet.
  const wantLiveCount = repos.length > 0;
  // Generation guard, and it has to be a GENERATION rather than the effect's
  // `cancelled` flag. This read is dispatched from three places — mount, window
  // focus, and the change stream — and the Sessions page writes the same state
  // through onLiveCount whenever it is open. A boolean captured by one effect
  // run cannot speak for a request dispatched by another, so a read issued
  // before the page opened could settle after it and overwrite a FRESHER count
  // with an older one (or, on a transient failure, blank a badge the page had
  // just filled in).
  //
  // Every read takes the next generation; only the latest generation's
  // resolution OR rejection is allowed to write. Bumping it is also how the
  // effect cleanup retires reads that are still in flight.
  const liveCountGen = useRef(0);
  const readLiveCount = useCallback(() => {
    const gen = ++liveCountGen.current;
    api.listClientSessions()
      .then(r => {
        if (gen !== liveCountGen.current) return;
        setLiveSessions(r.sessions.filter(s => s.state === 'live').length);
        setLiveTruncated(r.truncated);
      })
      .catch(() => { if (gen === liveCountGen.current) { setLiveSessions(null); setLiveTruncated(false); } });
  }, []);
  useEffect(() => {
    if (!wantLiveCount) return;
    // Not while the Sessions page owns the number. The page reports its count
    // back through onLiveCount, so a read here would be a second request for
    // something already on its way — which is exactly what made opening
    // Sessions cost two list reads instead of one.
    if (!sessionsOpen) readLiveCount();
    // Skipped while the Sessions page is mounted, for the same reason.
    const onFocus = () => { if (!sessionsOpen) readLiveCount(); };
    window.addEventListener('focus', onFocus);
    return () => {
      // Retires anything still in flight from this run — the same mechanism,
      // so there is only one rule about which read may write.
      liveCountGen.current += 1;
      window.removeEventListener('focus', onFocus);
    };
  }, [sessionsOpen, wantLiveCount, readLiveCount]);
  // Live, but only while this header is the one that owns the number: the
  // Sessions page subscribes for itself and reports back through onLiveCount,
  // and two streams for one badge would be one too many.
  useClientSessionChanges(wantLiveCount && !sessionsOpen, readLiveCount);
  // STABLE identity, and that is the whole requirement. ManageSessions keeps
  // onLiveCount in its load callback's deps and runs its poll effect on that
  // callback, so an inline arrow here — a new function every render — re-runs
  // the page's effect and buys an extra list read each time RepoManager
  // renders. Opening Sessions cost two reads instead of one until this was a
  // useCallback.
  const handleLiveCount = useCallback((n: number | null, truncated?: boolean) => {
    // The page's count supersedes anything the header has in flight, so it
    // takes a generation too — otherwise a read dispatched before the page
    // opened could still land on top of it.
    liveCountGen.current += 1;
    setLiveSessions(n);
    // Carried with the count, not inferred: the page knows whether ITS read was
    // cut, and the badge must not keep a stale "+" from an earlier one.
    setLiveTruncated(truncated === true);
  }, []);

  // The Authorizations tab (F19 phase 3b). The list call is the probe: null
  // (404) means this server has no OAuth issuer — [oauth] unset, or the
  // desktop, which never opens the OAuth listener — and then there is no tab.
  // A refusal (403: say, loopback-anonymous without admin) still shows the
  // tab, because the page is where the reason is explained. Not polled while
  // the page is open: it polls for itself and reports through onCount.
  const [oauthAvailable, setOAuthAvailable] = useState(false);
  const [oauthCount, setOAuthCount] = useState<number | null>(null);
  const oauthOpen = sel?.kind === 'oauth';
  useEffect(() => {
    if (!open || serverReadOnly || !wantLiveCount || oauthOpen) return;
    let cancelled = false;
    const probe = () => {
      api.listOAuthPending()
        .then(list => {
          if (cancelled) return;
          setOAuthAvailable(list !== null);
          setOAuthCount(list === null ? null : list.length);
        })
        .catch(() => { if (!cancelled) { setOAuthAvailable(true); setOAuthCount(null); } });
    };
    probe();
    const t = setInterval(probe, 30_000);
    return () => { cancelled = true; clearInterval(t); };
  }, [open, serverReadOnly, wantLiveCount, oauthOpen]);
  const handleOAuthCount = useCallback((n: number) => setOAuthCount(n), []);

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

      {/* ── Server pages ──
          The pane has two navigation axes and they are deliberately
          perpendicular: PAGES about this server run across the top, the
          entities you OWN run down the rail. Drawn as a strip spanning both
          columns so the two never read as siblings — which is what the old
          unlabelled rows at the top of the rail did. Hidden with zero repos,
          the same rule the Overview row had: nothing to summarise, and the
          create form owns that screen.

          The strip is on EVERY Manage page. The rail is not: it renders only
          beside a pane whose content is entities (isServerPage above), so a
          server page gets the full width. */}
      {repos.length > 0 && (
        <div style={tabStrip} role="tablist" aria-label="Server pages">
          <button
            type="button"
            role="tab"
            data-testid="repomgr-overview"
            aria-selected={view.kind === 'overview'}
            onMouseDown={noMouseFocus}
            style={tabBtn(view.kind === 'overview')}
            disabled={connectBusy}
            onClick={() => setSel({ kind: 'overview' })}
          >
            {/* TreeIcon, not Home (this tab is not a home), and not the
                rail's own Book or Layers — the tab CONTAINS both repositories
                and lenses, so borrowing either section's glyph would claim it
                is one of them. Tree is otherwise only a sort-axis glyph in the
                browse-mode Library header, never the Library's own mark. */}
            <TreeIcon color="currentColor" size={12} /> Knowledge
          </button>
          <button
            type="button"
            role="tab"
            data-testid="repomgr-fleet"
            aria-selected={view.kind === 'fleet'}
            onMouseDown={noMouseFocus}
            style={tabBtn(view.kind === 'fleet')}
            disabled={connectBusy}
            onClick={() => setSel({ kind: 'fleet' })}
          >
            <BotIcon color="currentColor" size={12} /> Fleet
          </button>
          <button
            type="button"
            role="tab"
            data-testid="repomgr-sessions"
            aria-selected={view.kind === 'sessions'}
            onMouseDown={noMouseFocus}
            style={tabBtn(view.kind === 'sessions')}
            disabled={connectBusy}
            onClick={() => setSel({ kind: 'sessions' })}
          >
            <BroadcastIcon color="currentColor" size={12} /> Sessions
            {/* Absent at zero AND when the call failed: a "0" we cannot vouch
                for reads as "nobody is connected". */}
            {liveSessions !== null && liveSessions > 0 && (
              <span data-testid="repomgr-sessions-badge" style={tabBadge}>
                {liveSessions}{liveTruncated ? '+' : ''}
              </span>
            )}
          </button>
          {/* Absent under read-only, not disabled: the endpoint answers 403
              there — the server's own log is not part of the public demo, and
              free text has no useful redaction — so a tab would be a control
              that cannot work. Same rule as the rail on a server page. */}
          {!serverReadOnly && (
            <button
              type="button"
              role="tab"
              data-testid="repomgr-logs"
              aria-selected={view.kind === 'logs'}
              onMouseDown={noMouseFocus}
              style={tabBtn(view.kind === 'logs')}
              disabled={connectBusy}
              onClick={() => setSel({ kind: 'logs' })}
            >
              <ScrollIcon color="currentColor" size={12} /> Logs
            </button>
          )}
          {!serverReadOnly && oauthAvailable && (
            <button
              type="button"
              role="tab"
              data-testid="repomgr-oauth"
              aria-selected={view.kind === 'oauth'}
              onMouseDown={noMouseFocus}
              style={tabBtn(view.kind === 'oauth')}
              disabled={connectBusy}
              onClick={() => setSel({ kind: 'oauth' })}
            >
              <GlobeIcon color="currentColor" size={12} /> Authorizations
              {oauthCount !== null && oauthCount > 0 && (
                <span data-testid="repomgr-oauth-badge" style={tabBadge}>{oauthCount}</span>
              )}
            </button>
          )}
        </div>
      )}

      <div style={body}>
          {/* ── Master list ── */}
          {/* Absent, not dimmed, on a server page: nothing in it would be
              selected and nothing in it explains or filters a table about the
              server, so it would be a column of dead exits beside unrelated
              content — and a dead control is worse than an absent one. The
              detail section is flex:1, so it takes the freed width on its own.

              Dimmed as a whole while a connect commit runs: every row below is
              disabled, and a rail that looked live but refused every click
              would read as a broken pane rather than a held one. The reason is
              stated where the reader is looking — the connect page's own rail
              note — not repeated here. */}
          {!isServerPage(view) && (
          <nav style={connectBusy ? { ...listCol, opacity: 0.4 } : listCol}>
            <div style={sectionHeader}>
              <BookIcon color="#7c9" size={13} />
              <span style={sectionTitle}>Repositories</span>
              <button
                type="button"
                data-testid="repomgr-new"
                  onMouseDown={noMouseFocus}
                title="New repository"
                aria-label="New repository"
                style={plusBtn(serverReadOnly, view.kind === 'new')}
                disabled={serverReadOnly || connectBusy}
                onClick={() => setSel({ kind: 'new' })}
              ><PlusIcon color="currentColor" size={14} /></button>
            </div>
            {/* Creates in flight sit in the rail WHERE THEIR NAME SORTS, with
                a chip — the same rule an unavailable repo follows, and for the
                same reason: a reader scanning for a name must find it in the
                place a name goes, whatever state the thing behind it is in.
                Hiding a create until it succeeds is what made one vanish. */}
            {railCreates.map(c => (
              <PendingCreateRow key={c.create_id} status={c} surface="rail"
                // listItem — the SAME style the repository rows below use, not
                // a lookalike. A create is a repository being made, so its row
                // is a repository row with a flag on it; passing the style in
                // rather than copying it is what stops the two drifting.
                style={listItem(view.kind === 'create' && view.id === c.create_id)}
                active={view.kind === 'create' && view.id === c.create_id}
                disabled={connectBusy}
                onOpen={id => setSel({ kind: 'create', id })} />
            ))}
            {repos.map(r => {
              // The create still working on THIS repo, if any.
              const job = activeCreates.get(r.name);
              const jobFlag = job ? createFlag(job.state) : null;
              return (
              <button
                key={r.name}
                type="button"
                data-testid={`repomgr-item-${r.name}`}
                data-create-state={jobFlag ?? undefined}
                onMouseDown={noMouseFocus}
                // Lit for the repo's connect sub-page too: the reader is still
                // inside that repository, and a rail that went dark mid-flow
                // would be the takeover's context loss in miniature.
                style={listItem(
                  job
                    ? view.kind === 'create' && view.id === job.create_id
                    : (view.kind === 'repo' || view.kind === 'connect') && view.name === r.name)}
                disabled={connectBusy}
                // WHILE A CREATE IS STILL WORKING ON IT, the row opens the
                // CREATE page, not the settings page. The settings page offers
                // rename, archive and remote configuration for a repository
                // that is still being made — or, after a cancel, unmade — and
                // the thing the reader actually needs is the job's status and
                // its Cancel button.
                // The merged page: a repository being created IS a repository
                // page, gated by state. There is no separate create view for a
                // repo that exists.
                onClick={() => setSel({ kind: 'repo', name: r.name })}
              >
                {/* The repo's own deterministic hue, as in the top-bar switcher,
                    the Overview table and every RepoDot in the detail panes.
                    This rail was the one place a repo appeared WITHOUT it,
                    which left the lenses below looking like the only things
                    with an identity. Lenses share one accent because their
                    identity is "lens"; a repo's is its own. */}
                {/* THE NAME YIELDS LAST. It grows into the row and is the only
                    element allowed to shrink (min-width 0, ellipsis); the
                    badges beside it never shrink, so they are kept compact
                    instead — a percentage, a word, an icon, with the full
                    wording in each tooltip. The rail is a fixed 236px column,
                    and "indexing 1259/1259" plus "viewing" at full length left
                    a repository's name as "k…". */}
                <span style={{ display: 'flex', alignItems: 'center', gap: 8, minWidth: 0, flexGrow: 1 }}>
                  <RepoDot repo={r.name} />
                  <span data-testid={`repomgr-item-name-${r.name}`} style={railName}>{r.name}</span>
                </span>
                {/* A repo with no live store keeps its rail row: this is the one
                    surface that can still act on it, so hiding it here would
                    leave the user reading about a repository with nowhere to
                    go. The chip replaces "viewing", which cannot be true of a
                    repo the browse surface refuses to open. */}
                {/* THE CREATE FLAG WINS over the index chip and "viewing".
                    While a job is still working on this repo, "creating" and
                    "cancelling" are the truth about it, and an index chip
                    beside them would be reporting a detail of work whose
                    outcome is not settled — at worst "indexing" on a repo that
                    is being deleted. One row, one flag. */}
                <span data-testid={`repomgr-badges-${r.name}`} style={railBadges}>
                {jobFlag
                  ? <span data-testid={`repomgr-create-chip-${r.name}`} style={createChip}>{jobFlag}</span>
                  : !repoAvailable(r)
                    ? <RepoStateChip repo={r} />
                    : <>
                        <RepoOntologyChip repo={r} compact />
                        <RepoIndexChip repo={r} compact />
                        {r.name === currentRepo && (
                          <span
                            data-testid={`repomgr-viewing-${r.name}`}
                            role="img"
                            aria-label="viewing"
                            title="viewing — the web UI is currently browsing this repo"
                            style={viewingTag}
                          ><EyeIcon color="currentColor" size={12} /></span>
                        )}
                      </>}
                </span>
              </button>
              );
            })}

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
                style={plusBtn(serverReadOnly, view.kind === 'newLens')}
                disabled={serverReadOnly || connectBusy}
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
                <span style={{ display: 'flex', alignItems: 'center', gap: 7, minWidth: 0 }}>
                  <span style={{ width: 7, height: 7, borderRadius: '50%', background: LENS.accent, flexShrink: 0 }} />
                  <span style={railName}>{l.name}</span>
                </span>
              </button>
            ))}
          </nav>
          )}

          {/* ── Detail pane ── */}
          <section ref={detailRef} data-testid="manage-detail" style={detailCol}>
            {view.kind === 'overview' && (
              <ManageOverview
                repos={repos}
                lenses={lenses}
                archivedCount={archived.length}
                autoArchivedCount={archived.filter(a => a.reason?.source === 'system').length}
                hideRemoteConfig={hideRemoteConfig}
                serverReadOnly={serverReadOnly}
                onSelectRepo={(name, focus) => setSel({ kind: 'repo', name, focus })}
                onSelectLens={name => setSel({ kind: 'lens', name })}
                onNewRepo={() => setSel({ kind: 'new' })}
                onNewLens={() => setSel({ kind: 'newLens' })}
                onSelectSessions={() => setSel({ kind: 'sessions' })}
                liveSessions={liveSessions}
                onOpenCreate={id => setSel({ kind: 'create', id })}
                createSurface="overview"
              />
            )}
            {view.kind === 'fleet' && <ManageFleet />}
            {view.kind === 'sessions' && <ManageSessions onLiveCount={handleLiveCount} />}
            {view.kind === 'logs' && <ManageLogs />}
            {view.kind === 'oauth' && <ManageOAuth onCount={handleOAuthCount} />}
            {/* An unavailable repo gets its own pane rather than the settings
                page. RepoSettings's every read (description, agent branch, remote,
                mounts) resolves through the repo endpoints, which answer 409 for
                this repo — so the settings page would render as a wall of
                failures that never says the one thing worth knowing. */}
            {view.kind === 'repo' && !repoAvailable(repos.find(r => r.name === view.name) ?? {}) && (
              <UnavailableSettings
                key={view.name}
                repo={repos.find(r => r.name === view.name)!}
                readOnly={serverReadOnly}
                onArchived={() => { onChanged(); refresh(); setSel(null); }}
              />
            )}
            {view.kind === 'repo' && repoAvailable(repos.find(r => r.name === view.name) ?? {}) && (
              <RepoSettings
                key={view.name}
                name={view.name}
                lenses={lenses}
                focus={view.focus}
                onSelectLens={n => setSel({ kind: 'lens', name: n })}
                canArchive={!serverReadOnly}
                serverReadOnly={serverReadOnly}
                hideRemoteConfig={hideRemoteConfig}
                createJob={activeCreates.get(view.name) ?? null}
                onCancelCreate={async () => {
                  const job = activeCreates.get(view.name);
                  if (!job) return;
                  try { await api.cancelRepoCreate(job.create_id); } catch { /* the refresh tells the truth */ }
                  await refreshRepoCreates();
                }}
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
                onEnterBranch={onEnterBranch}
                currentBranch={currentBranch}
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
              <ArchivedSettings
                archived={archived}
                readOnly={serverReadOnly}
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
            {view.kind === 'create' && (
              <PendingCreateSettings
                createId={view.id}
                onClose={() => setSel({ kind: 'overview' })}
                // BROWSE it, do not open its settings. The reader asked for a
                // repository and the thing to do with one is read it — which is
                // also what a page reload gave them, and the reason they were
                // reloading. Leaving Manage is the point.
                // Hands over to the MERGED repo page — this is the manage
                // surface, and the reader came here for the job. The wizard's
                // own flow browses instead; it is a different arrival.
                onOpenRepo={name => { onChanged(); refresh(); setSel({ kind: 'repo', name }); }}
              />
            )}
            {view.kind === 'new' && serverReadOnly && <CreateBlocked what="repository" />}
            {view.kind === 'new' && !serverReadOnly && (
              <CreateRepoWizard
                // Fires the moment the repo EXISTS, not when the job ends — see
                // maybeEnterRepo. Browse rather than settings, for the same
                // reason: this is the reader arriving at their new repository.
                onDone={(name) => { onChanged(); refresh(); onBrowse({ kind: 'repo', repo: name }); }}
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
              <LensSettings
                key={view.name}
                lens={lenses.find(l => l.name === view.name)}
                name={view.name}
                repos={repos}
                readOnly={serverReadOnly}
                onDeleted={() => { onChanged(); refresh(); setSel(null); }}
                onSaved={() => { onChanged(); refresh(); }}
                // Same rename hint as RepoSettings's onRenamed (see its comment
                // above): the pane is keyed on `name`, so onChanged carries
                // {from, to} for the app to follow a browsed lens to its new
                // name instead of treating the old one as vanished.
                onRenamed={newName => { onChanged({ from: view.name, to: newName }); refresh(); setSel({ kind: 'lens', name: newName }); }}
                onBrowse={onBrowse}
                onError={setErr}
              />
            )}
            {view.kind === 'newLens' && serverReadOnly && <CreateBlocked what="lens" />}
            {view.kind === 'newLens' && !serverReadOnly && (
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
const sectionHeader: React.CSSProperties = { display: 'flex', alignItems: 'center', gap: 7, padding: '6px 8px 5px', marginTop: 6, borderBottom: '1px solid #242424' };
const sectionTitle: React.CSSProperties = { flex: 1, fontSize: 11, fontWeight: 600, letterSpacing: '0.08em', textTransform: 'uppercase', color: '#9a9a9a' };
const viewingTag: React.CSSProperties = { display: 'inline-flex', alignItems: 'center', color: '#7c9', flexShrink: 0 };
// A rail row's name and its badges. The name is the one element that shrinks;
// the badges never do, which is why every badge in the rail is compact.
const railName: React.CSSProperties = {
  flexGrow: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
};
const railBadges: React.CSSProperties = {
  display: 'inline-flex', alignItems: 'center', gap: 4, flexShrink: 0, marginLeft: 6,
};
// tabStrip spans rail AND detail: the server pages are not a column, they are
// the frame both columns sit inside.
const tabStrip: React.CSSProperties = {
  display: 'flex', gap: 2, padding: '8px 10px 0',
  borderBottom: '1px solid #222', flexShrink: 0,
};
// Same lit background as a selected rail row, so "this is the current thing"
// looks identical on both axes; the resting colour is the rail's own dim grey.
const tabBtn = (active: boolean): React.CSSProperties => ({
  display: 'flex', alignItems: 'center', gap: 7,
  background: active ? '#22303a' : 'transparent',
  color: active ? '#eee' : '#9a9a9a',
  border: 'none', borderRadius: '4px 4px 0 0',
  padding: '7px 12px', fontSize: 13, cursor: 'pointer',
  marginBottom: -1,
});
const tabBadge: React.CSSProperties = {
  fontSize: 10, padding: '0 5px', borderRadius: 8,
  background: '#1d2a22', color: '#4ade80', fontVariantNumeric: 'tabular-nums',
};
const listItem = (active: boolean): React.CSSProperties => ({
  width: '100%', display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 6,
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
// createChip flags a repository row whose create is still working. Same shape
// as the rail's other chips; blue, like PendingCreateRow's, because it is the
// same fact about the same job seen from the repository's side.
const createChip: React.CSSProperties = {
  display: 'inline-flex', alignItems: 'center',
  fontSize: 9.5, lineHeight: 1.7, padding: '0 5px', borderRadius: 3,
  fontFamily: 'var(--k-font-mono)', whiteSpace: 'nowrap', flexShrink: 0,
  color: '#8ab6d6', background: '#131d26', border: '1px solid #244056',
};
