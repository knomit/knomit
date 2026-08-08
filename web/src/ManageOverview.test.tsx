import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent, within, act } from '@testing-library/react';
import { RepoManager } from './RepoManager';
import { api } from './api';

// Overview is Manage's landing page and the only screen in the app that reads
// EVERY repository's configuration rather than the active one's. These pin the
// two things that makes possible — the attention list and the wiring table —
// and, just as importantly, the things it must NOT show.

const ORIGIN_OK = {
  name: 'origin', url: 'https://github.com/knomit/kb.git', branch: 'main', auth_method: 'token',
  last_sync_at: '2026-06-11T10:00:00Z', last_status: 'ok', last_error: null,
  push_interval: 300, last_push_at: '2026-06-11T10:00:00Z', last_push_status: 'ok', last_push_error: null,
  interval: 300,
};
const ORIGIN_PUSH_REJECTED = {
  ...ORIGIN_OK,
  last_push_status: 'error', last_push_error: 'non-fast-forward: the remote has commits you do not',
};

vi.mock('./api', async importOriginal => ({
  ...(await importOriginal<typeof import('./api')>()),
  api: {
    listArchived: vi.fn().mockResolvedValue([]),
    listLenses: vi.fn().mockResolvedValue([]),
    getLens: vi.fn().mockResolvedValue({ name: 'all', write: 'core', reads: [] }),
    listBranchNames: vi.fn().mockResolvedValue([]),
    getAgentBranch: vi.fn().mockResolvedValue('agent/test'),
    getRepo: vi.fn().mockResolvedValue({ name: 'core' }),
    updateRepo: vi.fn(),
    getOrigin: vi.fn().mockResolvedValue(null),
    deleteOrigin: vi.fn(),
    rebuild: vi.fn(),
    updateLens: vi.fn(),
    deleteLens: vi.fn(),
    createLens: vi.fn(),
  },
}));

const baseProps = {
  open: true as const,
  repos: [{ name: 'core' }, { name: 'work' }],
  currentRepo: 'core',
  readOnly: false,
  hideRemoteConfig: false,
  onChanged: () => {},
  onBrowse: () => {},
};

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(api.listArchived).mockResolvedValue([]);
  vi.mocked(api.listLenses).mockResolvedValue([]);
  vi.mocked(api.getRepo).mockResolvedValue({ name: 'core' });
  vi.mocked(api.getOrigin).mockResolvedValue(null);
});

describe('Manage Overview', () => {
  it('is where Manage lands, with the browsed repo one click away', async () => {
    render(<RepoManager {...baseProps} />);
    // Not the repo you were browsing: Overview is the only screen that answers
    // "which of my repositories needs something".
    expect(await screen.findByTestId('manage-overview')).toBeInTheDocument();
    expect(screen.queryByTestId('repo-detail-branch')).not.toBeInTheDocument();
    // …and the rail still marks it, so getting there is one click.
    expect(screen.getByTestId('repomgr-item-core').textContent).toContain('viewing');
  });

  it('carries no statistics — those belong to the browse summary', async () => {
    render(<RepoManager {...baseProps} />);
    const page = await screen.findByTestId('manage-overview');
    // The browse summary already owns every one of these; a second home for
    // them would be two places to disagree about one number.
    for (const word of [/facts/i, /domains/i, /entities/i, /commits/i, /confidence/i]) {
      expect(page.textContent).not.toMatch(word);
    }
    expect(api.stats).toBeUndefined();
  });

  it('names the repo whose push was rejected, and offers its remote', async () => {
    vi.mocked(api.getOrigin).mockImplementation((repo: string) =>
      Promise.resolve(repo === 'work' ? ORIGIN_PUSH_REJECTED : ORIGIN_OK));
    render(<RepoManager {...baseProps} />);

    const row = await screen.findByTestId('attention-work');
    expect(row.textContent).toContain('push rejected');
    expect(row.textContent).toContain('non-fast-forward');
    // The healthy one is not listed: this is a to-do, not an inventory.
    expect(screen.queryByTestId('attention-core')).not.toBeInTheDocument();
  });

  it('treats an unconfigured remote as attention, below the failures', async () => {
    vi.mocked(api.getOrigin).mockImplementation((repo: string) =>
      Promise.resolve(repo === 'core' ? ORIGIN_PUSH_REJECTED : null));
    render(<RepoManager {...baseProps} />);
    await screen.findByTestId('attention-core');

    // A broken remote is losing writes now; an unconnected one never had any.
    const failing = screen.getByTestId('attention-core');
    const absent = screen.getByTestId('attention-work');
    expect(failing.compareDocumentPosition(absent) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(absent.textContent).toContain('no remote configured');
  });

  it('says nothing at all when nothing needs attention', async () => {
    vi.mocked(api.getOrigin).mockResolvedValue(ORIGIN_OK);
    render(<RepoManager {...baseProps} />);
    await screen.findByTestId('fleet-row-core');
    // No empty "Needs attention · 0" heading: a screen element whose only job
    // would be to announce it has no job.
    await waitFor(() => expect(screen.queryByText(/needs attention/i)).not.toBeInTheDocument());
  });

  it('does NOT flag a missing licence, because nothing here can set one', async () => {
    // Healthy remotes throughout, so a missing licence is the ONLY thing that
    // could possibly raise a row — otherwise this would pass for the wrong
    // reason on the back of "no remote configured".
    vi.mocked(api.getOrigin).mockResolvedValue(ORIGIN_OK);
    vi.mocked(api.getRepo).mockResolvedValue({ name: 'core' }); // no license field
    render(<RepoManager {...baseProps} />);
    await screen.findByTestId('fleet-row-core');
    await waitFor(() => expect(screen.getByTestId('fleet-row-core').textContent).toContain('none'));
    // LICENSE is read-only server-side (manifest.go has no write path), so a
    // checklist row offering to fix it would name an action that does not
    // exist. It stays a column, which reports, not a row, which acts.
    expect(screen.queryByText(/licence/i)).toBeInTheDocument();   // the column header
    expect(screen.queryByTestId('attention-core')).not.toBeInTheDocument();
  });

  it('keeps "unreadable" distinct from "not connected"', async () => {
    vi.mocked(api.getOrigin).mockRejectedValue(new Error('boom'));
    render(<RepoManager {...baseProps} />);
    const row = await screen.findByTestId('fleet-row-core');

    // We do not know what is there. Reporting it as unconnected would invite
    // connecting over a live remote, and it is not actionable either.
    await waitFor(() => expect(row.textContent).toContain('unreadable'));
    expect(row.textContent).not.toContain('local only');
    expect(screen.queryByTestId('attention-core')).not.toBeInTheDocument();
  });

  it('lands a cell on the block it names, not the top of the page', async () => {
    vi.mocked(api.getOrigin).mockImplementation((repo: string) =>
      Promise.resolve(repo === 'work' ? ORIGIN_PUSH_REJECTED : ORIGIN_OK));
    render(<RepoManager {...baseProps} />);

    fireEvent.click(await screen.findByTestId('attention-fix-work'));
    // The repo's page, opened on Remote — the failing thing you clicked.
    await waitFor(() => expect(screen.getByTestId('block-remote')).toBeInTheDocument());
    await waitFor(() => expect(screen.getByTestId('toc-remote')).toHaveAttribute('aria-current', 'true'));
  });

  it('shows lens membership, write target first, overflowing past two', async () => {
    vi.mocked(api.listLenses).mockResolvedValue([
      { name: 'zeta', write: 'other', reads: [{ repo: 'core' }] },
      { name: 'alpha', write: 'other', reads: [{ repo: 'core' }] },
      { name: 'writes-here', write: 'core', reads: [{ repo: 'core' }] },
    ]);
    render(<RepoManager {...baseProps} />);
    const row = await screen.findByTestId('fleet-row-core');

    // The write target leads: that is where an agent using the lens writes.
    expect(within(row).getByTestId('fleet-lens-core-writes-here').textContent).toContain('write');
    // Two shown, the rest counted — never silently truncated.
    expect(within(row).getByTestId('fleet-more-core').textContent).toContain('+1');
    expect(within(row).queryByTestId('fleet-lens-core-zeta')).not.toBeInTheDocument();
  });

  it('drops the remote column entirely on a read-only instance', async () => {
    render(<RepoManager {...baseProps} hideRemoteConfig />);
    await screen.findByTestId('fleet-row-core');
    expect(screen.queryByText('Remote')).not.toBeInTheDocument();
    expect(screen.queryByTestId('attention-core')).not.toBeInTheDocument();
  });

  it('has no Overview row at all with zero repositories', async () => {
    render(<RepoManager {...baseProps} repos={[]} currentRepo="" />);
    // Nothing to summarise, and the create form owns that screen.
    await waitFor(() => expect(screen.getByTestId('create-name')).toBeInTheDocument());
    expect(screen.queryByTestId('repomgr-overview')).not.toBeInTheDocument();
  });

  // The rail's `+` buttons have always been gated on read-only. These were not,
  // which made Overview the one route to a create form whose submit is going to
  // be refused — and read-only is not just the read-only INSTANCE, it is every
  // history excursion, so an ordinary user could reach it by time-travelling.
  it('gates its create buttons on read-only, exactly as the rail does', async () => {
    render(<RepoManager {...baseProps} readOnly />);
    await screen.findByTestId('fleet-row-core');

    expect(screen.getByTestId('overview-new-repo')).toBeDisabled();
    expect(screen.getByTestId('overview-new-lens')).toBeDisabled();

    fireEvent.click(screen.getByTestId('overview-new-repo'));
    expect(screen.queryByTestId('create-name')).not.toBeInTheDocument();
  });
});

// Landing on the block a cell names is the headline behaviour of Overview, and
// two separate things were quietly cancelling it.
describe('arriving from an Overview cell', () => {
  it('resets the detail column BEFORE the focused block scrolls itself in', async () => {
    render(<RepoManager {...baseProps} />);
    const column = await screen.findByTestId('manage-detail');

    // Both writers, in the order they actually ran. The reset lived in a
    // passive effect in the PARENT and the focus scroll in a passive effect in
    // a CHILD; React flushes those child-first, so the reset ran second and
    // undid it — every cell landed you at the top of the page with the rail
    // highlighting a block off screen. Making the reset a layout effect fixes
    // the order, because the whole layout pass precedes the whole passive one.
    const order: string[] = [];
    let scrollTop = 400;
    Object.defineProperty(column, 'scrollTop', {
      configurable: true,
      get: () => scrollTop,
      set: (v: number) => { scrollTop = v; order.push('reset'); },
    });
    const scrollIntoView = vi.spyOn(Element.prototype, 'scrollIntoView')
      .mockImplementation(function () { order.push('focus'); });

    fireEvent.click(screen.getByTestId('fleet-branch-core'));
    await waitFor(() => expect(order).toContain('focus'));
    expect(order).toEqual(['reset', 'focus']);

    scrollIntoView.mockRestore();
  });

  // The Licence cell could never work: RepoDetail only pushes the `license`
  // section once its GET resolves, which is strictly after mount, and the focus
  // effect bailed on the missing block and never looked again.
  it('still finds a block that only arrives after the page has mounted', async () => {
    vi.mocked(api.getRepo).mockResolvedValue({ name: 'core', license: 'MIT License\n\nCopyright…' });
    render(<RepoManager {...baseProps} />);

    fireEvent.click(await screen.findByTestId('fleet-license-core'));

    // The block itself is late, and the rail entry that marks where you landed
    // arrives with it.
    await waitFor(() => expect(screen.getByTestId('block-license')).toBeInTheDocument());
    await waitFor(() => expect(screen.getByTestId('toc-license')).toHaveAttribute('aria-current', 'true'));
  });

  // …but only once. A plain re-run on section identity would drag the page back
  // to the requested block every time a later section landed, or every time the
  // user opened an editor that adds one.
  it('does not re-scroll to the block once the reader has moved on', async () => {
    // One gate for every getRepo, so the late `license` section lands exactly
    // when this test says it does rather than a microtask after mount.
    let release: () => void = () => {};
    const gate = new Promise<void>(res => { release = res; });
    vi.mocked(api.getRepo).mockImplementation(async () => {
      await gate;
      return { name: 'core', license: 'MIT License' };
    });

    render(<RepoManager {...baseProps} />);
    fireEvent.click(await screen.findByTestId('fleet-branch-core'));
    await waitFor(() => expect(screen.getByTestId('toc-agent-branch')).toHaveAttribute('aria-current', 'true'));

    // The reader takes over — a wheel releases the pin.
    fireEvent.wheel(window);
    await waitFor(() => expect(screen.getByTestId('toc-agent-branch')).not.toHaveAttribute('aria-current'));

    // NOW the late section arrives, changing section identity and re-running
    // the focus effect. Depending on `ids` is what makes the Licence cell work
    // at all; without the once-per-request guard the same dependency would drag
    // the page back to a block the reader had deliberately scrolled away from.
    await act(async () => { release(); await gate; });
    await screen.findByTestId('block-license');
    expect(screen.getByTestId('toc-agent-branch')).not.toHaveAttribute('aria-current');
  });
});

describe('Mounted in', () => {
  it('is the reverse of a lens\'s read mounts, and opens the lens', async () => {
    vi.mocked(api.listLenses).mockResolvedValue([
      { name: 'reads-core', write: 'work', reads: [{ repo: 'core', branch: 'main' }] },
      { name: 'writes-core', write: 'core', reads: [{ repo: 'core' }] },
      { name: 'unrelated', write: 'work', reads: [{ repo: 'work' }] },
    ]);
    render(<RepoManager {...baseProps} />);
    fireEvent.click(await screen.findByTestId('repomgr-item-core'));

    const block = await screen.findByTestId('block-mounted-in');
    expect(within(block).getByTestId('repo-mounted-writes-core').textContent).toContain('write target');
    expect(within(block).getByTestId('repo-mounted-reads-core').textContent).toContain('pinned');
    // A lens that does not touch this repo is not listed.
    expect(within(block).queryByTestId('repo-mounted-unrelated')).not.toBeInTheDocument();

    // Clicking a lens opens the LENS, not the repo — you clicked the lens.
    fireEvent.click(within(block).getByTestId('repo-mounted-open-writes-core'));
    await waitFor(() => expect(screen.getByTestId('lens-browse')).toBeInTheDocument());
  });

  it('has no block when no lens references the repo', async () => {
    vi.mocked(api.listLenses).mockResolvedValue([]);
    render(<RepoManager {...baseProps} />);
    fireEvent.click(await screen.findByTestId('repomgr-item-core'));
    await screen.findByTestId('block-agent-branch');
    // "Mounted in nothing" is the default state of an install with no lenses;
    // heading it would be noise on every page.
    expect(screen.queryByTestId('block-mounted-in')).not.toBeInTheDocument();
  });
});
