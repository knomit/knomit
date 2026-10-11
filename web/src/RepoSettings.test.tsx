import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent, within, act } from '@testing-library/react';
import { useState } from 'react';
import { LensSettings } from './LensSettings';
import { RepoSettings } from './RepoSettings';
import { api, createSession, streamTest, streamPreview, streamApply, streamCommit, RepoIndexingError, MAX_LENS_DESCRIPTION_BYTES, MAX_REPO_DESCRIPTION_BYTES } from './api';
import type { Lens, RepoCreateStatus } from './api';

// `api` and the origin-session streams are stubbed; the module's other exports
// — the description byte caps — pass through from the real module, so a test
// asserting on a cap is asserting on the value the app actually ships.
vi.mock('./api', async importOriginal => ({
  ...(await importOriginal<typeof import('./api')>()),
  // The connect sub-page's backend. Only the commit-lock test drives these;
  // everywhere else they exist so the wizard cannot reach the network.
  createSession: vi.fn(),
  streamTest: vi.fn(() => () => {}),
  streamPreview: vi.fn(() => () => {}),
  streamApply: vi.fn(),
  streamCommit: vi.fn(),
  // Resolved, not bare: every caller does deleteSession(...).catch(...), so a
  // bare vi.fn() fails inside a click handler rather than at an assertion.
  deleteSession: vi.fn().mockResolvedValue(undefined),
  api: {
    listClientSessions: vi.fn().mockResolvedValue({ truncated: false, sessions: [], policy: { dead_after_s: 3600, hidden_after_s: 10800, retention_s: 604800, live_window_s: 360, limit: 500, max_limit: 2000 } }),
    // null = this server has no OAuth issuer (404), so no Authorizations tab.
    listOAuthPending: vi.fn().mockResolvedValue(null),
    listArchived: vi.fn().mockResolvedValue([
      { id: 'old.1', name: 'old', origin: '', archivedAt: '2026-06-01T00:00:00Z' },
    ]),
    listLenses: vi.fn().mockResolvedValue([
      { name: 'dev', write: { uid: 'uid-work', name: 'work' }, reads: [{ uid: 'uid-core', name: 'core', branch: 'main' }, { uid: 'uid-work', name: 'work' }] },
    ]),
    getLens: vi.fn().mockResolvedValue({ name: 'dev', write: { uid: 'uid-work', name: 'work' }, reads: [{ uid: 'uid-core', name: 'core', branch: 'main' }, { uid: 'uid-work', name: 'work' }] }),
    createLens: vi.fn().mockResolvedValue({ name: 'newlens', write: { uid: 'uid-core', name: 'core' }, reads: [] }),
    updateLens: vi.fn().mockResolvedValue({ name: 'dev', write: { uid: 'uid-work', name: 'work' }, reads: [{ uid: 'uid-core', name: 'core', branch: 'main' }, { uid: 'uid-work', name: 'work' }] }),
    deleteLens: vi.fn().mockResolvedValue(undefined),
    renameLens: vi.fn().mockResolvedValue({ name: 'dev' }),
    listBranchNames: vi.fn().mockResolvedValue([]),
    getAgentBranch: vi.fn().mockResolvedValue('agent/test'),
    getRepo: vi.fn().mockResolvedValue({ name: 'core' }),
    updateRepo: vi.fn().mockResolvedValue({ name: 'core' }),
    renameRepo: vi.fn().mockResolvedValue({ name: 'core' }),
    getOrigin: vi.fn().mockResolvedValue(null),
    deleteOrigin: vi.fn(),
    cancelIndex: vi.fn().mockResolvedValue({ name: 'core', uid: 'uid-core', index_state: 'error', index_reason: 'indexing cancelled' }),
    rebuild: vi.fn().mockResolvedValue({ id: 'job1', state: 'running' }),
    restoreRepo: vi.fn().mockResolvedValue({ name: 'old' }),
    purgeRepo: vi.fn().mockResolvedValue(undefined),
    archiveRepo: vi.fn().mockResolvedValue(undefined),
  },
}));

const ARCHIVED_FIXTURE = [
  { id: 'old.1', name: 'old', origin: '', archivedAt: '2026-06-01T00:00:00Z' },
];

const LENSES = [
  { name: 'dev', write: { uid: 'uid-work', name: 'work' }, reads: [{ uid: 'uid-core', name: 'core', branch: 'main' }, { uid: 'uid-work', name: 'work' }] },
];

// Page mounts RepoSettings DIRECTLY, the way the Settings facet does, with the
// little wiring every owner gives it: an error line for onError, the pane
// following a rename to the new name, and archiving gated on the instance.
// onChanged receives {from, to} on a rename, as the app's refresh expects.
function Page({ name: initial = 'core', serverReadOnly = false, hideRemoteConfig = false,
  onChanged, onRenamed, onBusyChange, onSelectLens, focus }: {
  name?: string; serverReadOnly?: boolean; hideRemoteConfig?: boolean;
  onChanged?: (renamed?: { from: string; to: string }) => void;
  onRenamed?: (n: string) => void;
  onBusyChange?: (busy: boolean) => void;
  onSelectLens?: (n: string) => void;
  focus?: string;
}) {
  const [name, setName] = useState(initial);
  const [err, setErr] = useState('');
  return (
    <>
      {err && <div data-testid="page-error">{err}</div>}
      <RepoSettings
        key={name}
        name={name}
        lenses={LENSES}
        focus={focus}
        canArchive={!serverReadOnly}
        serverReadOnly={serverReadOnly}
        hideRemoteConfig={hideRemoteConfig}
        onArchived={() => onChanged?.()}
        onChanged={() => onChanged?.()}
        onRenamed={n => { onChanged?.({ from: name, to: n }); onRenamed?.(n); setName(n); }}
        onSelectLens={onSelectLens ?? (() => {})}
        onBusyChange={onBusyChange}
        onError={setErr}
      />
    </>
  );
}

// renderLens mounts LensSettings DIRECTLY, the way the Settings facet does,
// for the editor tests that a lens shares with a repo.
function renderLens() {
  return render(
    <LensSettings lens={LENSES[0]} name="dev" repos={[{ name: 'core', uid: 'uid-core' }, { name: 'work', uid: 'uid-work' }]}
      readOnly={false} onDeleted={() => {}} onSaved={() => {}} onRenamed={() => {}} onError={() => {}} />,
  );
}

describe('RepoSettings', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    // clearAllMocks clears CALLS, not implementations, so a test that narrows
    // listArchived would otherwise leak its fixture into every test after it.
    // Re-assert the default so order cannot matter.
    vi.mocked(api.listArchived).mockResolvedValue(ARCHIVED_FIXTURE);
  });

  it('shows the repo settings page with its agent branch and remote', async () => {
    render(<Page />);
    // RepoDetail for core loads its agent branch and its origin.
    await waitFor(() => expect(api.getAgentBranch).toHaveBeenCalledWith('core'));
    await waitFor(() => expect(api.getOrigin).toHaveBeenCalledWith('core'));
    await waitFor(() => expect(screen.getByTestId('repo-detail-branch')).toBeInTheDocument());
    // The ⋯ overflow is gone: each of its items now lives in the block that
    // owns it, visible without opening anything.
    expect(screen.queryByTestId('repo-menu')).not.toBeInTheDocument();
    expect(screen.getByTestId('repo-rebuild')).toBeInTheDocument();
    expect(screen.getByTestId('repo-archive')).toBeInTheDocument();
  });

  // A repo with no usable ontology (missing, empty, does not parse) is OPEN and
  // readable, but refuses every write. Its page says so, quoting the server; a
  // repo with an ontology says nothing. (The rail row's chip is TargetRail's.)
  describe('a repo with no usable ontology', () => {
    const msg = 'ontology at .knomit/ontology.yaml does not parse: ontology id is required';

    it('states it on the repo page from the single GET', async () => {
      vi.mocked(api.getRepo).mockImplementation(async (name: string) =>
        (name === 'garbled' ? { name, ontology_error: msg } : { name }));
      try {
        render(<Page name="garbled" />);
        const banner = await screen.findByTestId('repo-ontology-error');
        expect(banner).toHaveTextContent('every write to it is refused');
        expect(banner).toHaveTextContent(msg);
      } finally {
        vi.mocked(api.getRepo).mockResolvedValue({ name: 'core' });
      }
    });

    it('says nothing on the page of a repo that has an ontology', async () => {
      render(<Page />);
      await waitFor(() => expect(api.getRepo).toHaveBeenCalledWith('core'));
      await screen.findByTestId('repo-detail-branch');
      expect(screen.queryByTestId('repo-ontology-error')).toBeNull();
    });
  });

  // "Not connected" is STATE, so the Remote block renders it and carries the
  // action that changes it — rather than the block vanishing and the offer
  // hiding in an overflow menu, which is what a boxed pane forced.
  it('says so in the Remote block when unconnected, with Connect on it', async () => {
    render(<Page />);
    await waitFor(() => expect(api.getOrigin).toHaveBeenCalledWith('core'));
    const block = await screen.findByTestId('block-remote');
    expect(block.textContent).toContain('Not connected');
    expect(within(block).getByTestId('remote-connect')).toBeInTheDocument();
  });

  // The connect flow is a SUB-FLOW of the repo's page: while it runs it
  // replaces the whole body, and it hands back to the Remote block. It lives
  // inside RepoSettings, so wherever the page is mounted it gets the same flow.
  describe('the connect sub-flow', () => {
    async function openConnect(props: Parameters<typeof Page>[0] = {}) {
      render(<Page {...props} />);
      fireEvent.click(await within(await screen.findByTestId('block-remote')).findByTestId('remote-connect'));
      return screen.findByTestId('remote-connect-wizard');
    }

    it('names the repository it belongs to in its crumb', async () => {
      await openConnect();
      expect(screen.getByTestId('wizard-crumb-back').textContent).toBe('core');
    });

    it('replaces the settings page rather than stacking under it', async () => {
      await openConnect();
      expect(screen.queryByTestId('repo-settings')).not.toBeInTheDocument();
    });

    it('returns to the Remote block via the crumb', async () => {
      await openConnect();
      fireEvent.click(screen.getByTestId('wizard-crumb-back'));
      await screen.findByTestId('repo-settings');
      expect(screen.queryByTestId('remote-connect-wizard')).not.toBeInTheDocument();
      // focus='remote' is what lands the reader back on the block they left
      // from; SettingsPage marks the focused block current in its index.
      await waitFor(() => expect(screen.getByTestId('toc-remote')).toHaveAttribute('aria-current', 'true'));
    });

    // The commit stream has no abort and no undo: unmounting the wizard mid-
    // commit leaves the swap and rebuild running with nothing listening. The
    // wizard withholds its own crumb; every OTHER exit belongs to whoever
    // mounts this page, so the page raises onBusyChange for exactly the span
    // of the commit — and only on a change, so the owner sees [true, false].
    it('raises onBusyChange(true) while the commit is in flight and false after', async () => {
      type Fn = ReturnType<typeof vi.fn>;
      (createSession as unknown as Fn).mockResolvedValueOnce({ session_id: 'sess-lock' });
      (streamTest as unknown as Fn).mockImplementation((_r: string, _s: string, onEvent: (e: unknown) => void) => {
        queueMicrotask(() => onEvent({ phase: 'done', result: { branches: ['main'], agent_branches: [], default_branch: 'main', matched_agent: '', history: 'shared', remote_fact_count: 1, local_fact_count: 1 } }));
        return () => {};
      });
      (streamPreview as unknown as Fn).mockImplementation((_r: string, _s: string, onEvent: (e: unknown) => void) => {
        queueMicrotask(() => onEvent({ phase: 'done', result: { local_only: 1, remote_only: 0, shared_path: 0, dead_refs_found: 0 } }));
        return () => {};
      });
      (streamApply as unknown as Fn).mockImplementation(async (_r: string, _s: string, _st: string, _b: string | undefined, onEvent: (e: unknown) => void) => {
        onEvent({ phase: 'done', result: { total_facts: 0, from_local: 0, from_remote: 0, overwrites: 0 } });
      });

      let finish!: () => void;
      (streamCommit as unknown as Fn).mockImplementation(() => new Promise<void>((_res, rej) => { finish = () => rej(new Error('commit failed')); }));
      const calls: boolean[] = [];
      await openConnect({ onBusyChange: b => calls.push(b) });
      expect(calls).toEqual([]);
      fireEvent.change(screen.getByTestId('wizard-url'), { target: { value: 'https://example.com/repo.git' } });
      fireEvent.click(screen.getByTestId('wizard-test'));
      fireEvent.click(await screen.findByTestId('wizard-connect'));

      await waitFor(() => expect(calls).toEqual([true]));
      // The commit ends (here by failing, the outcome that keeps the wizard
      // mounted), and the lock drops exactly once.
      await act(async () => { finish(); });
      await waitFor(() => expect(calls).toEqual([true, false]));
      expect(screen.getByTestId('remote-connect-wizard')).toBeInTheDocument();
    });

    // Every step is either ahead of you or behind you, and none of them is a
    // place you can click to — so the tracker is inert by construction.
    it('shows a three-step tracker with Connect current', async () => {
      const page = await openConnect();
      expect(within(page).getByTestId('wizard-step-1')).toHaveAttribute('aria-current', 'step');
      expect(within(page).getByTestId('wizard-step-2')).not.toHaveAttribute('aria-current');
      expect(within(page).getByTestId('wizard-step-3')).not.toHaveAttribute('aria-current');
      expect(within(page).queryByRole('button', { name: 'Review' })).toBeNull();
    });
  });

  it('shows the remote state when connected, and drops the Connect offer', async () => {
    (api.getOrigin as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'origin', url: 'https://github.com/knomit/kb.git', branch: 'main', auth_method: 'token',
      last_sync_at: '2026-06-11T10:00:00Z', last_status: 'ok', last_error: null,
    });
    render(<Page />);
    await waitFor(() => expect(screen.getByTestId('sync-line')).toBeInTheDocument());
    expect(screen.queryByTestId('remote-connect')).not.toBeInTheDocument();
  });

  // Nothing folds any more. In a boxed dialog the connect snippets had to hide
  // behind a disclosure to leave room for state; the mode has the room, and a
  // fold is a click on the way to the thing you came for.
  it('renders the agent-access snippets without anything to expand', async () => {
    render(<Page />);
    expect(await screen.findByTestId('repo-copy')).toBeInTheDocument();
    expect(screen.getByTestId('repo-copy-mcp')).toBeInTheDocument();
    expect(screen.queryByTestId('repo-connect-toggle')).not.toBeInTheDocument();
  });

  // The order is the spec's: Description, Remote, Licence, Agent branch,
  // Experiments, Pushed branches, Index, Mounted in, Agent access (kept, plan
  // assumption 6), Danger zone. Asserted as the FULL id list, so a block that
  // moves or appears anywhere fails it.
  it('orders the repo page as the spec lists it', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core', description: 'Root manifest.', license: 'MIT License' });
    (api.getOrigin as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'origin', url: 'https://github.com/knomit/kb.git', branch: 'main', auth_method: 'token',
      last_sync_at: '2026-06-11T10:00:00Z', last_status: 'ok', last_error: null,
    });
    render(<Page />);
    await screen.findByTestId('block-license');
    await screen.findByTestId('block-pushed-branches');
    await screen.findByTestId('block-mounted-in');

    const ids = Array.from(document.querySelectorAll('[data-testid^="block-"]')).map(n => n.id);
    expect(ids).toEqual(['description', 'remote', 'license', 'agent-branch', 'experiments', 'pushed-branches', 'index', 'mounted-in', 'agent-access', 'danger']);
    // The index lists them in the same order.
    const toc = Array.from(document.querySelectorAll('[data-testid^="toc-"]'))
      .map(n => n.getAttribute('data-testid')!).filter(t => !t.endsWith('-problem')).map(t => t.slice(4));
    expect(toc).toEqual(ids);
  });

  it('a failing push marks the Remote index item', async () => {
    (api.getOrigin as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'origin', url: 'https://github.com/knomit/kb.git', branch: 'main', auth_method: 'token',
      last_sync_at: '2026-06-11T10:00:00Z', last_status: 'ok', last_error: null,
      last_push_status: 'error', last_push_error: 'rejected',
    });
    render(<Page />);
    expect(await screen.findByTestId('toc-remote-problem')).toBeInTheDocument();
  });

  it('a healthy origin leaves the Remote index item unmarked', async () => {
    (api.getOrigin as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'origin', url: 'https://github.com/knomit/kb.git', branch: 'main', auth_method: 'token',
      last_sync_at: '2026-06-11T10:00:00Z', last_status: 'ok', last_error: null,
      last_push_status: 'ok', last_push_error: null,
    });
    render(<Page />);
    // The card has loaded (positive control), so the absence is the rule.
    await screen.findByTestId('sync-line');
    expect(screen.getByTestId('toc-remote')).toBeInTheDocument();
    expect(screen.queryByTestId('toc-remote-problem')).toBeNull();
  });

  it('opens a lens from Mounted in through onSelectLens', async () => {
    const onSelectLens = vi.fn();
    render(<Page onSelectLens={onSelectLens} />);
    fireEvent.click(await screen.findByTestId('repo-mounted-open-dev'));
    expect(onSelectLens).toHaveBeenCalledWith('dev');
  });

  it('has no Browse button: the Browse tab replaced it', async () => {
    render(<Page />);
    await screen.findByTestId('repo-detail-branch');
    expect(screen.queryByTestId('repo-browse')).toBeNull();
  });

  // Pushed branches (F11 UI merge) sit directly under Experiments on an
  // ordinary repo: both are other branches whose facts can be brought in.
  it('shows the Pushed branches block right after Experiments', async () => {
    render(<Page />);
    const pushed = await screen.findByTestId('block-pushed-branches');
    const exps = screen.getByTestId('block-experiments');
    expect(exps.compareDocumentPosition(pushed) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  // The contents rail is an index, not a nav: every block is already on the
  // page, so it lists them all rather than gating any behind a selection.
  it('lists every block in the contents rail', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core', description: 'Root manifest.' });
    render(<Page />);
    await screen.findByTestId('toc-description');
    for (const id of ['description', 'agent-branch', 'remote', 'agent-access', 'index', 'danger']) {
      expect(screen.getByTestId(`toc-${id}`)).toBeInTheDocument();
    }
  });

  // Disconnect is a card-local action: it edits the connection the Remote card
  // describes, so it is an icon button ON that card and NOT a ⋯ menu item.
  // Asserted by scoping the query to the card — a document-wide getByTestId
  // would pass against either placement and so could not tell them apart.
  it('disconnects a remote from the Remote card, behind a confirm', async () => {
    (api.getOrigin as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'origin', url: 'https://github.com/knomit/kb.git', branch: 'main', auth_method: 'token',
      last_sync_at: '2026-06-11T10:00:00Z', last_status: 'ok', last_error: null,
    });
    (api.deleteOrigin as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    const onChanged = vi.fn();
    render(<Page onChanged={onChanged} />);

    const remoteCard = await screen.findByTestId('remote-card');
    fireEvent.click(within(remoteCard).getByTestId('remote-disconnect'));
    // Disconnecting asks first — no request until the confirm is accepted.
    expect(api.deleteOrigin).not.toHaveBeenCalled();
    fireEvent.click(screen.getByTestId('disconnect-confirm'));
    await waitFor(() => expect(api.deleteOrigin).toHaveBeenCalledWith('core'));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
  });

  // A detach is refused (409) while the repo's index job runs. That refusal
  // has a way through — cancel the index — so it is offered in place, and the
  // detach is retried by pressing Disconnect again once the cancel lands.
  it('offers to cancel indexing when the detach is refused for it, then retries', async () => {
    (api.getOrigin as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'origin', url: 'https://github.com/knomit/kb.git', branch: 'main', auth_method: 'token',
      last_sync_at: '2026-06-11T10:00:00Z', last_status: 'ok', last_error: null,
    });
    (api.deleteOrigin as ReturnType<typeof vi.fn>)
      .mockRejectedValueOnce(new RepoIndexingError('wait for it to finish or cancel indexing', '/api/v1/repos/core/index:cancel'))
      .mockResolvedValueOnce(undefined);
    render(<Page />);

    fireEvent.click(within(await screen.findByTestId('remote-card')).getByTestId('remote-disconnect'));
    fireEvent.click(screen.getByTestId('disconnect-confirm'));
    expect(await screen.findByTestId('disconnect-indexing')).toHaveTextContent('wait for it to finish or cancel indexing');
    // Not reported as a generic failure: the box above IS the report.
    expect(screen.queryByText(/disconnect failed/)).not.toBeInTheDocument();

    fireEvent.click(screen.getByTestId('disconnect-cancel-indexing'));
    // It follows the refusal's link rather than building the URL.
    await waitFor(() => expect(api.cancelIndex).toHaveBeenCalledWith('/api/v1/repos/core/index:cancel'));
    await waitFor(() => expect(screen.queryByTestId('disconnect-indexing')).not.toBeInTheDocument());

    fireEvent.click(screen.getByTestId('disconnect-confirm'));
    await waitFor(() => expect(api.deleteOrigin).toHaveBeenCalledTimes(2));
  });

  // A failed origin load is a THIRD state, not "unconnected": the block carries
  // the error and withholds "Connect a remote…", so the user is not invited to
  // overwrite a remote that is merely unreadable.
  it('surfaces a failed remote load instead of rendering it as unconnected', async () => {
    (api.getOrigin as ReturnType<typeof vi.fn>).mockRejectedValue(new Error('boom'));
    render(<Page />);

    await waitFor(() => expect(screen.getByTestId('remote-error')).toHaveTextContent(/could not load remote status/i));
    const block = screen.getByTestId('block-remote');
    expect(block.textContent).not.toContain('Not connected');
    expect(within(block).queryByTestId('remote-connect')).not.toBeInTheDocument();
  });

  // Archive asks for an optional note before it archives, and sends it.
  // Sabotage: archive on the first click (no confirm) → no note input → red.
  it('archives a repo with the note from the confirm', async () => {
    vi.mocked(api.archiveRepo).mockResolvedValue({ id: 'uid-core', name: 'core', origin: '', archivedAt: '2026-10-09T00:00:00Z' });
    render(<Page />);
    fireEvent.click(await screen.findByTestId('repo-archive'));
    expect(api.archiveRepo).not.toHaveBeenCalled();
    fireEvent.change(screen.getByTestId('repo-archive-note'), { target: { value: 'superseded' } });
    fireEvent.click(screen.getByTestId('repo-archive-confirm'));
    await waitFor(() => expect(api.archiveRepo).toHaveBeenCalledWith('core', 'superseded'));
  });

  it('retries a failed remote load', async () => {
    // Driven by an explicit flag rather than mockRejectedValueOnce, so the
    // failure lasts exactly as long as the test says, however many times the
    // page asks.
    let failing = true;
    const getOrigin = api.getOrigin as ReturnType<typeof vi.fn>;
    getOrigin.mockImplementation((repo: string) => {
      if (repo !== 'core') return Promise.resolve(null);
      if (failing) return Promise.reject(new Error('boom'));
      return Promise.resolve({
        name: 'origin', url: 'https://github.com/knomit/kb.git', branch: 'main', auth_method: 'token',
        last_sync_at: '2026-06-11T10:00:00Z', last_status: 'ok', last_error: null,
      });
    });
    render(<Page />);

    const retry = await screen.findByTestId('remote-retry');
    failing = false;
    fireEvent.click(retry);
    await waitFor(() => expect(screen.getByText('https://github.com/knomit/kb.git')).toBeInTheDocument());
    expect(screen.queryByTestId('remote-error')).not.toBeInTheDocument();
  });

  // Archive was the ⋯ menu's last item. It now sits in the Danger zone block,
  // fenced off by its own tint rather than by an overflow that had to be opened.
  it('puts Archive in the Danger zone block, not behind an overflow', async () => {
    render(<Page />);
    const danger = await screen.findByTestId('block-danger');
    expect(within(danger).getByTestId('repo-archive')).toBeInTheDocument();
    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
  });

  // Rename shares the Danger zone with Archive: same tint, same fence, and the
  // same typed-confirmation friction — but it deletes nothing and is freely
  // reversible, which is why its warning names the one real consequence
  // (agent MCP URLs) instead of borrowing Archive's "this is dangerous" tone.
  describe('Rename', () => {
    it('puts Rename in the Danger zone block, beside Archive', async () => {
      render(<Page />);
      const danger = await screen.findByTestId('block-danger');
      expect(within(danger).getByTestId('repo-rename-submit')).toBeInTheDocument();
    });

    // The warning must name what ACTUALLY breaks post-rename (the MCP
    // endpoint URL) and must say in-flight agent queries survive it — MCP
    // tool-session cursors are pinned to the repo's stable uid, not its
    // display name, so a query outlives a rename that runs underneath it. It
    // must not read as "agent activity breaks" generally.
    it('warns that MCP endpoint URLs break but in-flight agent queries are unaffected', async () => {
      render(<Page />);
      const warning = await screen.findByTestId('repo-rename-warning');
      expect(warning).toHaveTextContent(/MCP/i);
      expect(warning.textContent).toMatch(/in-flight agent quer(y|ies) (is|are) unaffected/i);
    });

    it('keeps Rename disabled until the current name is typed exactly', async () => {
      render(<Page />);

      const submit = await screen.findByTestId('repo-rename-submit');
      expect(submit).toBeDisabled();

      fireEvent.change(screen.getByTestId('repo-rename-input'), { target: { value: 'beta' } });
      expect(submit).toBeDisabled();

      fireEvent.change(screen.getByTestId('repo-rename-confirm'), { target: { value: 'cor' } });
      expect(submit).toBeDisabled();

      fireEvent.change(screen.getByTestId('repo-rename-confirm'), { target: { value: 'core' } });
      expect(submit).toBeEnabled();
    });

    // A no-op rename (new name === current name) would report success and
    // change nothing — confusing rather than dangerous, so it is disabled
    // rather than sent.
    it('disables Rename when the new name equals the current name', async () => {
      render(<Page />);

      fireEvent.change(screen.getByTestId('repo-rename-input'), { target: { value: 'core' } });
      fireEvent.change(screen.getByTestId('repo-rename-confirm'), { target: { value: 'core' } });
      expect(screen.getByTestId('repo-rename-submit')).toBeDisabled();
    });

    it('calls renameRepo with the new name and follows the pane to it', async () => {
      (api.renameRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'beta' });
      render(<Page />);

      fireEvent.change(screen.getByTestId('repo-rename-input'), { target: { value: 'beta' } });
      fireEvent.change(screen.getByTestId('repo-rename-confirm'), { target: { value: 'core' } });
      fireEvent.click(screen.getByTestId('repo-rename-submit'));

      await waitFor(() => expect(api.renameRepo).toHaveBeenCalledWith('core', 'beta'));
      // The pane is addressed by name; it must follow the repo to its new one
      // rather than show a 404 on the next read under the old name. The page
      // has no heading of its own any more (the top bar's label names the
      // target), so "follows" is read off the page re-reading under the new
      // name.
      await waitFor(() => expect(api.getRepo).toHaveBeenCalledWith('beta'));
    });

    // The parent learns about the rename too — with BOTH names — so it can
    // re-point a stale "currently browsed repo" selection at the new one
    // instead of falling back to an arbitrary remaining repo.
    it('reports the rename to the parent as {from, to}, not a bare refresh', async () => {
      (api.renameRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'beta' });
      const onChanged = vi.fn();
      render(<Page onChanged={onChanged} />);

      fireEvent.change(screen.getByTestId('repo-rename-input'), { target: { value: 'beta' } });
      fireEvent.change(screen.getByTestId('repo-rename-confirm'), { target: { value: 'core' } });
      fireEvent.click(screen.getByTestId('repo-rename-submit'));

      await waitFor(() => expect(onChanged).toHaveBeenCalledWith({ from: 'core', to: 'beta' }));
    });

    // A concurrent rename (someone else won the race) is a 409 whose detail
    // already tells the user the right move — re-read and retry. The UI must
    // not swallow that into invented copy the way Rebuild's "already running"
    // 409 does: there is no "someone else broke this" framing to invent, and
    // the server's own sentence already says what to do.
    it('surfaces the server detail verbatim on a 409 conflict, without inventing new copy', async () => {
      (api.renameRepo as ReturnType<typeof vi.fn>).mockRejectedValue(
        new Error('/api/v1/repos/core/rename → 409 repo "core" was renamed or removed while this request was in flight; re-read it and retry'),
      );
      render(<Page />);

      fireEvent.change(screen.getByTestId('repo-rename-input'), { target: { value: 'beta' } });
      fireEvent.change(screen.getByTestId('repo-rename-confirm'), { target: { value: 'core' } });
      fireEvent.click(screen.getByTestId('repo-rename-submit'));

      expect(await screen.findByText(/re-read it and retry/i)).toBeInTheDocument();
    });

    it('is disabled entirely on a read-only instance', async () => {
      render(<Page serverReadOnly />);

      fireEvent.change(screen.getByTestId('repo-rename-input'), { target: { value: 'beta' } });
      fireEvent.change(screen.getByTestId('repo-rename-confirm'), { target: { value: 'core' } });
      expect(screen.getByTestId('repo-rename-input')).toBeDisabled();
      expect(screen.getByTestId('repo-rename-confirm')).toBeDisabled();
      expect(screen.getByTestId('repo-rename-submit')).toBeDisabled();
    });
  });

  it('renders the README.md description in the detail pane', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'core', description: '# Knowledge Base\n\nRoot manifest.',
    });
    render(<Page />);
    await waitFor(() => expect(api.getRepo).toHaveBeenCalledWith('core'));
    expect(await screen.findByTestId('repo-description')).toHaveTextContent('Root manifest.');
  });

  it('renders GFM in the README.md description — a table, not literal pipe text', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'core',
      description: '# KB\n\n| Topic | Meaning |\n|---|---|\n| invariants | violate this and it breaks |',
    });
    render(<Page />);
    await waitFor(() => expect(api.getRepo).toHaveBeenCalledWith('core'));

    const desc = await screen.findByTestId('repo-description');
    expect(desc.querySelector('table')).not.toBeNull();
    expect(desc.querySelectorAll('th')).toHaveLength(2);
    expect(desc.textContent).not.toContain('|---|');
    // The prose class sits on the inner scroll container that wraps the markdown.
    expect(desc.querySelector('.k-prose')).not.toBeNull();
  });

  // A licence rendered through the markdown pipeline loses every single
  // newline — markdown reflows them. This asserts the raw text survives
  // byte-for-byte, which is the one thing that distinguishes a correct
  // implementation from a plausible-looking one.
  it('renders LICENSE as preformatted text, preserving line breaks', async () => {
    const mit = 'MIT License\n\nPermission is hereby granted, free of charge,\nto any person obtaining a copy';
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'core', license: mit,
    });
    render(<Page />);
    await waitFor(() => expect(api.getRepo).toHaveBeenCalledWith('core'));

    expect((await screen.findByTestId('repo-license')).textContent).toBe(mit);
  });

  // No LICENSE and nothing writable ⇒ no block at all, mirroring the
  // description's rule: a read-only repo with neither a file to show nor a
  // way to create one gets no heading for it.
  it('omits the license block when read-only and the repo has no LICENSE', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core' });
    render(<Page serverReadOnly />);
    await waitFor(() => expect(api.getRepo).toHaveBeenCalledWith('core'));
    expect(screen.queryByTestId('block-license')).toBeNull();
    expect(screen.queryByTestId('toc-license')).toBeNull();
  });

  // Offers to add a LICENSE when there is none, mirroring the description's
  // empty-but-writable state.
  it('offers to add a LICENSE when there is none', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core', description: '' });
    render(<Page />);

    expect(await screen.findByText(/No LICENSE at the repo root/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /add license/i })).toBeEnabled();
  });

  // Saving a new LICENSE goes through the same PATCH the description uses,
  // just with `license` instead of `description` in the body — the two
  // fields are independent, so this must not touch the README.
  it('saves a new LICENSE through updateRepo', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core' });
    (api.updateRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core', license: 'MIT\n' });
    render(<Page />);

    fireEvent.click(await screen.findByRole('button', { name: /add license/i }));
    fireEvent.change(screen.getByTestId('license-textarea'), { target: { value: 'MIT' } });
    fireEvent.click(screen.getByTestId('repo-license-save'));

    await waitFor(() => expect(api.updateRepo).toHaveBeenCalledWith('core', { license: 'MIT' }));
  });

  // The trap this whole feature is built around: reusing DescriptionBody's
  // read view for the licence would run it through ReactMarkdown, which
  // reflows single newlines and turns "* not a bullet" into a real bullet.
  it('renders the licence preformatted, not as markdown', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'core', license: 'MIT License\n\n* not a bullet\nsecond line\n',
    });
    render(<Page />);

    const pre = await screen.findByTestId('repo-license');
    expect(pre.tagName).toBe('PRE');
    expect(pre).toHaveTextContent('* not a bullet');
    expect(screen.queryByRole('listitem')).toBeNull();
  });

  // The trap this state exists to close: a LICENSE too large for the server
  // to hand back must render as "present but unreadable", not as "absent" —
  // and crucially must offer neither Add nor Edit, since either control would
  // open a blank textarea over a file the server never actually read, and a
  // Save from there would look like an ordinary edit while destroying the
  // original (the server now refuses that write too, but the control must
  // not even be there to invite it).
  it('renders the oversize-license state with no Add/Edit control', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'core', license_oversize: true,
    });
    render(<Page />);

    expect(await screen.findByTestId('repo-license-oversize')).toHaveTextContent(/too large/i);
    expect(screen.queryByTestId('repo-license')).toBeNull();
    expect(screen.queryByRole('button', { name: /add license/i })).toBeNull();
    expect(screen.queryByRole('button', { name: /edit license/i })).toBeNull();
    expect(screen.queryByTestId('license-textarea')).toBeNull();
  });

  // A read-only repo with an oversize LICENSE has something to report (a file
  // exists) even though readOnly already forbids Add/Edit for other reasons —
  // the block must still appear and say so, mirroring how a readable LICENSE
  // renders under readOnly.
  it('shows the oversize-license state even when read-only', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'core', license_oversize: true,
    });
    render(<Page serverReadOnly />);

    expect(await screen.findByTestId('repo-license-oversize')).toBeInTheDocument();
  });

  // With no README.md the block is still offered so a description can be
  // written — but only when the user could actually write one.
  it('offers an empty description block when the repo has no README.md', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core' });
    render(<Page />);
    await waitFor(() => expect(api.getRepo).toHaveBeenCalledWith('core'));
    expect(await screen.findByTestId('repo-description')).toHaveTextContent(/No description yet/i);
  });

  it('hides the description block entirely when read-only and empty', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core' });
    render(<Page serverReadOnly />);
    await waitFor(() => expect(api.getRepo).toHaveBeenCalledWith('core'));
    expect(screen.queryByTestId('block-description')).not.toBeInTheDocument();
  });

  // The pencil belongs to the block heading, not to the body. In the body it
  // was a flex sibling of a fixed-height scroller, so a long README lost a
  // gutter's width down its whole length for a glyph only visible at the top.
  it('puts the description pencil in the block heading, not beside the prose', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core', description: '# Old' });
    render(<Page />);

    const pencil = await screen.findByTestId('repo-description-edit');
    expect(screen.getByTestId('block-description')).toContainElement(pencil);
    expect(screen.getByTestId('repo-description')).not.toContainElement(pencil);
  });

  // Save and Cancel take the pencil's place in the HEADING rather than stacking
  // under the textarea. Under it they pushed every block below the description
  // down the page the moment you clicked the pencil; in the heading's slot they
  // swap for a control of the same height and nothing moves.
  it('puts save and cancel in the block heading, where the pencil was', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core', description: '# Old' });
    render(<Page />);

    fireEvent.click(await screen.findByTestId('repo-description-edit'));
    const save = screen.getByTestId('repo-description-save');
    const cancel = screen.getByTestId('repo-description-cancel');
    expect(screen.getByTestId('block-description')).toContainElement(save);
    expect(screen.getByTestId('repo-description')).not.toContainElement(save);
    expect(screen.getByTestId('repo-description')).not.toContainElement(cancel);
    // The pencil is gone, not disabled-in-place: its slot is what the two
    // buttons occupy.
    expect(screen.queryByTestId('repo-description-edit')).not.toBeInTheDocument();

    fireEvent.click(cancel);
    expect(screen.getByTestId('repo-description-edit')).not.toBeDisabled();
  });

  // The counter rides in the heading with the buttons, for the same reason they
  // are there: appearing at 80% of the cap must not shove the page down.
  it('shows the byte counter in the block heading', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core', description: '# Old' });
    render(<Page />);

    fireEvent.click(await screen.findByTestId('repo-description-edit'));
    fireEvent.change(screen.getByTestId('repo-description-input'),
      { target: { value: 'x'.repeat(MAX_REPO_DESCRIPTION_BYTES) } });
    const count = screen.getByTestId('repo-description-count');
    expect(screen.getByTestId('block-description')).toContainElement(count);
    expect(screen.getByTestId('repo-description')).not.toContainElement(count);
  });

  // Editing a repo description writes README.md through PATCH /repos/{repo},
  // and the pane adopts the SERVER's re-read value, not the local draft.
  it('edits a repo description and saves it to README.md', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core', description: '# Old' });
    (api.updateRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core', description: '# New\n\nBody.' });
    render(<Page />);

    // The pencil opens the card AND enters edit mode in one click.
    fireEvent.click(await screen.findByTestId('repo-description-edit'));
    const input = screen.getByTestId('repo-description-input') as HTMLTextAreaElement;
    expect(input.value).toBe('# Old'); // seeded with the current markdown

    fireEvent.change(input, { target: { value: '# New\n\nBody.' } });
    fireEvent.click(screen.getByTestId('repo-description-save'));

    await waitFor(() => expect(api.updateRepo).toHaveBeenCalledWith('core', { description: '# New\n\nBody.' }));
    // Back to the rendered view, showing what the server returned.
    await waitFor(() => expect(screen.queryByTestId('repo-description-input')).not.toBeInTheDocument());
    expect(screen.getByTestId('repo-description')).toHaveTextContent('Body.');
  });

  it('keeps the editor open and shows the error when a description save fails', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core', description: '# Old' });
    (api.updateRepo as ReturnType<typeof vi.fn>).mockRejectedValue(new Error('description too long'));
    render(<Page />);

    fireEvent.click(await screen.findByTestId('repo-description-edit'));
    fireEvent.change(screen.getByTestId('repo-description-input'), { target: { value: 'x' } });
    fireEvent.click(screen.getByTestId('repo-description-save'));

    await waitFor(() => expect(screen.getByText(/description too long/)).toBeInTheDocument());
    // The draft is not lost — the user can fix it and retry.
    expect((screen.getByTestId('repo-description-input') as HTMLTextAreaElement).value).toBe('x');
  });

  it('cancelling a description edit discards the draft', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core', description: '# Old' });
    render(<Page />);

    fireEvent.click(await screen.findByTestId('repo-description-edit'));
    fireEvent.change(screen.getByTestId('repo-description-input'), { target: { value: 'scratch' } });
    fireEvent.click(screen.getByTestId('repo-description-cancel'));

    expect(api.updateRepo).not.toHaveBeenCalled();
    expect(screen.queryByTestId('repo-description-input')).not.toBeInTheDocument();
    // Re-opening starts from the persisted value again, not the discarded draft.
    fireEvent.click(screen.getByTestId('repo-description-edit'));
    expect((screen.getByTestId('repo-description-input') as HTMLTextAreaElement).value).toBe('# Old');
  });

  // A lens description is capped an order of magnitude below a repo's, and the
  // two share one editor — so the editor has to name the cap it is holding
  // rather than let the difference surface as a 422 on Save.
  it('counts bytes against the lens cap and blocks Save when over', async () => {
    (api.getLens as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'dev', write: { uid: 'uid-work', name: 'work' }, reads: [{ uid: 'uid-work', name: 'work' }], description: 'note',
    });
    renderLens();
    fireEvent.click(await screen.findByTestId('repo-description-edit'));

    // A short draft is nowhere near the cap: no counter, Save enabled.
    expect(screen.queryByTestId('repo-description-count')).not.toBeInTheDocument();
    expect(screen.getByTestId('repo-description-save')).not.toBeDisabled();

    fireEvent.change(screen.getByTestId('repo-description-input'),
      { target: { value: 'x'.repeat(MAX_LENS_DESCRIPTION_BYTES + 1) } });
    expect(screen.getByTestId('repo-description-count')).toHaveTextContent('4,097 / 4,096 bytes');
    expect(screen.getByTestId('repo-description-save')).toBeDisabled();

    fireEvent.click(screen.getByTestId('repo-description-save'));
    expect(api.updateLens).not.toHaveBeenCalled();
  });

  // Bytes, not characters — the server caps len(string), so multi-byte text
  // reaches the limit well before the character count suggests.
  it('measures the draft in bytes, not characters', async () => {
    (api.getLens as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'dev', write: { uid: 'uid-work', name: 'work' }, reads: [{ uid: 'uid-work', name: 'work' }], description: 'note',
    });
    renderLens();
    fireEvent.click(await screen.findByTestId('repo-description-edit'));

    // 3-byte characters: under the cap by count, over it by size.
    fireEvent.change(screen.getByTestId('repo-description-input'),
      { target: { value: '—'.repeat(2000) } });
    expect(screen.getByTestId('repo-description-count')).toHaveTextContent('6,000 / 4,096 bytes');
    expect(screen.getByTestId('repo-description-save')).toBeDisabled();
  });

  it('uses the much larger repo cap for a repo description', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core', description: '# Old' });
    render(<Page />);
    fireEvent.click(await screen.findByTestId('repo-description-edit'));

    // Over a lens's cap, nowhere near a repo's: no counter, Save enabled.
    fireEvent.change(screen.getByTestId('repo-description-input'),
      { target: { value: 'x'.repeat(MAX_LENS_DESCRIPTION_BYTES + 1) } });
    expect(screen.queryByTestId('repo-description-count')).not.toBeInTheDocument();
    expect(screen.getByTestId('repo-description-save')).not.toBeDisabled();
  });

  it('hides the description edit affordance in read-only mode', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({ name: 'core', description: '# Old' });
    render(<Page serverReadOnly />);
    await screen.findByTestId('repo-description');
    expect(screen.queryByTestId('repo-description-edit')).not.toBeInTheDocument();
  });

  // A lens description goes to the lens registry via PATCH /lenses/{name} —
  // same editor, different destination.
  it('edits a lens description and saves it via updateLens', async () => {
    (api.getLens as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'dev', write: { uid: 'uid-work', name: 'work' }, reads: [{ uid: 'uid-core', name: 'core', branch: 'main' }, { uid: 'uid-work', name: 'work' }],
      description: 'old lens note',
    });
    (api.updateLens as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'dev', write: { uid: 'uid-work', name: 'work' }, reads: [{ uid: 'uid-core', name: 'core', branch: 'main' }, { uid: 'uid-work', name: 'work' }],
      description: '# New lens note',
    });
    renderLens();

    fireEvent.click(await screen.findByTestId('repo-description-edit'));
    fireEvent.change(screen.getByTestId('repo-description-input'), { target: { value: '# New lens note' } });
    fireEvent.click(screen.getByTestId('repo-description-save'));

    await waitFor(() => expect(api.updateLens).toHaveBeenCalledWith('dev', { description: '# New lens note' }));
    await waitFor(() => expect(screen.getByTestId('repo-description')).toHaveTextContent('New lens note'));
  });

  // A long README renders open — the fold is gone — but it still scrolls
  // within a bounded height, so it cannot push the wiring blocks off the page.
  it('renders a long description open, in its own bounded scroll', async () => {
    (api.getRepo as ReturnType<typeof vi.fn>).mockResolvedValue({
      name: 'core', description: 'line\n'.repeat(40),
    });
    render(<Page />);
    const body = await screen.findByTestId('repo-description');
    expect(screen.queryByTestId('repo-description-toggle')).not.toBeInTheDocument();
    const prose = body.querySelector('.k-prose') as HTMLElement;
    expect(prose).not.toBeNull();
    expect(prose.style.overflowY).toBe('auto');
    expect(prose.style.maxHeight).toBe('360px');
  });

  it('rebuild gives immediate feedback and a completion message', async () => {
    render(<Page />);
    fireEvent.click(await screen.findByTestId('repo-rebuild'));
    await waitFor(() => expect(api.rebuild).toHaveBeenCalledWith('core', 'agent/test'));
    // Visible confirmation that the background rebuild kicked off (the bug: none).
    await waitFor(() => expect(screen.getByTestId('rebuild-status')).toHaveTextContent('Rebuild started'));
  });

  // The server no longer refuses a rebuild with 409: a click that lands on an
  // identical running rebuild is absorbed (200, same envelope as a new job's
  // 201). Any error that does come back is shown as itself, never rewritten
  // into the old "already running" copy.
  it('reports a rebuild error verbatim, without the retired "already running" copy', async () => {
    vi.mocked(api.rebuild).mockRejectedValueOnce(new Error('/api/v1/repos/core/branches/agent:test/index-rebuilds → 409 something else'));
    render(<Page />);
    fireEvent.click(await screen.findByTestId('repo-rebuild'));
    expect(await screen.findByText(/^rebuild failed: .*409 something else/)).toBeInTheDocument();
    expect(screen.queryByText(/already running/)).not.toBeInTheDocument();
  });

  it('hideRemoteConfig hides the remote block entirely', async () => {
    // First verify the block IS present when hideRemoteConfig is false (non-vacuity check).
    const { unmount } = render(<Page hideRemoteConfig={false} />);
    await waitFor(() => expect(screen.getByTestId('block-remote')).toBeInTheDocument());
    unmount();

    // Now render with hideRemoteConfig=true and assert the panel is absent.
    render(<Page serverReadOnly hideRemoteConfig />);
    await screen.findByTestId('block-agent-branch');
    // No block, and nothing in the contents rail pointing at one.
    expect(screen.queryByTestId('block-remote')).toBeNull();
    expect(screen.queryByTestId('toc-remote')).toBeNull();
  });
});

// A subscription has no agent branch, so the block that normally names one has
// to say something true instead: the branch it READS, marked read-only. The
// server omits agent_branch for these (Task 9 DTO), so a UI that only knew
// about agent_branch would render an empty chip and no explanation.
describe('RepoSettings — a subscription', () => {
  beforeEach(() => {
    vi.mocked(api.getRepo).mockResolvedValue({ name: 'core', mode: 'subscribe', read_branch: 'main' });
    // The server's answer for a subscription: no agent branch, so getAgentBranch
    // falls back to read_branch (api.ts).
    vi.mocked(api.getAgentBranch).mockResolvedValue('main');
  });

  // The degenerate-upstream warning means "the consensus branch IS this
  // machine's agent branch, so pulls are disabled". A subscription has no agent
  // branch and is pull-only, so the warning is wrong in both directions — and
  // it fires purely because getAgentBranch now falls back to read_branch, which
  // equals the origin's branch for every subscription.
  it('does not show the push-only degenerate-upstream warning', async () => {
    vi.mocked(api.getOrigin).mockResolvedValue({
      url: 'https://example.com/kb.git', branch: 'main', auth_method: 'none',
    } as unknown as Awaited<ReturnType<typeof api.getOrigin>>);

    render(<Page />);

    // The read branch still renders — proving the page loaded and the mode was
    // applied, so the absence below is not an empty render.
    const row = await screen.findByTestId('repo-detail-branch');
    await waitFor(() => expect(screen.getByTestId('repo-readonly-badge')).toBeInTheDocument());
    expect(row.textContent).toContain('main');

    await waitFor(() => expect(api.getOrigin).toHaveBeenCalledWith('core'));
    expect(screen.queryByTestId('upstream-warning')).not.toBeInTheDocument();
  });

  // Nothing can be merged into a subscription: it has no agent branch.
  it('has no Pushed branches block', async () => {
    render(<Page />);
    await waitFor(() => expect(screen.getByTestId('repo-readonly-badge')).toBeInTheDocument());
    expect(screen.queryByTestId('block-pushed-branches')).not.toBeInTheDocument();
  });

  it('names the read branch and badges it read-only', async () => {
    render(<Page />);

    const row = await screen.findByTestId('repo-detail-branch');
    await waitFor(() => expect(screen.getByTestId('repo-readonly-badge')).toBeInTheDocument());
    expect(row.textContent).toContain('main');
    expect(row.textContent).toContain('read-only');
  });

  // #324: a subscription's CONTENT cannot change — README and LICENSE commit to
  // an agent branch it does not have — but its registry config can. The server
  // also refuses detaching or re-pointing its origin (409), so those two go;
  // the upstream stays.
  it('locks its content and keeps its admin operations', async () => {
    vi.mocked(api.getOrigin).mockResolvedValue({
      url: 'https://example.com/kb.git', branch: 'main', auth_method: 'none',
    } as unknown as Awaited<ReturnType<typeof api.getOrigin>>);
    render(<Page />);
    await waitFor(() => expect(screen.getByTestId('repo-readonly-badge')).toBeInTheDocument());

    expect(screen.queryByTestId('repo-description-edit')).not.toBeInTheDocument();
    expect(screen.queryByTestId('repo-license-edit')).not.toBeInTheDocument();

    expect(await screen.findByTestId('upstream-change')).toBeInTheDocument();
    expect(screen.queryByTestId('remote-reconnect')).not.toBeInTheDocument();
    expect(screen.queryByTestId('remote-disconnect')).not.toBeInTheDocument();
    expect(screen.getByTestId('repo-rebuild')).toBeEnabled();
    expect(screen.getByTestId('repo-archive')).toBeEnabled();
    expect(screen.getByTestId('repo-rename-input')).toBeEnabled();
  });

  // #324's other half: the pane used to take the BROWSED repo's read-only state
  // for every settings page. Browsing a subscription must not lock a writable
  // repo's page — the page describes the repo on screen.
  it('does not lock a writable repo while the subscription is the one browsed', async () => {
    vi.mocked(api.getRepo).mockImplementation(async (name: string) => (
      name === 'core'
        ? { name: 'core', mode: 'subscribe', read_branch: 'main' }
        : { name: 'work', description: '' }
    ) as Awaited<ReturnType<typeof api.getRepo>>);
    vi.mocked(api.getAgentBranch).mockResolvedValue('machine/test');
    render(<Page name="work" />);

    expect(await screen.findByTestId('repo-description-edit')).toBeInTheDocument();
    expect(screen.queryByTestId('repo-readonly-badge')).not.toBeInTheDocument();
    expect(screen.getByTestId('repo-archive')).toBeEnabled();
  });
});

// Until a repo's details answer, the page cannot tell a subscription from an
// ordinary repo. Showing the editors meanwhile would flash, on a subscription,
// controls whose every use fails (#324) — so the unknown mode locks them, and
// the lock lifts once the mode says "ordinary".
describe('RepoSettings — mode not yet known', () => {
  it('withholds content editors and origin re-pointing until the mode resolves', async () => {
    let resolveRepo!: (r: Awaited<ReturnType<typeof api.getRepo>>) => void;
    vi.mocked(api.getRepo).mockReturnValue(new Promise(res => { resolveRepo = res; }));
    vi.mocked(api.getAgentBranch).mockResolvedValue('machine/test');
    vi.mocked(api.getOrigin).mockResolvedValue({
      url: 'https://example.com/kb.git', branch: 'main', auth_method: 'none',
    } as unknown as Awaited<ReturnType<typeof api.getOrigin>>);
    render(<Page />);

    // The origin has loaded — the card is drawn — so the absences below are
    // the mode gate, not an empty page.
    expect(await screen.findByTestId('upstream-change')).toBeInTheDocument();
    expect(screen.queryByTestId('remote-reconnect')).not.toBeInTheDocument();
    expect(screen.queryByTestId('remote-disconnect')).not.toBeInTheDocument();
    expect(screen.queryByTestId('repo-description-edit')).not.toBeInTheDocument();
    expect(screen.queryByTestId('repo-license-edit')).not.toBeInTheDocument();
    // Registry operations do not wait on the mode: a subscription keeps them.
    expect(screen.getByTestId('repo-archive')).toBeEnabled();

    await act(async () => { resolveRepo({ name: 'core', description: '' } as Awaited<ReturnType<typeof api.getRepo>>); });
    expect(await screen.findByTestId('repo-description-edit')).toBeInTheDocument();
    expect(screen.getByTestId('remote-disconnect')).toBeInTheDocument();
    expect(screen.getByTestId('remote-reconnect')).toBeInTheDocument();
  });
});

// A read-only INSTANCE is the one thing that locks every control on a repo's
// page — registry operations included, which a subscription leaves open.
describe('RepoSettings — a read-only instance', () => {
  it('disables archive, rename and rebuild', async () => {
    vi.mocked(api.getRepo).mockResolvedValue({ name: 'core', description: '' });
    render(<Page serverReadOnly />);

    expect(await screen.findByTestId('repo-archive')).toBeDisabled();
    expect(screen.getByTestId('repo-rename-input')).toBeDisabled();
    expect(screen.getByTestId('repo-rebuild')).toBeDisabled();
    expect(screen.queryByTestId('repo-description-edit')).not.toBeInTheDocument();
  });
});

// ── Re-homed from RepoManager.creates.test.tsx and ManageOverview.test.tsx ──
// A repository still being created IS a repository page, in creating mode: the
// create block first, with the one action its state allows, and every part
// that cannot honestly be offered on a half-made repo withheld. It turns into
// the ordinary page IN PLACE when the job finishes.
describe('RepoSettings — creating mode', () => {
  const job = (over: Partial<RepoCreateStatus> = {}): RepoCreateStatus =>
    ({ create_id: 'c1', name: 'core', mode: 'subscribe', state: 'running', step: 'index', index_state: 'indexing', ...over });
  const mount = (createJob: RepoCreateStatus | null, onCancelCreate = vi.fn()) => (
    <RepoSettings name="core" lenses={[]} canArchive serverReadOnly={false} hideRemoteConfig={false}
      createJob={createJob} onCancelCreate={onCancelCreate}
      onArchived={() => {}} onChanged={() => {}} onRenamed={() => {}} onSelectLens={() => {}} onError={() => {}} />
  );
  beforeEach(() => { vi.clearAllMocks(); vi.mocked(api.getRepo).mockResolvedValue({ name: 'core' }); });

  it('leads with the create block and withholds what cannot be offered', async () => {
    render(mount(job({ state: 'cancelling' })));
    expect(await screen.findByTestId('create-progress')).toBeInTheDocument();
    const ids = Array.from(document.querySelectorAll('section[id]')).map(s => s.id);
    expect(ids[0]).toBe('create');
    const cancel = screen.getByTestId('create-cancel-button');
    expect(cancel).toBeDisabled();
    expect(cancel).toHaveTextContent('Cancelling…');
    // WITHHELD: nothing here can honestly be offered on a half-made repo.
    expect(screen.queryByTestId('block-danger')).toBeNull();
    expect(screen.queryByTestId('block-remote')).toBeNull();
    expect(screen.queryByTestId('repo-rebuild')).toBeNull();
    // SHOWN, read-only: what can be read stays.
    expect(await screen.findByTestId('block-agent-branch')).toBeInTheDocument();
  });

  it('offers Cancel create while the job is running, and calls the owner', async () => {
    const onCancelCreate = vi.fn();
    render(mount(job(), onCancelCreate));
    const cancel = await screen.findByTestId('create-cancel-button');
    expect(cancel).toBeEnabled();
    expect(cancel).toHaveTextContent('Cancel create');
    fireEvent.click(cancel);
    expect(onCancelCreate).toHaveBeenCalledTimes(1);
  });

  it('turns into the normal settings page when the job finishes, without remounting', async () => {
    const { rerender } = render(mount(job()));
    const page = await screen.findByTestId('repo-settings');
    expect(screen.getByTestId('create-progress')).toBeInTheDocument();
    rerender(mount(null));
    await waitFor(() => expect(screen.queryByTestId('create-progress')).toBeNull());
    expect(screen.getByTestId('repo-settings')).toBe(page);
    expect(screen.queryByTestId('block-create')).toBeNull();
    expect(await screen.findByTestId('block-danger')).toBeInTheDocument();
    expect(screen.getByTestId('repo-rebuild')).toBeInTheDocument();
  });
});

// Arriving with `focus` (the banner's Reconnect, the connect flow's return)
// lands on that block — including one that only arrives after the page has
// mounted — and only once.
describe('RepoSettings — arriving with focus', () => {
  beforeEach(() => { vi.clearAllMocks(); });

  it('still finds a block that only arrives after the page has mounted', async () => {
    vi.mocked(api.getRepo).mockResolvedValue({ name: 'core', license: 'MIT License\n\nCopyright…' });
    render(<Page focus="license" />);
    await waitFor(() => expect(screen.getByTestId('block-license')).toBeInTheDocument());
    await waitFor(() => expect(screen.getByTestId('toc-license')).toHaveAttribute('aria-current', 'true'));
  });

  it('does not re-scroll to the block once the reader has moved on', async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>(res => { release = res; });
    vi.mocked(api.getRepo).mockImplementation(async () => {
      await gate;
      return { name: 'core', license: 'MIT License' };
    });
    render(<Page focus="agent-branch" />);
    await waitFor(() => expect(screen.getByTestId('toc-agent-branch')).toHaveAttribute('aria-current', 'true'));
    // The reader takes over — a wheel releases the pin.
    fireEvent.wheel(window);
    await waitFor(() => expect(screen.getByTestId('toc-agent-branch')).not.toHaveAttribute('aria-current'));
    // NOW the late section arrives, re-running the focus effect; the
    // once-per-request guard keeps the page where the reader left it.
    await act(async () => { release(); await gate; });
    await screen.findByTestId('block-license');
    expect(screen.getByTestId('toc-agent-branch')).not.toHaveAttribute('aria-current');
    vi.mocked(api.getRepo).mockResolvedValue({ name: 'core' });
  });
});

describe('RepoSettings — Mounted in', () => {
  const mount = (lenses: Lens[]) => (
    <RepoSettings name="core" lenses={lenses} canArchive serverReadOnly={false} hideRemoteConfig={false}
      onArchived={() => {}} onChanged={() => {}} onRenamed={() => {}} onSelectLens={() => {}} onError={() => {}} />
  );
  beforeEach(() => { vi.clearAllMocks(); vi.mocked(api.getRepo).mockResolvedValue({ name: 'core' }); });

  it('is the reverse of a lens\'s read mounts', async () => {
    render(mount([
      { name: 'reads-core', write: { uid: 'uid-work', name: 'work' }, reads: [{ uid: 'uid-core', name: 'core', branch: 'main' }] },
      { name: 'writes-core', write: { uid: 'uid-core', name: 'core' }, reads: [{ uid: 'uid-core', name: 'core' }] },
      { name: 'unrelated', write: { uid: 'uid-work', name: 'work' }, reads: [{ uid: 'uid-work', name: 'work' }] },
    ]));
    const block = await screen.findByTestId('block-mounted-in');
    expect(within(block).getByTestId('repo-mounted-writes-core').textContent).toContain('write target');
    expect(within(block).getByTestId('repo-mounted-reads-core').textContent).toContain('pinned');
    expect(within(block).queryByTestId('repo-mounted-unrelated')).toBeNull();
  });

  it('has no block when no lens references the repo', async () => {
    render(mount([]));
    await screen.findByTestId('block-agent-branch');
    expect(screen.queryByTestId('block-mounted-in')).toBeNull();
  });
});
