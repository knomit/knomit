import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, within, cleanup, waitFor } from '@testing-library/react';
import { TargetRail } from './TargetRail';
import { api } from './api';
import type { RepoInfo, Lens, ArchivedRepo, RepoCreateStatus } from './api';
import type { BrowseContext } from './state';
import { __resetRepoCreatesForTest } from './useRepoCreates';

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>();
  return {
    ...actual,
    api: { ...actual.api, listRepoCreates: vi.fn().mockResolvedValue([]) },
  };
});

// Deliberately NOT in alphabetical order: the server sorts its listing, and a
// fixture that arrives sorted would let a rail that never sorts pass.
const repos: RepoInfo[] = [
  { name: 'ops-kb', uid: 'u-ops' },
  { name: 'core-kb', uid: 'u-core' },
  { name: 'b', uid: 'u-b' },
  { name: 'a', uid: 'u-a' },
  { name: 'ghost', uid: 'u-ghost', state: 'missing', detail: 'database file not found' },
];

const lenses: Lens[] = [
  { name: 'ops-desk', write: { uid: 'u-ops', name: 'ops-kb' }, reads: [{ uid: 'u-ops', name: 'ops-kb' }] },
  { name: 'dev', write: { uid: 'u-core', name: 'core-kb' }, reads: [{ uid: 'u-core', name: 'core-kb' }, { uid: 'u-b', name: 'b' }] },
];

function renderRail(over: Partial<React.ComponentProps<typeof TargetRail>> = {}) {
  const onSelect = vi.fn<(ctx: BrowseContext) => void>();
  const utils = render(
    <TargetRail
      repos={repos}
      lenses={lenses}
      context={{ kind: 'repo', repo: 'a' }}
      currentRepo="a"
      remoteError=""
      serverReadOnly={false}
      onSelect={onSelect}
      {...over}
    />,
  );
  return { ...utils, onSelect };
}

const repoNames = () =>
  screen.queryAllByTestId(/^rail-repo-name-/).map(el => el.textContent);
const lensNames = () =>
  screen.queryAllByTestId(/^rail-lens-name-/).map(el => el.textContent);

beforeEach(() => { __resetRepoCreatesForTest(); });
afterEach(() => { cleanup(); vi.clearAllMocks(); });

describe('TargetRail', () => {
  it('lists the repositories in alphabetical order', () => {
    renderRail();
    expect(repoNames()).toEqual(['a', 'b', 'core-kb', 'ghost', 'ops-kb']);
  });

  it('lists the lenses in alphabetical order', () => {
    renderRail();
    expect(lensNames()).toEqual(['dev', 'ops-desk']);
  });

  it('the search filters BOTH sections', () => {
    renderRail();
    fireEvent.change(screen.getByTestId('rail-search'), { target: { value: 'ops' } });
    expect(repoNames()).toEqual(['ops-kb']);
    expect(lensNames()).toEqual(['ops-desk']);
    // Named absences, each paired with the shown row above.
    expect(screen.queryByTestId('rail-repo-core-kb')).toBeNull();
    expect(screen.queryByTestId('rail-lens-dev')).toBeNull();
  });

  it('labels the search "Find repository or lens" and names its shortcut', () => {
    renderRail();
    const input = screen.getByTestId('rail-search') as HTMLInputElement;
    expect(input.placeholder).toBe('Find repository or lens');
    expect(screen.getByTestId('target-rail').textContent).toContain('⌘K');
  });

  it('clicking an available repo selects it, exactly once, with that value', () => {
    const { onSelect } = renderRail();
    fireEvent.click(screen.getByTestId('rail-repo-b'));
    expect(onSelect).toHaveBeenCalledTimes(1);
    expect(onSelect).toHaveBeenCalledWith({ kind: 'repo', repo: 'b' });
  });

  it('clicking a readable lens selects the lens context', () => {
    const { onSelect } = renderRail();
    fireEvent.click(screen.getByTestId('rail-lens-dev'));
    expect(onSelect).toHaveBeenCalledTimes(1);
    expect(onSelect).toHaveBeenCalledWith({ kind: 'lens', name: 'dev' });
  });

  // A broken lens cannot be the context (every read under it answers 503), but
  // its Settings is the only place its broken mount can be removed: the server
  // refuses to archive a repo a lens mounts. So the row is live and PINS.
  it('a lens with a mount that has no store is enabled, gives the reason, and pins', () => {
    const broken: Lens = { name: 'haunted', write: { uid: 'u-a', name: 'a' }, reads: [{ uid: 'u-ghost', name: 'ghost' }] };
    const onPin = vi.fn();
    const { onSelect } = renderRail({ lenses: [...lenses, broken], onPin });
    const row = screen.getByTestId('rail-lens-haunted') as HTMLButtonElement;
    expect(row.disabled).toBe(false);
    expect(row).toHaveAttribute('title', 'This lens cannot be read: its mount "ghost" has no store.');
    fireEvent.click(row);
    expect(onPin).toHaveBeenCalledWith({ kind: 'brokenLens', name: 'haunted' });
    expect(onSelect).not.toHaveBeenCalled();
    // Control: a healthy lens is still picked as the context, not pinned.
    fireEvent.click(screen.getByTestId('rail-lens-dev'));
    expect(onSelect).toHaveBeenCalledWith({ kind: 'lens', name: 'dev' });
    expect(onPin).toHaveBeenCalledTimes(1);
  });

  it('marks a pinned broken lens selected, and not the context', () => {
    const broken: Lens = { name: 'haunted', write: { uid: 'u-a', name: 'a' }, reads: [{ uid: 'u-ghost', name: 'ghost' }] };
    renderRail({ lenses: [...lenses, broken], context: { kind: 'repo', repo: 'b' }, pinned: { kind: 'brokenLens', name: 'haunted' } });
    expect(screen.getByTestId('rail-lens-haunted').getAttribute('aria-current')).toBe('true');
    expect(screen.getByTestId('rail-repo-b').getAttribute('aria-current')).toBeNull();
  });

  it('the selected repo row and the selected lens row carry their two different selected styles', () => {
    const { unmount } = renderRail({ context: { kind: 'repo', repo: 'b' } });
    const repoRow = screen.getByTestId('rail-repo-b');
    expect(repoRow).toHaveStyle({ background: '#1a231d', color: '#dfe9e2' });
    expect(repoRow.getAttribute('aria-current')).toBe('true');
    // Control: an unselected sibling does not wear it.
    expect(screen.getByTestId('rail-repo-a')).not.toHaveStyle({ background: '#1a231d' });
    unmount();

    renderRail({ context: { kind: 'lens', name: 'dev' } });
    const lensRow = screen.getByTestId('rail-lens-dev');
    expect(lensRow).toHaveStyle({ background: '#1d1a2c', color: '#e6defc' });
    expect(lensRow.getAttribute('aria-current')).toBe('true');
    // In a lens context no repo row is selected, even the write mount.
    expect(screen.getByTestId('rail-repo-core-kb').getAttribute('aria-current')).toBeNull();
  });

  it('is 236px wide and the rail background', () => {
    renderRail();
    expect(screen.getByTestId('target-rail')).toHaveStyle({ width: '236px', background: '#0e0e0e' });
  });
});


// The health dot takes over the cross-repo alert the Knowledge page used to
// give. A listed repo reads its own `remote` (kept current by the `remote`
// repo-event); the CURRENT repo reads remoteErrorText(state), the same source
// as the app banner, so the two never disagree about the repo being read.
describe('TargetRail — remote health dots', () => {
  const health: RepoInfo[] = [
    { name: 'a', uid: 'u-a', remote: { sync: 'ok', push: 'ok' } },
    { name: 'b', uid: 'u-b', remote: { sync: 'ok', push: 'error' } },
    { name: 'c', uid: 'u-c' },
    { name: 'd', uid: 'u-d', remote: { sync: 'error', push: '' } },
  ];

  it('marks a repo whose push or sync is failing, and only those', () => {
    renderRail({ repos: health, context: { kind: 'repo', repo: 'c' }, currentRepo: 'c' });
    expect(screen.getByTestId('rail-health-b')).toBeInTheDocument();
    expect(screen.getByTestId('rail-health-d')).toBeInTheDocument();
    expect(screen.queryByTestId('rail-health-a')).toBeNull();
    expect(screen.queryByTestId('rail-health-c')).toBeNull();
  });

  it('is a 6px red dot inside the row', () => {
    renderRail({ repos: health, context: { kind: 'repo', repo: 'c' }, currentRepo: 'c' });
    const dot = screen.getByTestId('rail-health-b');
    expect(screen.getByTestId('rail-repo-b').contains(dot)).toBe(true);
    expect(dot).toHaveStyle({ width: '6px', height: '6px', background: '#f44336' });
  });

  it("for the CURRENT repo follows the app's remote error, not the listing", () => {
    // a's listing says healthy, but the app holds a remote error for it.
    const { unmount } = renderRail({ repos: health, currentRepo: 'a', remoteError: 'push rejected' });
    expect(screen.getByTestId('rail-health-a')).toBeInTheDocument();
    unmount();
    // b's listing says failing, but b is current and the app holds no error
    // (the reader dismissed the banner): the row agrees with the banner.
    renderRail({ repos: health, context: { kind: 'repo', repo: 'b' }, currentRepo: 'b', remoteError: '' });
    expect(screen.queryByTestId('rail-health-b')).toBeNull();
    // Control: a non-current failing repo still shows its dot in the same render.
    expect(screen.getByTestId('rail-health-d')).toBeInTheDocument();
  });

  it('shows no dot at all on a read-only server, where the Remote settings are hidden', () => {
    renderRail({ repos: health, currentRepo: 'a', remoteError: 'push rejected', serverReadOnly: true });
    // Control: the rows themselves render.
    expect(screen.getByTestId('rail-repo-b')).toBeInTheDocument();
    expect(screen.queryAllByTestId(/^rail-health-/)).toHaveLength(0);
  });
});


// ── Edge targets and drafts (PR E) ───────────────────────────────────────────
// A row for every kind of target: a draft (the create form), a create that is
// not registered yet, a repo with no store, and the archived repos. All but the
// draft are PINNED over the context rather than becoming it.
describe('TargetRail — drafts', () => {
  it('Repositories + asks for a repository draft, and the draft row is the FIRST row of the section, selected', () => {
    const onNewDraft = vi.fn();
    const { rerender } = renderRail({ onNewDraft });
    fireEvent.click(screen.getByTestId('rail-new-repo'));
    expect(onNewDraft).toHaveBeenCalledTimes(1);
    expect(onNewDraft).toHaveBeenCalledWith('repo');

    rerender(
      <TargetRail repos={repos} lenses={lenses} context={{ kind: 'repo', repo: 'a' }} currentRepo="a"
        remoteError="" serverReadOnly={false} onSelect={() => {}} drafts={{ repo: true, lens: false }} draftShown="repo" />,
    );
    const section = screen.getByTestId('rail-repos');
    const rows = within(section).getAllByRole('button').filter(b => b.dataset.testid !== 'rail-new-repo');
    expect(rows[0]).toBe(screen.getByTestId('rail-draft-repo'));
    expect(rows[0]).toHaveTextContent('New repository');
    expect(rows[0]).toHaveAttribute('aria-current', 'true');
    expect(rows[0]).toHaveStyle({ fontStyle: 'italic', background: '#14191f', color: '#cfe0f2' });
    // The context row is NOT selected while the draft is the target on screen.
    expect(screen.getByTestId('rail-repo-a').getAttribute('aria-current')).toBeNull();
  });

  it('keeps the draft row while another row is selected, and picking it asks to show the draft', () => {
    const onPickDraft = vi.fn();
    renderRail({ drafts: { repo: true, lens: false }, draftShown: null, onPickDraft });
    const draft = screen.getByTestId('rail-draft-repo');
    expect(draft.getAttribute('aria-current')).toBeNull();
    // Unselected, it keeps the draft's blue italic rather than the selected tint.
    expect(draft).toHaveStyle({ fontStyle: 'italic', color: '#8ab6d6' });
    // The context row is the selected one now.
    expect(screen.getByTestId('rail-repo-a')).toHaveAttribute('aria-current', 'true');
    fireEvent.click(draft);
    expect(onPickDraft).toHaveBeenCalledWith('repo');
  });

  it('Lenses + asks for a lens draft, drawn in the lens accent at the top of Lenses', () => {
    const onNewDraft = vi.fn();
    renderRail({ onNewDraft, drafts: { repo: false, lens: true }, draftShown: 'lens' });
    fireEvent.click(screen.getByTestId('rail-new-lens'));
    expect(onNewDraft).toHaveBeenCalledWith('lens');
    const draft = screen.getByTestId('rail-draft-lens');
    expect(draft).toHaveTextContent('New lens');
    expect(draft).toHaveStyle({ fontStyle: 'italic', background: '#1d1a2c', color: '#e6defc' });
    const lensRows = within(screen.getByTestId('rail-lenses')).getAllByRole('button').filter(b => b.dataset.testid !== 'rail-new-lens');
    expect(lensRows[0]).toBe(draft);
    // No repository draft was asked for.
    expect(screen.queryByTestId('rail-draft-repo')).toBeNull();
  });

  it('both + are disabled on a read-only server, and enabled otherwise', () => {
    const { unmount } = renderRail();
    expect(screen.getByTestId('rail-new-repo')).toBeEnabled();
    expect(screen.getByTestId('rail-new-lens')).toBeEnabled();
    unmount();
    renderRail({ serverReadOnly: true });
    expect(screen.getByTestId('rail-new-repo')).toBeDisabled();
    expect(screen.getByTestId('rail-new-lens')).toBeDisabled();
  });

  it('both + are disabled while a connect commit holds the page', () => {
    renderRail({ locked: true });
    expect(screen.getByTestId('rail-new-repo')).toBeDisabled();
    expect(screen.getByTestId('rail-new-lens')).toBeDisabled();
  });
});

describe('TargetRail — pinned targets', () => {
  it('an unavailable repo row is explained and PINS the repo instead of selecting it as the context', () => {
    const onPin = vi.fn();
    const { onSelect } = renderRail({ onPin });
    const ghost = screen.getByTestId('rail-repo-ghost') as HTMLButtonElement;
    expect(ghost.disabled).toBe(false);
    expect(ghost).toHaveAttribute('title', 'database file not found');
    expect(within(ghost).getByTestId('repo-state-missing')).toHaveTextContent('missing');
    fireEvent.click(ghost);
    expect(onPin).toHaveBeenCalledTimes(1);
    expect(onPin).toHaveBeenCalledWith({ kind: 'unavailable', repo: 'ghost' });
    expect(onSelect).not.toHaveBeenCalled();
  });

  it('a pinned unavailable repo is the selected row, and the context row is not', () => {
    renderRail({ pinned: { kind: 'unavailable', repo: 'ghost' } });
    expect(screen.getByTestId('rail-repo-ghost')).toHaveAttribute('aria-current', 'true');
    expect(screen.getByTestId('rail-repo-a').getAttribute('aria-current')).toBeNull();
  });

  const archived: ArchivedRepo[] = [
    { id: 'id-scratch', name: 'scratch', origin: '', archivedAt: '2026-08-01T10:00:00Z' },
    { id: 'id-q3', name: 'q3-planning', origin: '', archivedAt: '2026-08-12T10:00:00Z' },
    { id: 'id-old', name: 'old-wiki', origin: '', archivedAt: '2026-07-01T10:00:00Z' },
  ];
  const archivedNames = () => screen.queryAllByTestId(/^rail-archived-name-/).map(el => el.textContent);

  it('Archived shows its count and expands in place to the archived names, sorted', () => {
    renderRail({ archived });
    const toggle = screen.getByTestId('rail-archived');
    expect(within(toggle).getByTestId('rail-archived-count')).toHaveTextContent(/^3$/);
    expect(archivedNames()).toEqual([]);
    fireEvent.click(toggle);
    expect(archivedNames()).toEqual(['old-wiki', 'q3-planning', 'scratch']);
    // Indented, in the archive tone.
    expect(screen.getByTestId('rail-archived-id-q3')).toHaveStyle({ color: '#8a7a6a', paddingLeft: '26px' });
    fireEvent.click(toggle);
    expect(archivedNames()).toEqual([]);
  });

  it('an archived row pins that archived repo, and is drawn selected while it is pinned', () => {
    const onPin = vi.fn();
    const { unmount } = renderRail({ archived, onPin });
    fireEvent.click(screen.getByTestId('rail-archived'));
    fireEvent.click(screen.getByTestId('rail-archived-id-q3'));
    expect(onPin).toHaveBeenCalledWith({ kind: 'archived', id: 'id-q3' });
    unmount();
    // Pinned: open without a click, and the row carries the archive selection.
    renderRail({ archived, pinned: { kind: 'archived', id: 'id-q3' } });
    const row = screen.getByTestId('rail-archived-id-q3');
    expect(row).toHaveAttribute('aria-current', 'true');
    expect(row).toHaveStyle({ background: '#221d16', color: '#e6d9c6' });
    expect(screen.getByTestId('rail-archived-id-old').getAttribute('aria-current')).toBeNull();
  });

  it('renders no Archived row when nothing is archived', () => {
    renderRail({ archived: [] });
    // Control: the Repositories section itself is there.
    expect(screen.getByTestId('rail-repos')).toBeInTheDocument();
    expect(screen.queryByTestId('rail-archived')).toBeNull();
  });
});

describe('TargetRail — creates in flight', () => {
  function job(over: Partial<RepoCreateStatus> = {}): RepoCreateStatus {
    return { create_id: 'c1', name: 'b-kb', mode: 'subscribe', state: 'running', ...over };
  }
  const abc: RepoInfo[] = [{ name: 'c-kb', uid: 'u-c' }, { name: 'a-kb', uid: 'u-a' }];

  it('a create not yet registered sorts where its name goes, between a-kb and c-kb, with its chip', async () => {
    vi.mocked(api.listRepoCreates).mockResolvedValue([job()]);
    renderRail({ repos: abc, context: { kind: 'repo', repo: 'a-kb' }, currentRepo: 'a-kb' });
    const row = await screen.findByTestId('pending-create-rail-b-kb');
    expect(screen.getByTestId('pending-create-chip-rail-b-kb')).toHaveTextContent('creating');
    const order = within(screen.getByTestId('rail-repos')).getAllByRole('button')
      .map(b => b.dataset.testid).filter(id => id !== 'rail-new-repo');
    expect(order).toEqual(['rail-repo-a-kb', 'pending-create-rail-b-kb', 'rail-repo-c-kb']);
    // No inline controls on it: the page carries those.
    expect(within(row).queryAllByRole('button')).toHaveLength(0);
  });

  it('clicking a pending create pins it', async () => {
    vi.mocked(api.listRepoCreates).mockResolvedValue([job()]);
    const onPin = vi.fn();
    renderRail({ repos: abc, onPin });
    fireEvent.click(await screen.findByTestId('pending-create-rail-b-kb'));
    expect(onPin).toHaveBeenCalledWith({ kind: 'pending', createId: 'c1' });
  });

  it('draws a cancelling create, and never a cancelled one', async () => {
    vi.mocked(api.listRepoCreates).mockResolvedValue([job({ state: 'cancelling' }), job({ create_id: 'c2', name: 'x-kb', state: 'cancelled' })]);
    renderRail({ repos: abc });
    expect(await screen.findByTestId('pending-create-chip-rail-b-kb')).toHaveTextContent('cancelling');
    expect(screen.queryByTestId('pending-create-rail-x-kb')).toBeNull();
  });

  it('the create flag wins over the index chip, and the repo keeps one row', async () => {
    vi.mocked(api.listRepoCreates).mockResolvedValue([job({ name: 'c-kb', step: 'index' })]);
    renderRail({ repos: [{ name: 'a-kb', uid: 'u-a' }, { name: 'c-kb', uid: 'u-c', index_state: 'indexing', index_done: 1, index_total: 4 }] });
    expect(await screen.findByTestId('rail-create-chip-c-kb')).toHaveTextContent('creating');
    expect(within(screen.getByTestId('rail-repo-c-kb')).queryByTestId('repo-index-indexing')).toBeNull();
    expect(screen.queryByTestId('pending-create-rail-c-kb')).toBeNull();
    expect(screen.getAllByTestId('rail-repo-c-kb')).toHaveLength(1);
  });

  it('drops the pending row once its repo is listed', async () => {
    vi.mocked(api.listRepoCreates).mockResolvedValue([job({ state: 'done', repo: { name: 'b-kb', uid: 'u-b' } })]);
    renderRail({ repos: [...abc, { name: 'b-kb', uid: 'u-b' }] });
    await waitFor(() => expect(api.listRepoCreates).toHaveBeenCalled());
    expect(screen.queryByTestId('pending-create-rail-b-kb')).toBeNull();
    expect(screen.getByTestId('rail-repo-b-kb')).toBeInTheDocument();
  });

  it('a pinned pending create is the selected row', async () => {
    vi.mocked(api.listRepoCreates).mockResolvedValue([job()]);
    renderRail({ repos: abc, pinned: { kind: 'pending', createId: 'c1' } });
    expect(await screen.findByTestId('pending-create-rail-b-kb')).toHaveAttribute('aria-current', 'true');
  });

  // THE USER'S REPORT: create "agentic-engineering-kb", rename it, and a rail
  // showed BOTH the real repo and a row under the old name. The rail matches a
  // finished create to its repo by uid, not by name.
  it('follows a created repo through a rename: one row, under the new name', async () => {
    vi.mocked(api.listRepoCreates).mockResolvedValue([
      job({ name: 'agentic-engineering-kb', state: 'done', repo: { name: 'agentic-engineering-kb', uid: 'uid-ae' } }),
    ]);
    renderRail({ repos: [{ name: 'a-kb', uid: 'u-a' }, { name: 'agentic-engineering', uid: 'uid-ae' }] });
    await waitFor(() => expect(api.listRepoCreates).toHaveBeenCalled());
    expect(screen.getByTestId('rail-repo-agentic-engineering')).toBeInTheDocument();
    expect(screen.queryByTestId('pending-create-rail-agentic-engineering-kb')).toBeNull();
    expect(document.querySelectorAll('[data-create-state]')).toHaveLength(0);
  });
});

// THE NAME IS THE LAST THING ON A RAIL ROW TO GIVE WAY. The rail is a fixed
// 236px column, so the badges are compact (a percentage, a word, an icon, with
// the full wording in each tooltip) and the name is the element that shrinks.
describe('TargetRail — row badges', () => {
  const long = 'agentic-engineering-knowledge';
  const indexing: RepoInfo = { name: long, uid: 'uid-long', index_state: 'indexing', index_done: 1259, index_total: 1259 };

  it('keeps the index chip compact with its full wording in the tooltip, and lets the name shrink', () => {
    renderRail({ repos: [indexing], context: { kind: 'repo', repo: long }, currentRepo: long });
    const row = screen.getByTestId(`rail-repo-${long}`);
    const chip = within(row).getByTestId('repo-index-indexing');
    expect(chip).toHaveTextContent(/^100%$/);
    expect(chip.getAttribute('title')).toContain('indexing 1259/1259');
    const name = screen.getByTestId(`rail-repo-name-${long}`);
    expect(name.style.minWidth).toBe('0px');
    expect(name.style.textOverflow).toBe('ellipsis');
    expect(name.style.flexGrow).toBe('1');
  });

  it('renders an index error as a one-word chip', () => {
    renderRail({ repos: [{ ...indexing, index_state: 'error', index_reason: 'indexing cancelled' }] });
    const chip = within(screen.getByTestId(`rail-repo-${long}`)).getByTestId('repo-index-error');
    expect(chip).toHaveTextContent(/^error$/);
    expect(chip.getAttribute('title')).toContain('indexing cancelled');
  });

  // A repo with no usable ontology is open but refuses writes: its row carries
  // a lock with the server's message in the tooltip; a healthy row is bare.
  it('chips a repo with no usable ontology with the server message, and leaves a healthy row bare', () => {
    const msg = 'ontology at .knomit/ontology.yaml does not parse: ontology id is required';
    renderRail({ repos: [{ name: 'core', uid: 'u-core' }, { name: 'garbled', uid: 'u-g', state: 'active', ontology_error: msg }] });
    const chip = within(screen.getByTestId('rail-repo-garbled')).getByTestId('repo-ontology-error-garbled');
    expect(chip).toHaveAttribute('aria-label', 'read-only');
    expect(chip.getAttribute('title')).toContain(msg);
    expect(within(screen.getByTestId('rail-repo-core')).queryByTestId('repo-ontology-error-core')).toBeNull();
  });
});

// The Fleet section (PR 7): one row once in a fleet (the fleet repository
// moves there from Repositories), else the `+` that opens New fleet, which
// GET /fleet's `local` alone enables.
describe('TargetRail: the Fleet section', () => {
  const standalone = { state: 'standalone' as const, agent_id: 'a', local: true, host: { listener_on: false, external_addresses: [] }, fleet_urls: [] };
  const inFleet = { ...standalone, state: 'registered' as const, fleet_repo: 'fleet', master: true };

  it('standalone on the desktop: "Not in a fleet" and a working + that opens the fleet draft', () => {
    const onNewDraft = vi.fn();
    renderRail({ fleet: standalone, onNewDraft });
    expect(screen.getByTestId('rail-fleet-none')).toHaveTextContent(/^Not in a fleet$/);
    const plus = screen.getByTestId('rail-new-fleet');
    expect(plus).toHaveAttribute('title', 'Create or join a fleet');
    expect(plus).toBeEnabled();
    fireEvent.click(plus);
    expect(onNewDraft).toHaveBeenCalledWith('fleet');
  });

  // SABOTAGE: enable the + regardless of `local` -> red.
  it('in a browser over TCP (local false): the + is off and the line says where to go', () => {
    renderRail({ fleet: { ...standalone, local: false } });
    expect(screen.getByTestId('rail-new-fleet')).toBeDisabled();
    expect(screen.getByTestId('rail-fleet-none')).toHaveTextContent('Not in a fleet — create or join from the desktop app');
  });

  it('the draft row reads New fleet', () => {
    renderRail({ fleet: standalone, drafts: { repo: false, lens: false, fleet: true }, draftShown: 'fleet' });
    expect(screen.getByTestId('rail-draft-fleet')).toHaveTextContent('New fleet');
    expect(screen.getByTestId('rail-draft-fleet')).toHaveAttribute('aria-current', 'true');
    expect(screen.queryByTestId('rail-fleet-none')).toBeNull();
  });

  it('in a fleet: the fleet repository is the fleet row with its role, not a repository', () => {
    const onPickFleet = vi.fn();
    renderRail({ repos: [...repos, { name: 'fleet', uid: 'u-fleet' }], fleet: inFleet, onPickFleet, pinned: { kind: 'fleet' } });
    expect(repoNames()).not.toContain('fleet');
    const row = screen.getByTestId('rail-fleet-row');
    expect(row).toHaveTextContent('fleet');
    expect(screen.getByTestId('rail-fleet-role')).toHaveTextContent('master');
    expect(row).toHaveAttribute('aria-current', 'true');
    expect(screen.queryByTestId('rail-new-fleet')).toBeNull();
    fireEvent.click(row);
    expect(onPickFleet).toHaveBeenCalled();
  });

  it('a member whose sync failed carries the red dot', () => {
    renderRail({ fleet: { ...inFleet, master: false, last_error: 'fetch: connection refused' } });
    expect(screen.getByTestId('rail-fleet-role')).toHaveTextContent('member');
    expect(screen.getByTestId('rail-fleet-health')).toHaveAttribute('title', 'fetch: connection refused');
  });
});
