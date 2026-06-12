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
});
