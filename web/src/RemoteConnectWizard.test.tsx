import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { RemoteConnectWizard } from './RemoteConnectWizard';

vi.mock('./api', () => ({
  api: { getOrigin: vi.fn() },
  createSession: vi.fn(),
  streamTest: vi.fn(),
  streamPreview: vi.fn(),
  streamApply: vi.fn(),
  streamCommit: vi.fn(),
  // Resolved, not bare: every caller does `deleteSession(...).catch(...)`, so a
  // stub returning undefined throws inside a click handler — and clearAllMocks
  // clears calls, not implementations, so this survives beforeEach.
  deleteSession: vi.fn().mockResolvedValue(undefined),
}));

import { api, createSession, streamTest, streamPreview, streamApply, streamCommit } from './api';
type Fn = ReturnType<typeof vi.fn>;

describe('RemoteConnectWizard', () => {
  beforeEach(() => vi.clearAllMocks());

  it('prefills URL/auth from an existing origin, leaving the token blank', async () => {
    (api.getOrigin as unknown as Fn).mockResolvedValueOnce({
      name: 'origin', url: 'https://github.com/knomit/knowkb.git', branch: 'main', auth_method: 'token',
    });
    render(<RemoteConnectWizard repo="knomit" onCancel={() => {}} onDone={() => {}} />);

    const url = await screen.findByTestId('wizard-url') as HTMLInputElement;
    await waitFor(() => expect(url.value).toBe('https://github.com/knomit/knowkb.git'));
    const tok = await screen.findByPlaceholderText('ghp_…') as HTMLInputElement;
    expect(tok.value).toBe('');
    expect(api.getOrigin).toHaveBeenCalledWith('knomit');
  });

  it('advances Connect→Review on a successful test and hides the strategy picker for shared history', async () => {
    (api.getOrigin as unknown as Fn).mockResolvedValueOnce(null);
    (createSession as unknown as Fn).mockResolvedValueOnce({ session_id: 'sess-1' });
    (streamTest as unknown as Fn).mockImplementation((_r: string, _s: string, onEvent: (e: unknown) => void) => {
      queueMicrotask(() => onEvent({
        phase: 'done',
        result: { branches: ['main'], agent_branches: [], default_branch: 'main', matched_agent: '', history: 'shared', remote_fact_count: 69, local_fact_count: 270 },
      }));
      return () => {};
    });
    (streamPreview as unknown as Fn).mockImplementation((_r: string, _s: string, onEvent: (e: unknown) => void) => {
      queueMicrotask(() => onEvent({ phase: 'done', result: { local_only: 209, remote_only: 8, shared_path: 61, dead_refs_found: 14 } }));
      return () => {};
    });

    render(<RemoteConnectWizard repo="knomit" onCancel={() => {}} onDone={() => {}} />);
    const url = await screen.findByTestId('wizard-url') as HTMLInputElement;
    fireEvent.change(url, { target: { value: 'https://example.com/repo.git' } });
    fireEvent.click(screen.getByTestId('wizard-test'));

    // Reached the Review step (connect action present) with the review summary.
    await screen.findByTestId('wizard-connect');
    expect(screen.getByText(/shared histories/)).toBeInTheDocument();
    // Shared history → no conflict-strategy picker.
    expect(screen.queryByText(/Conflict strategy:/)).toBeNull();
    expect(screen.queryByText(/Local wins/)).toBeNull();
  });

  // Regression: a brand-new/empty remote returns branches: null (Go marshals a
  // nil slice as null). The Review step must not crash on .branches.map.
  it('does not crash on Review when the remote has no branches (null)', async () => {
    (api.getOrigin as unknown as Fn).mockResolvedValueOnce(null);
    (createSession as unknown as Fn).mockResolvedValueOnce({ session_id: 'sess-2' });
    (streamTest as unknown as Fn).mockImplementation((_r: string, _s: string, onEvent: (e: unknown) => void) => {
      queueMicrotask(() => onEvent({
        phase: 'done',
        result: { branches: null, agent_branches: null, default_branch: '', matched_agent: '', history: 'disjoint', remote_fact_count: 0, local_fact_count: 5 },
      }));
      return () => {};
    });
    (streamPreview as unknown as Fn).mockImplementation((_r: string, _s: string, onEvent: (e: unknown) => void) => {
      queueMicrotask(() => onEvent({ phase: 'done', result: { local_only: 5, remote_only: 0, shared_path: 0, dead_refs_found: 0 } }));
      return () => {};
    });

    render(<RemoteConnectWizard repo="knomit-kb" onCancel={() => {}} onDone={() => {}} />);
    const url = await screen.findByTestId('wizard-url') as HTMLInputElement;
    fireEvent.change(url, { target: { value: 'https://github.com/knomit/knomit-kb' } });
    fireEvent.click(screen.getByTestId('wizard-test'));

    expect(await screen.findByText('(no remote branches yet)')).toBeInTheDocument();
  });

  // Regression: shared history must connect in ONE click (apply + commit
  // chained), not two identical "Connect" clicks.
  it('shared history: a single Connect click runs both apply and commit', async () => {
    (api.getOrigin as unknown as Fn).mockResolvedValueOnce(null);
    (createSession as unknown as Fn).mockResolvedValueOnce({ session_id: 'sess-3' });
    (streamTest as unknown as Fn).mockImplementation((_r: string, _s: string, onEvent: (e: unknown) => void) => {
      queueMicrotask(() => onEvent({ phase: 'done', result: { branches: ['main'], agent_branches: [], default_branch: 'main', matched_agent: '', history: 'shared', remote_fact_count: 5, local_fact_count: 7 } }));
      return () => {};
    });
    (streamPreview as unknown as Fn).mockImplementation((_r: string, _s: string, onEvent: (e: unknown) => void) => {
      queueMicrotask(() => onEvent({ phase: 'done', result: { local_only: 2, remote_only: 1, shared_path: 4, dead_refs_found: 0 } }));
      return () => {};
    });
    (streamApply as unknown as Fn).mockImplementation(async (_r: string, _s: string, _strat: string, _b: string | undefined, onEvent: (e: unknown) => void) => {
      onEvent({ phase: 'done', result: { total_facts: 0, from_local: 0, from_remote: 0, overwrites: 0 } });
    });
    (streamCommit as unknown as Fn).mockImplementation(async (_r: string, _s: string, onEvent: (e: unknown) => void) => {
      onEvent({ phase: 'done' });
    });

    render(<RemoteConnectWizard repo="knomit-kb" onCancel={() => {}} onDone={() => {}} />);
    const url = await screen.findByTestId('wizard-url') as HTMLInputElement;
    fireEvent.change(url, { target: { value: 'https://example.com/repo.git' } });
    fireEvent.click(screen.getByTestId('wizard-test'));

    const connect = await screen.findByTestId('wizard-connect');
    fireEvent.click(connect);

    await waitFor(() => expect(streamApply).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(streamCommit).toHaveBeenCalledTimes(1));
  });

  // The session is the whole of step ①, so its failure is step ①'s only failure
  // path. It used to strand the page: the catch asked `step === 'creating'`,
  // which is the value captured when the button was clicked — always 'idle' —
  // so nothing reset, every control stayed disabled by `busy`, and the error box
  // (gated on 'idle') never rendered the reason either.
  it('returns to the form, with the reason, when the session cannot be created', async () => {
    (api.getOrigin as unknown as Fn).mockResolvedValueOnce(null);
    (createSession as unknown as Fn).mockRejectedValueOnce(new Error('dial tcp: connection refused'));

    render(<RemoteConnectWizard repo="knomit-kb" onCancel={() => {}} onDone={() => {}} />);
    const url = await screen.findByTestId('wizard-url') as HTMLInputElement;
    fireEvent.change(url, { target: { value: 'https://example.com/repo.git' } });
    fireEvent.click(screen.getByTestId('wizard-test'));

    expect(await screen.findByText('dial tcp: connection refused')).toBeInTheDocument();
    // Back at step ① and usable: the fields take input and Test can run again.
    expect(url.disabled).toBe(false);
    expect((screen.getByTestId('wizard-test') as HTMLButtonElement).disabled).toBe(false);
    expect(screen.getByTestId('wizard-step-1')).toHaveAttribute('aria-current', 'step');
    expect(screen.queryByText('Creating session…')).toBeNull();
  });

  // Leaving during the MERGE is allowed — nothing is written yet — but the
  // shared-history path chains apply → commit, and a commit is a store swap on
  // a session cancel() has already asked the server to delete.
  it('does not chain into the commit when the reader leaves during the merge', async () => {
    const onCancel = vi.fn();
    let finishApply: () => void = () => {};
    (api.getOrigin as unknown as Fn).mockResolvedValueOnce(null);
    (createSession as unknown as Fn).mockResolvedValueOnce({ session_id: 'sess-leave' });
    (streamTest as unknown as Fn).mockImplementation((_r: string, _s: string, onEvent: (e: unknown) => void) => {
      queueMicrotask(() => onEvent({ phase: 'done', result: { branches: ['main'], agent_branches: [], default_branch: 'main', matched_agent: '', history: 'shared', remote_fact_count: 5, local_fact_count: 7 } }));
      return () => {};
    });
    (streamPreview as unknown as Fn).mockImplementation((_r: string, _s: string, onEvent: (e: unknown) => void) => {
      queueMicrotask(() => onEvent({ phase: 'done', result: { local_only: 2, remote_only: 1, shared_path: 4, dead_refs_found: 0 } }));
      return () => {};
    });
    // Held open so the crumb can be clicked mid-merge, then completed.
    (streamApply as unknown as Fn).mockImplementation((_r: string, _s: string, _strat: string, _b: string | undefined, onEvent: (e: unknown) => void) =>
      new Promise<void>(resolve => {
        finishApply = () => { onEvent({ phase: 'done', result: { total_facts: 0, from_local: 0, from_remote: 0, overwrites: 0 } }); resolve(); };
      }));
    (streamCommit as unknown as Fn).mockResolvedValue(undefined);

    render(<RemoteConnectWizard repo="knomit-kb" onCancel={onCancel} onDone={() => {}} />);
    fireEvent.change(await screen.findByTestId('wizard-url'), { target: { value: 'https://example.com/repo.git' } });
    fireEvent.click(screen.getByTestId('wizard-test'));
    fireEvent.click(await screen.findByTestId('wizard-connect'));

    // The crumb is live during the merge — that is the point — and it is the
    // one enabled control while the buttons below are disabled by `busy`.
    await waitFor(() => expect(streamApply).toHaveBeenCalledTimes(1));
    expect((screen.getByTestId('wizard-crumb-back') as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(screen.getByTestId('wizard-crumb-back'));
    expect(onCancel).toHaveBeenCalled();

    // Let the merge finish and the continuation after its await run.
    finishApply();
    await new Promise(r => setTimeout(r, 0));
    await new Promise(r => setTimeout(r, 0));
    expect(streamCommit).not.toHaveBeenCalled();
  });

  const mockTestPreviewOK = () => {
    (api.getOrigin as unknown as Fn).mockResolvedValueOnce(null);
    (createSession as unknown as Fn).mockResolvedValueOnce({ session_id: 'sess-local' });
    (streamTest as unknown as Fn).mockImplementation((_r: string, _s: string, onEvent: (e: unknown) => void) => {
      queueMicrotask(() => onEvent({ phase: 'done', result: { branches: ['main'], agent_branches: [], default_branch: 'main', matched_agent: '', history: 'disjoint', remote_fact_count: 1, local_fact_count: 0 } }));
      return () => {};
    });
    (streamPreview as unknown as Fn).mockImplementation((_r: string, _s: string, onEvent: (e: unknown) => void) => {
      queueMicrotask(() => onEvent({ phase: 'done', result: { local_only: 0, remote_only: 1, shared_path: 0, dead_refs_found: 0 } }));
      return () => {};
    });
  };

  // Auto-detect omits auth_method, letting the backend infer anonymous/SSH from
  // the URL. Select away to 'none' and back to '' so the assertion exercises the
  // Auto-detect option's wiring (and the '' -> omitted mapping) rather than just
  // the component's initial default state.
  it('connects a local path with auto-detect, omitting auth_method', async () => {
    mockTestPreviewOK();
    render(<RemoteConnectWizard repo="knomit-kb" onCancel={() => {}} onDone={() => {}} />);
    const url = await screen.findByTestId('wizard-url') as HTMLInputElement;
    fireEvent.change(url, { target: { value: '/srv/kb' } });
    const select = screen.getByRole('combobox');
    fireEvent.change(select, { target: { value: 'none' } });   // move off the default
    fireEvent.change(select, { target: { value: '' } });       // explicitly choose Auto-detect
    fireEvent.click(screen.getByTestId('wizard-test'));

    await waitFor(() => expect(createSession).toHaveBeenCalled());
    const opts = (createSession as unknown as Fn).mock.calls[0][1];
    expect(opts).toMatchObject({ url: '/srv/kb' });
    expect(opts.auth_method).toBeUndefined();
  });

  // Selecting None explicitly sends auth_method "none" (explicit anonymous) so
  // the backend does NOT auto-promote an SSH-style URL to SSH auth.
  it('sends auth_method "none" when None is explicitly selected', async () => {
    mockTestPreviewOK();
    render(<RemoteConnectWizard repo="knomit-kb" onCancel={() => {}} onDone={() => {}} />);
    const url = await screen.findByTestId('wizard-url') as HTMLInputElement;
    fireEvent.change(url, { target: { value: 'git@github.com:user/repo.git' } });
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'none' } });
    fireEvent.click(screen.getByTestId('wizard-test'));

    await waitFor(() => expect(createSession).toHaveBeenCalled());
    const opts = (createSession as unknown as Fn).mock.calls[0][1];
    expect(opts).toMatchObject({ url: 'git@github.com:user/repo.git', auth_method: 'none' });
  });

  // SSH URL + explicit None shows a non-blocking advisory but still lets the
  // user run the connectivity test (the override stays usable).
  it('warns but does not block on SSH URL + None', async () => {
    (api.getOrigin as unknown as Fn).mockResolvedValueOnce(null);
    render(<RemoteConnectWizard repo="knomit-kb" onCancel={() => {}} onDone={() => {}} />);
    const url = await screen.findByTestId('wizard-url') as HTMLInputElement;

    // No warning for a local path under None.
    fireEvent.change(url, { target: { value: '/srv/kb' } });
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'none' } });
    expect(screen.queryByTestId('wizard-auth-warning')).toBeNull();

    // SSH-style URL + None → advisory appears, Test stays enabled.
    fireEvent.change(url, { target: { value: 'git@github.com:user/repo.git' } });
    expect(screen.getByTestId('wizard-auth-warning')).toBeTruthy();
    expect((screen.getByTestId('wizard-test') as HTMLButtonElement).disabled).toBe(false);
  });

  // The commit is the one irreversible step: it swaps the store and rebuilds
  // the index, streamCommit has no abort, and nothing in this component is
  // registered to stop it. So "can the reader leave" is a claim these tests own
  // — including the case where the page is no longer the whole surface and the
  // manager's rail can unmount it.
  describe('the commit lock', () => {
    // Shared history connects in one click (apply chains into commit), so this
    // reaches the commit with a single button press. `commit` is the stream
    // implementation under test.
    const driveToCommit = async (
      commit: (onEvent: (e: unknown) => void) => Promise<void>,
      props: Partial<React.ComponentProps<typeof RemoteConnectWizard>> = {},
    ) => {
      (api.getOrigin as unknown as Fn).mockResolvedValueOnce(null);
      (createSession as unknown as Fn).mockResolvedValueOnce({ session_id: 'sess-commit' });
      (streamTest as unknown as Fn).mockImplementation((_r: string, _s: string, onEvent: (e: unknown) => void) => {
        queueMicrotask(() => onEvent({ phase: 'done', result: { branches: ['main'], agent_branches: [], default_branch: 'main', matched_agent: '', history: 'shared', remote_fact_count: 5, local_fact_count: 7 } }));
        return () => {};
      });
      (streamPreview as unknown as Fn).mockImplementation((_r: string, _s: string, onEvent: (e: unknown) => void) => {
        queueMicrotask(() => onEvent({ phase: 'done', result: { local_only: 2, remote_only: 1, shared_path: 4, dead_refs_found: 0 } }));
        return () => {};
      });
      (streamApply as unknown as Fn).mockImplementation(async (_r: string, _s: string, _strat: string, _b: string | undefined, onEvent: (e: unknown) => void) => {
        onEvent({ phase: 'done', result: { total_facts: 0, from_local: 0, from_remote: 0, overwrites: 0 } });
      });
      (streamCommit as unknown as Fn).mockImplementation((_r: string, _s: string, onEvent: (e: unknown) => void) => commit(onEvent));

      const view = render(<RemoteConnectWizard repo="knomit-kb" onCancel={() => {}} onDone={() => {}} {...props} />);
      const url = await screen.findByTestId('wizard-url') as HTMLInputElement;
      fireEvent.change(url, { target: { value: 'https://example.com/repo.git' } });
      fireEvent.click(screen.getByTestId('wizard-test'));
      fireEvent.click(await screen.findByTestId('wizard-connect'));
      return view;
    };

    const crumb = () => screen.getByTestId('wizard-crumb-back') as HTMLButtonElement;

    it('holds the exit while the commit runs and releases it on unmount', async () => {
      const onBusyChange = vi.fn();
      // Never resolves: the commit is still in flight for the whole test.
      const { unmount } = await driveToCommit(() => new Promise<void>(() => {}), { onBusyChange });

      await waitFor(() => expect(crumb().disabled).toBe(true));
      expect(onBusyChange.mock.calls.at(-1)?.[0]).toBe(true);

      // The lock is published, not just enforced here — the manager's rail is
      // the other exit, and it can only refuse what it has been told about.
      unmount();
      expect(onBusyChange.mock.calls.at(-1)?.[0]).toBe(false);
    });

    // A failed commit is NOT in flight: leaving it locked strands the reader on
    // a page whose only exit is disabled, under a note telling them to wait for
    // work that already died. The step stays 'committing' regardless — that is
    // what keeps the Sync block, and with it the error, on screen.
    it('reopens the exit when the commit fails, without losing the error', async () => {
      const onBusyChange = vi.fn();
      await driveToCommit(async onEvent => { onEvent({ phase: 'error', message: 'swap failed: boom' }); }, { onBusyChange });

      expect(await screen.findByText('swap failed: boom')).toBeInTheDocument();
      await waitFor(() => expect(crumb().disabled).toBe(false));
      expect(onBusyChange.mock.calls.at(-1)?.[0]).toBe(false);
      expect(screen.getByTestId('wizard-step-3')).toHaveAttribute('aria-current', 'step');
      expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument();
    });

    // Retry lands on step ②, and shared history used to have nothing there: the
    // Connect button excluded it, the Preview button excludes it by design, and
    // what was left was "← Back", which deletes the session. So the one error
    // the reader is most likely to want to retry was the one they could not.
    it('offers the retry it lands on after a failed commit, for shared history too', async () => {
      let fail = true;
      await driveToCommit(async onEvent => {
        if (fail) { fail = false; onEvent({ phase: 'error', message: 'swap failed: boom' }); return; }
        onEvent({ phase: 'done' });
      });

      fireEvent.click(await screen.findByRole('button', { name: 'Retry' }));

      const connect = await screen.findByTestId('wizard-connect');
      fireEvent.click(connect);
      await waitFor(() => expect(streamCommit).toHaveBeenCalledTimes(2));
      // The retry re-runs the COMMIT only — the merge already happened, and
      // replaying it would be a second write of the same reconciliation.
      expect(streamApply).toHaveBeenCalledTimes(1);
      expect(await screen.findByText('Remote connected successfully.')).toBeInTheDocument();
    });

    // onDone MOVES THE SELECTION. Left running past unmount, the success pause
    // yanks a reader who navigated away back onto this repo's settings page.
    it('cancels the success pause on unmount rather than navigating later', async () => {
      const onDone = vi.fn();
      const { unmount } = await driveToCommit(async onEvent => { onEvent({ phase: 'done' }); }, { onDone });

      expect(await screen.findByText('Remote connected successfully.')).toBeInTheDocument();
      expect(onDone).not.toHaveBeenCalled();   // the pause has started, not elapsed
      unmount();

      await new Promise(r => setTimeout(r, 1400));   // longer than the 1200ms pause
      expect(onDone).not.toHaveBeenCalled();
    });
  });
});
