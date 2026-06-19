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
  deleteSession: vi.fn(),
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
});
