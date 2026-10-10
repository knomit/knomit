import { useEffect, useRef, useState } from 'react';
import type { CSSProperties } from 'react';
import type { BrowseContext } from './state';
import { repoAvailable, brokenLensMember } from './api';
import type { RepoInfo, Lens } from './api';
import { RepoStateChip } from './RepoStateChip';
import { RepoIndexChip } from './RepoIndexChip';
import { RepoOntologyChip } from './RepoOntologyChip';
import { RepoDot } from './settingsParts';
import { useRepoCreates, activeCreateByRepo, createFlag } from './useRepoCreates';
import { BookIcon, LayersIcon } from './icons';
import { LENS, noMouseFocus } from './utils';
import { remoteFailing } from './facets';

interface Props {
  repos: RepoInfo[];
  lenses: Lens[];
  /** The browse context, which is what the selected row marks. */
  context: BrowseContext;
  /** state.repo: the repo whose remote the app banner reports. */
  currentRepo: string;
  /** remoteErrorText(state) — the current repo's remote state. */
  remoteError: string;
  /** Instance read-only. Hides every remote health signal (the Remote
   *  settings are hidden there, and a dot pointing at a hidden block is a dead
   *  control). */
  serverReadOnly: boolean;
  /** Called with the picked target. Only browsable targets call it. */
  onSelect: (ctx: BrowseContext) => void;
  /** Bumped by the app to ask for the search to take focus (⌘K). Any change,
   *  including the first value at mount, focuses it once. */
  focusRequest?: number;
  /** A connect commit is in flight: every row refuses (and the list dims),
   *  because selecting anything would unmount the page the commit reports to.
   *  The search stays usable — it changes nothing on screen but the list. */
  locked?: boolean;
}

// TargetRail is the shell's left column: every repository and lens, one flat
// list each, and the way to switch between them. It replaced the top bar's
// repo/lens dropdown.
//
// It PUSHES the content — App renders it as the first column of the body row,
// never as an overlay — and when collapsed it is not rendered at all, so a
// closed rail costs nothing and hides nothing.
//
// In this PR a row selects only a BROWSABLE target, exactly as the dropdown
// did. A repo with no store and a lens with a broken mount are listed and
// explained but disabled; the repository page in Manage is still where they are
// acted on, as are new repositories, archived ones and creates in flight.
export function TargetRail({ repos, lenses, context, currentRepo, remoteError, serverReadOnly, onSelect, focusRequest, locked = false }: Props) {
  const [query, setQuery] = useState('');
  const searchRef = useRef<HTMLInputElement>(null);
  // A create still working on a listed repo flags its row, as the manage rail
  // and the old switcher did. The poller is already mounted app-wide.
  const activeCreates = activeCreateByRepo(useRepoCreates());

  useEffect(() => {
    if (focusRequest) searchRef.current?.focus();
  }, [focusRequest]);

  const q = query.trim().toLowerCase();
  const match = (name: string) => !q || name.toLowerCase().includes(q);
  // Sorted here rather than trusted from the server: the listing's order is
  // its own business, and a reader scanning for a name must find it where a
  // name goes, whatever state the thing behind it is in.
  const shownRepos = [...repos].sort(byName).filter(r => match(r.name));
  const shownLenses = [...lenses].sort(byName).filter(l => match(l.name));

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
        <div style={sectionHeader}>
          <BookIcon color="#7c9" size={13} />
          <span style={sectionTitle}>Repositories</span>
        </div>
        {shownRepos.map(r => {
          const available = repoAvailable(r);
          const selected = context.kind === 'repo' && context.repo === r.name;
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
              disabled={!available || locked}
              // A registered repo with no live store is listed but NOT
              // selectable: every request this pick would fire answers 409. It
              // stays visible — vanishing is the failure the server-side
              // registry was built to end — and the chip says which kind of
              // broken it is.
              title={available ? undefined : r.detail || `This repository has no store (${r.state}).`}
              onMouseDown={noMouseFocus}
              onClick={available && !locked ? () => onSelect({ kind: 'repo', repo: r.name }) : undefined}
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

        <div style={sectionHeader}>
          <LayersIcon color={LENS_ACCENT} size={13} />
          <span style={sectionTitle}>Lenses</span>
        </div>
        {lenses.length === 0 && <div style={{ color: '#555', fontSize: 12, padding: '4px 10px' }}>None</div>}
        {shownLenses.map(l => {
          // A lens binds ALL its members or none, so one member without a live
          // store makes every read under it answer 503 — while GET /lenses/{lens}
          // still answers 200, so the app's resolve-and-rescue cannot catch it.
          // The refusal has to happen here.
          const broken = brokenLensMember(l, repos);
          const available = broken === null;
          const selected = context.kind === 'lens' && context.name === l.name;
          return (
            <button
              key={l.name}
              type="button"
              data-testid={`rail-lens-${l.name}`}
              aria-current={selected || undefined}
              disabled={!available || locked}
              title={available ? undefined : `This lens cannot be read: its mount "${broken}" has no store.`}
              onMouseDown={noMouseFocus}
              onClick={available && !locked ? () => onSelect({ kind: 'lens', name: l.name }) : undefined}
              style={{ ...row(selected ? 'lens' : null, !available), opacity: available ? 1 : 0.55 }}
            >
              <span style={{ width: 7, height: 7, borderRadius: '50%', background: LENS.accent, flexShrink: 0 }} />
              <span data-testid={`rail-lens-name-${l.name}`} style={rowName}>{l.name}</span>
            </button>
          );
        })}
      </div>
    </nav>
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
  cursor: disabled ? 'default' : 'pointer',
  ...(selected === 'repo'
    ? { background: '#1a231d', color: '#dfe9e2', boxShadow: 'inset 2px 0 0 -0.5px #7c9' }
    : selected === 'lens'
      ? { background: '#1d1a2c', color: '#e6defc', boxShadow: `inset 2px 0 0 -0.5px ${LENS_ACCENT}` }
      : { background: 'transparent', color: disabled ? '#6a6a6a' : '#bbb' }),
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
// Same chip as the manage rail's create flag: one fact, one rendering.
const createChip: CSSProperties = {
  display: 'inline-flex', alignItems: 'center',
  fontSize: 9.5, lineHeight: 1.7, padding: '0 5px', borderRadius: 3,
  fontFamily: 'var(--k-font-mono)', whiteSpace: 'nowrap', flexShrink: 0,
  color: '#8ab6d6', background: '#131d26', border: '1px solid #244056',
};
