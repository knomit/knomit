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

import { api, createSession, streamTest, streamPreview } from './api';
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
});
