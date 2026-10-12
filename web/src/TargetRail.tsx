import { useEffect, useRef, useState } from 'react';
import type { CSSProperties } from 'react';
import type { BrowseContext } from './state';
import { repoAvailable, brokenLensMember } from './api';
import type { RepoInfo, Lens, ArchivedRepo, RepoCreateStatus } from './api';
import { RepoStateChip } from './RepoStateChip';
import { RepoIndexChip } from './RepoIndexChip';
import { RepoOntologyChip } from './RepoOntologyChip';
import { RepoDot } from './settingsParts';
import { PendingCreateRow } from './PendingCreateRow';
import { useRepoCreates, activeCreateByRepo, createFlag, pendingCreates } from './useRepoCreates';
import { BookIcon, LayersIcon, ArchiveIcon, PlusIcon, ChevronDownIcon } from './icons';
import { LENS, noMouseFocus } from './utils';
import { remoteFailing } from './facets';
import type { PinnedTarget, DraftKind } from './facets';

interface Props {
  repos: RepoInfo[];
  lenses: Lens[];
  /** The browse context, which is what the selected row marks unless a pinned
   *  target or the draft is the one on screen. */
  context: BrowseContext;
  /** state.repo: the repo whose remote the app banner reports. */
  currentRepo: string;
  /** remoteErrorText(state) — the current repo's remote state. */
  remoteError: string;
  /** Instance read-only. Hides every remote health signal (the Remote
   *  settings are hidden there, and a dot pointing at a hidden block is a dead
   *  control), and disables both `+`: nothing can be created. */
  serverReadOnly: boolean;
  /** Called with a picked NORMAL target: an available repo or a readable lens. */
  onSelect: (ctx: BrowseContext) => void;
  /** Archived repos, under Repositories as one expandable row. */
  archived?: ArchivedRepo[];
  /** The settings-only target on screen, if any; its row is the selected one. */
  pinned?: PinnedTarget | null;
  /** Called with a picked settings-only target: an unavailable repo, a create
   *  not yet registered, an archived repo. */
  onPin?: (t: PinnedTarget) => void;
  /** Which draft rows exist. A draft row sits at the top of its section. */
  drafts?: { repo: boolean; lens: boolean };
  /** The draft on screen, if any; its row is the selected one. */
  draftShown?: DraftKind | null;
  /** A section's `+`. */
  onNewDraft?: (kind: DraftKind) => void;
  /** A draft row was picked. */
  onPickDraft?: (kind: DraftKind) => void;
  /** Non-zero while the app is asking for the search to take focus (⌘K). The
   *  rail focuses once and reports it through onFocusHandled, and the app
   *  clears the request — so a rail that mounts LATER (the toggle, ⌘B) does
   *  not steal the keyboard for a ⌘K that was already served. */
  focusRequest?: number;
  onFocusHandled?: () => void;
  /** A connect commit is in flight: every row and `+` refuses (and the list
   *  dims), because selecting anything would unmount the page the commit
   *  reports to. The search stays usable — it changes nothing on screen but
   *  the list. */
  locked?: boolean;
}

// TargetRail is the shell's left column: every repository and lens, one flat
// list each, and the way to switch between them. It replaced the top bar's
// repo/lens dropdown and the rail Manage used to have.
//
// It PUSHES the content — App renders it as the first column of the body row,
// never as an overlay — and when collapsed it is not rendered at all, so a
// closed rail costs nothing and hides nothing.
//
// Every kind of target has a row here. An available repo and a readable lens
// are NORMAL targets: picking one switches the browse context. A repo with no
// store, a create that is not registered yet and an archived repo have nothing
// to browse, so picking one PINS it over the context (Settings is all they
// have). A `+` inserts a draft row — the create form as a target of its own —
// at the top of its section. A lens with a broken mount is pinned too: every
// read under it answers 503, so it cannot be the context, but its Settings is
// where the broken mount is removed (the server will not archive a repo a lens
// mounts, so nothing else can clear it).
export function TargetRail({
  repos, lenses, context, currentRepo, remoteError, serverReadOnly, onSelect,
  archived = [], pinned = null, onPin, drafts = { repo: false, lens: false }, draftShown = null,
  onNewDraft, onPickDraft, focusRequest, onFocusHandled, locked = false,
}: Props) {
  const [query, setQuery] = useState('');
  const searchRef = useRef<HTMLInputElement>(null);
  // A create still working on a listed repo flags its row; one not yet
  // registered is a row of its own. The poller is already mounted app-wide.
  const creates = useRepoCreates();
  const activeCreates = activeCreateByRepo(creates);
  const pending = pendingCreates(creates, repos);

  // Archived expands in place. Pinning an archived repo opens it (a selected
  // row hidden in a closed group would be a selection nobody can see); the
  // reader can still close it afterwards. Adjusted during render rather than
  // in an effect, so the open group and the selected row arrive in one paint.
  const pinnedArchived = pinned?.kind === 'archived' ? pinned.id : null;
  const [archivedOpen, setArchivedOpen] = useState(pinnedArchived !== null);
  const [openedFor, setOpenedFor] = useState(pinnedArchived);
  if (pinnedArchived !== openedFor) {
    setOpenedFor(pinnedArchived);
    if (pinnedArchived !== null) setArchivedOpen(true);
  }

  useEffect(() => {
    if (!focusRequest) return;
    searchRef.current?.focus();
    onFocusHandled?.();
  }, [focusRequest, onFocusHandled]);

  // A pick hands the keyboard back to the page. Rows suppress mousedown focus
  // (noMouseFocus), so without this a ⌘K → type → click left focus in the
  // search, and the input guard kept every page shortcut dead.
  const release = () => searchRef.current?.blur();
  const pick = (ctx: BrowseContext) => { release(); onSelect(ctx); };
  const pin = (t: PinnedTarget) => { release(); onPin?.(t); };

  const q = query.trim().toLowerCase();
  const match = (name: string) => !q || name.toLowerCase().includes(q);
  // Sorted here rather than trusted from the server: the listing's order is
  // its own business, and a reader scanning for a name must find it where a
  // name goes, whatever state the thing behind it is in — a create not yet
  // registered included.
  type RepoItem = { kind: 'repo'; name: string; repo: RepoInfo } | { kind: 'pending'; name: string; job: RepoCreateStatus };
  const repoItems: RepoItem[] = [
    ...repos.map(r => ({ kind: 'repo' as const, name: r.name, repo: r })),
    ...pending.map(j => ({ kind: 'pending' as const, name: j.name, job: j })),
  ].sort(byName).filter(it => match(it.name));
  const shownLenses = [...lenses].sort(byName).filter(l => match(l.name));
  const shownArchived = [...archived].sort(byName).filter(a => match(a.name));
  // A search shows its archived matches without the reader having to open
  // the group: a match hidden behind a closed row is not found.
  const archivedRowsShown = archivedOpen || (q !== '' && shownArchived.length > 0);

  // Exactly one row is selected: the draft if it is on screen, else the
  // pinned target, else the context. None over the server pages.
  const onContext = draftShown === null && pinned === null;

  return (
    <nav data-testid="target-rail" aria-label="Repositories and lenses" style={rail}>
      <label style={searchBox}>
        <SearchGlyph />
        <input
          ref={searchRef}
          id="rail-search"
          data-testid="rail-search"
          type="text"
          value={query}
          onChange={e => setQuery(e.target.value)}
          placeholder="Find repository or lens"
          aria-label="Find repository or lens"
          spellCheck={false}
          autoComplete="off"
          style={searchInput}
        />
        <kbd style={kbd}>⌘K</kbd>
      </label>

      <div data-testid="rail-list" style={locked ? { ...list, opacity: 0.4 } : list}>
        <div data-testid="rail-repos">
          <div style={sectionHeader}>
            <BookIcon color="#7c9" size={13} />
            <span style={sectionTitle}>Repositories</span>
            <PlusButton testId="rail-new-repo" label="New repository" disabled={serverReadOnly || locked}
              active={draftShown === 'repo'} onClick={() => { release(); onNewDraft?.('repo'); }} />
          </div>
          {drafts.repo && (
            <DraftRow kind="repo" selected={draftShown === 'repo'} disabled={locked}
              onClick={() => { release(); onPickDraft?.('repo'); }} />
          )}
          {repoItems.map(it => {
            if (it.kind === 'pending') {
              const selected = pinned?.kind === 'pending' && pinned.createId === it.job.create_id;
              return (
                <PendingCreateRow key={`pending:${it.job.create_id}`} status={it.job} surface="rail"
                  // The rail's own row, so a create reads as the repository it
                  // is becoming: same geometry, same selection, one chip.
                  style={{ ...row(selected ? 'repo' : null, false), justifyContent: 'space-between' }}
                  active={selected}
                  disabled={locked}
                  onOpen={id => pin({ kind: 'pending', createId: id })} />
              );
            }
            const r = it.repo;
            const available = repoAvailable(r);
            const selected = available
              ? onContext && context.kind === 'repo' && context.repo === r.name
              : pinned?.kind === 'unavailable' && pinned.repo === r.name;
            const job = activeCreates.get(r.name);
            const jobFlag = job ? createFlag(job.state) : null;
            // The health dot. The CURRENT repo reads the app's own remote state
            // (the banner's source), so the row and the banner never disagree —
            // dismissing the banner clears both. Every other repo reads the
            // listing, which the `remote` repo-event keeps current.
            const failing = !serverReadOnly && (r.name === currentRepo ? Boolean(remoteError) : remoteFailing(r));
            return (
              <button
                key={r.name}
                type="button"
                data-testid={`rail-repo-${r.name}`}
                data-repo-state={r.state ?? 'active'}
                aria-current={selected || undefined}
                disabled={locked}
                // A registered repo with no live store is NOT a browse context:
                // every request a context switch fires would answer 409. It is
                // pinned instead, and its page says what is wrong and offers
                // the way out. It stays visible — vanishing is the failure the
                // server-side registry was built to end — and the chip says
                // which kind of broken it is.
                title={available ? undefined : r.detail || `This repository has no store (${r.state}).`}
                onMouseDown={noMouseFocus}
                onClick={locked ? undefined : available
                  ? () => pick({ kind: 'repo', repo: r.name })
                  : () => pin({ kind: 'unavailable', repo: r.name })}
                style={row(selected ? 'repo' : null, !available)}
              >
                <span style={{ display: 'inline-flex', flexShrink: 0, opacity: available ? 1 : 0.4 }}>
                  <RepoDot repo={r.name} />
                </span>
                <span data-testid={`rail-repo-name-${r.name}`} style={rowName}>{r.name}</span>
                {/* One row, one flag: while a create is still working on this
                    repo, that is the truth about it, and an index chip beside it
                    would report a detail of unsettled work. */}
                <span style={badges}>
                  {jobFlag
                    ? <span data-testid={`rail-create-chip-${r.name}`} style={createChip}>{jobFlag}</span>
                    : !available
                      ? <RepoStateChip repo={r} />
                      : <>
                          <RepoOntologyChip repo={r} compact />
                          <RepoIndexChip repo={r} compact />
                        </>}
                  {failing && (
                    <span
                      data-testid={`rail-health-${r.name}`}
                      role="img"
                      aria-label="remote failing"
                      title="The remote's last sync or push failed — see this repository's Remote settings"
                      style={healthDot}
                    />
                  )}
                </span>
              </button>
            );
          })}

          {/* Archived belongs UNDER Repositories: an archived repo is a
              repository in a state, not a third kind of thing. One row with a
              count, expanding in place; absent when nothing is archived, since
              a dead control is worse than an absent one. */}
          {archived.length > 0 && (
            <button
              type="button"
              data-testid="rail-archived"
              aria-expanded={archivedRowsShown}
              onMouseDown={noMouseFocus}
              disabled={locked}
              onClick={() => setArchivedOpen(o => !o)}
              style={archGroup}
            >
              <ArchiveIcon color="#7a6a5a" size={12} />
              <span style={{ flex: 1 }}>Archived</span>
              <span data-testid="rail-archived-count" style={archCount}>{archived.length}</span>
              <span style={{ display: 'inline-flex', transform: archivedRowsShown ? 'none' : 'rotate(-90deg)' }}>
                <ChevronDownIcon color="#5a5a5a" size={11} />
              </span>
            </button>
          )}
          {archivedRowsShown && shownArchived.map(a => {
            const selected = pinned?.kind === 'archived' && pinned.id === a.id;
            return (
              <button
                key={a.id}
                type="button"
                data-testid={`rail-archived-${a.id}`}
                aria-current={selected || undefined}
                disabled={locked}
                onMouseDown={noMouseFocus}
                onClick={locked ? undefined : () => pin({ kind: 'archived', id: a.id })}
                style={archRow(selected)}
              >
                <span data-testid={`rail-archived-name-${a.id}`} style={rowName}>{a.name}</span>
              </button>
            );
          })}
        </div>

        <div data-testid="rail-lenses">
          <div style={sectionHeader}>
            <LayersIcon color={LENS_ACCENT} size={13} />
            <span style={sectionTitle}>Lenses</span>
            <PlusButton testId="rail-new-lens" label="New lens" disabled={serverReadOnly || locked}
              active={draftShown === 'lens'} onClick={() => { release(); onNewDraft?.('lens'); }} />
          </div>
          {drafts.lens && (
            <DraftRow kind="lens" selected={draftShown === 'lens'} disabled={locked}
              onClick={() => { release(); onPickDraft?.('lens'); }} />
          )}
          {lenses.length === 0 && !drafts.lens && <div style={{ color: '#555', fontSize: 12, padding: '4px 10px' }}>None</div>}
          {shownLenses.map(l => {
            // A lens binds ALL its members or none, so one member without a live
            // store makes every read under it answer 503 — while GET /lenses/{lens}
            // still answers 200, so the app's resolve-and-rescue cannot catch it.
            // So it is never picked as the context here: it is pinned.
            const broken = brokenLensMember(l, repos);
            const available = broken === null;
            const selected = available
              ? onContext && context.kind === 'lens' && context.name === l.name
              : pinned?.kind === 'brokenLens' && pinned.name === l.name;
            return (
              <button
                key={l.name}
                type="button"
                data-testid={`rail-lens-${l.name}`}
                aria-current={selected || undefined}
                disabled={locked}
                title={available ? undefined : `This lens cannot be read: its mount "${broken}" has no store.`}
                onMouseDown={noMouseFocus}
                onClick={locked ? undefined : available
                  ? () => pick({ kind: 'lens', name: l.name })
                  : () => pin({ kind: 'brokenLens', name: l.name })}
                style={row(selected ? 'lens' : null, !available)}
              >
                <span style={{ width: 7, height: 7, borderRadius: '50%', background: LENS.accent, flexShrink: 0, opacity: available ? 1 : 0.4 }} />
                <span data-testid={`rail-lens-name-${l.name}`} style={rowName}>{l.name}</span>
              </button>
            );
          })}
        </div>
      </div>
    </nav>
  );
}

// A section's `+`: the manage rail's button, unchanged (the component wins
// over the mock). Lit while its draft is the target on screen.
function PlusButton({ testId, label, disabled, active, onClick }: {
  testId: string; label: string; disabled: boolean; active: boolean; onClick: () => void;
}) {
  return (
    <button type="button" data-testid={testId} title={label} aria-label={label}
      onMouseDown={noMouseFocus} disabled={disabled} onClick={onClick} style={plusBtn(disabled, active)}>
      <PlusIcon color="currentColor" size={14} />
    </button>
  );
}

// A draft row: the create form as a target, at the top of its section,
// italic in the section's accent so it never reads as a repository that
// exists.
function DraftRow({ kind, selected, disabled, onClick }: {
  kind: DraftKind; selected: boolean; disabled: boolean; onClick: () => void;
}) {
  return (
    <button type="button" data-testid={`rail-draft-${kind}`} aria-current={selected || undefined}
      disabled={disabled} onMouseDown={noMouseFocus} onClick={disabled ? undefined : onClick}
      style={draftRow(kind, selected)}>
      <span style={rowName}>{kind === 'repo' ? 'New repository' : 'New lens'}</span>
    </button>
  );
}

const byName = (a: { name: string }, b: { name: string }) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0);

const SearchGlyph = () => (
  <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden="true" style={{ flexShrink: 0 }}>
    <circle cx="11" cy="11" r="7" />
    <line x1="21" y1="21" x2="16.6" y2="16.6" />
  </svg>
);

// The spec's lens accent for the section glyph and the selected bar. The row
// dot keeps the app's LENS.accent, which every other lens dot uses.
const LENS_ACCENT = '#b9a6f0';
// The draft's blue: the creating tone (#8ab6d6 on #131d26) and its selected row.
const DRAFT_BLUE = '#8ab6d6';

const rail: CSSProperties = {
  width: 236, flexShrink: 0, minHeight: 0,
  display: 'flex', flexDirection: 'column',
  background: '#0e0e0e', borderRight: '1px solid #222',
};
const searchBox: CSSProperties = {
  margin: '10px 10px 4px', height: 26, flexShrink: 0,
  display: 'flex', alignItems: 'center', gap: 7,
  background: '#161616', border: '1px solid #242424', borderRadius: 5,
  padding: '0 9px', color: '#555', fontSize: 12, cursor: 'text',
};
const searchInput: CSSProperties = {
  flex: 1, minWidth: 0, background: 'transparent', border: 'none', outline: 'none',
  color: '#ddd', fontSize: 12, fontFamily: 'inherit', padding: 0,
};
const kbd: CSSProperties = {
  fontFamily: 'var(--k-font-mono)', fontSize: 9.5, color: '#4a4a4a',
  border: '1px solid #2a2a2a', borderRadius: 3, padding: '0 4px', flexShrink: 0,
};
const list: CSSProperties = {
  flex: 1, minHeight: 0, overflowY: 'auto', padding: '0 10px 10px',
};
// The manage rail's section header, unchanged: the component wins over the mock.
const sectionHeader: CSSProperties = { display: 'flex', alignItems: 'center', gap: 7, padding: '6px 8px 5px', marginTop: 6, marginBottom: 2, borderBottom: '1px solid #242424' };
const sectionTitle: CSSProperties = { flex: 1, fontSize: 11, fontWeight: 600, letterSpacing: '0.08em', textTransform: 'uppercase', color: '#9a9a9a' };
// A row is the manage rail's listItem in geometry. The selected treatment is
// the spec's: the green-tinted row with an inset left bar for a repo, the lens
// tint and bar for a lens.
const row = (selected: 'repo' | 'lens' | null, disabled: boolean): CSSProperties => ({
  width: '100%', display: 'flex', alignItems: 'center', gap: 8,
  border: 'none', borderRadius: 4, padding: '7px 10px', fontSize: 13, textAlign: 'left',
  fontFamily: 'inherit',
  cursor: 'pointer',
  ...(selected === 'repo'
    ? { background: '#1a231d', color: '#dfe9e2', boxShadow: 'inset 2px 0 0 -0.5px #7c9' }
    : selected === 'lens'
      ? { background: '#1d1a2c', color: '#e6defc', boxShadow: `inset 2px 0 0 -0.5px ${LENS_ACCENT}` }
      : { background: 'transparent', color: disabled ? '#6a6a6a' : '#bbb' }),
});
const draftRow = (kind: DraftKind, selected: boolean): CSSProperties => ({
  ...row(null, false),
  fontStyle: 'italic',
  ...(kind === 'repo'
    ? selected
      ? { background: '#14191f', color: '#cfe0f2', boxShadow: `inset 2px 0 0 -0.5px ${DRAFT_BLUE}` }
      : { color: DRAFT_BLUE }
    : selected
      ? { background: '#1d1a2c', color: '#e6defc', boxShadow: `inset 2px 0 0 -0.5px ${LENS_ACCENT}` }
      : { color: LENS_ACCENT }),
});
// The Archived group row: shaped like a row (a header-styled control would
// read as an empty section), in the archive's warm grey.
const archGroup: CSSProperties = {
  ...row(null, false), gap: 8, fontSize: 12.5, color: '#8a8a8a',
};
const archCount: CSSProperties = {
  fontSize: 11, color: '#5a5a5a', fontVariantNumeric: 'tabular-nums',
};
// An archived repo: indented under the group, in the archive tone; selected
// in the archive's own warm treatment so it never reads as a live repository.
const archRow = (selected: boolean): CSSProperties => ({
  ...row(null, false), fontSize: 12.5, paddingLeft: 26,
  ...(selected
    ? { background: '#221d16', color: '#e6d9c6', boxShadow: 'inset 2px 0 0 -0.5px #c4b29a' }
    : { color: '#8a7a6a' }),
});
const plusBtn = (disabled: boolean, active: boolean): CSSProperties => ({
  display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
  width: 22, height: 22, borderRadius: 4,
  background: active ? '#1d4ed8' : 'transparent', color: disabled ? '#555' : active ? '#fff' : '#9a9a9a',
  border: '1px solid ' + (active ? '#1d4ed8' : '#333'), cursor: disabled ? 'default' : 'pointer', padding: 0,
});
// The name is the one element that shrinks; the badges never do.
const rowName: CSSProperties = {
  flexGrow: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
};
const badges: CSSProperties = {
  display: 'inline-flex', alignItems: 'center', gap: 4, flexShrink: 0, marginLeft: 'auto',
};
const healthDot: CSSProperties = {
  width: 6, height: 6, borderRadius: '50%', background: '#f44336', flexShrink: 0,
};
// Same chip as the pending row's: one fact, one rendering.
const createChip: CSSProperties = {
  display: 'inline-flex', alignItems: 'center',
  fontSize: 9.5, lineHeight: 1.7, padding: '0 5px', borderRadius: 3,
  fontFamily: 'var(--k-font-mono)', whiteSpace: 'nowrap', flexShrink: 0,
  color: '#8ab6d6', background: '#131d26', border: '1px solid #244056',
};
