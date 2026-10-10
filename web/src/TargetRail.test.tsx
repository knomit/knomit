import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, within, cleanup } from '@testing-library/react';
import { TargetRail } from './TargetRail';
import type { RepoInfo, Lens } from './api';
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

  it('an unavailable repo row is disabled, explained, and selects nothing', () => {
    const { onSelect } = renderRail();
    const ghost = screen.getByTestId('rail-repo-ghost') as HTMLButtonElement;
    // Positive control on a sibling: an available row is not disabled.
    expect((screen.getByTestId('rail-repo-b') as HTMLButtonElement).disabled).toBe(false);
    expect(ghost.disabled).toBe(true);
    expect(ghost).toHaveAttribute('title', 'database file not found');
    expect(within(ghost).getByTestId('repo-state-missing')).toHaveTextContent('missing');
    fireEvent.click(ghost);
    expect(onSelect).not.toHaveBeenCalled();
  });

  it('a lens with a mount that has no store is disabled with the reason', () => {
    const broken: Lens = { name: 'haunted', write: { uid: 'u-a', name: 'a' }, reads: [{ uid: 'u-ghost', name: 'ghost' }] };
    const { onSelect } = renderRail({ lenses: [...lenses, broken] });
    const row = screen.getByTestId('rail-lens-haunted') as HTMLButtonElement;
    expect((screen.getByTestId('rail-lens-dev') as HTMLButtonElement).disabled).toBe(false);
    expect(row.disabled).toBe(true);
    expect(row).toHaveAttribute('title', 'This lens cannot be read: its mount "ghost" has no store.');
    fireEvent.click(row);
    expect(onSelect).not.toHaveBeenCalled();
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
