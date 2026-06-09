import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { ConnectRemoteModal } from './ConnectRemoteModal';

vi.mock('./api', () => ({
  api: {
    getOrigin: vi.fn(),
  },
  createSession: vi.fn(),
  streamTest: vi.fn(),
  streamPreview: vi.fn(),
  streamApply: vi.fn(),
  streamCommit: vi.fn(),
  deleteSession: vi.fn(),
}));

import { api, createSession, streamTest, streamPreview, streamApply } from './api';

describe('ConnectRemoteModal pre-fill from existing origin', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('pre-fills the URL and auth method from GET /origin when one is already configured, leaving the token blank', async () => {
    (api.getOrigin as unknown as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      name: 'origin',
      url: 'https://github.com/knomit/knowkb.git',
      branch: 'main',
      auth_method: 'token',
    });

    render(<ConnectRemoteModal repo="knomit" onClose={() => {}} />);

    const urlInput = await screen.findByTestId('connect-remote-url-input') as HTMLInputElement;
    await waitFor(() => {
      expect(urlInput.value).toBe('https://github.com/knomit/knowkb.git');
    });

    // Auth method select: rendered as a <select> with no testid, so we
    // find it via the parent label's order — but simpler is to assert the
    // token input becomes visible because authMethod === 'token'.
    const tokenInput = await screen.findByPlaceholderText('ghp_...') as HTMLInputElement;
    // Secret must NEVER be pre-filled, even if it were returned by the API.
    expect(tokenInput.value).toBe('');

    expect(api.getOrigin).toHaveBeenCalledWith('knomit');
  });

  it('leaves the form blank when no origin is configured (API returns null on 204)', async () => {
    (api.getOrigin as unknown as ReturnType<typeof vi.fn>).mockResolvedValueOnce(null);

    render(<ConnectRemoteModal repo="knomit" onClose={() => {}} />);

    const urlInput = await screen.findByTestId('connect-remote-url-input') as HTMLInputElement;
    // Give the effect a tick to settle.
    await waitFor(() => {
      expect(api.getOrigin).toHaveBeenCalledWith('knomit');
    });
    expect(urlInput.value).toBe('');
  });
});

describe('ConnectRemoteModal shared-history merge workflow', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  // For shared history the backend skips the replay/swap. The UI must reflect
  // that: no "Local wins / Remote wins" picker, no misleading "0 total facts"
  // line, and the final action button labelled "Connect" rather than "Apply".
  it('hides the strategy picker and 0/0/0 line, and labels the action "Connect"', async () => {
    (api.getOrigin as unknown as ReturnType<typeof vi.fn>).mockResolvedValueOnce(null);

    (createSession as unknown as ReturnType<typeof vi.fn>)
      .mockResolvedValueOnce({ session_id: 'sess-1' });

    // Test step → shared history.
    (streamTest as unknown as ReturnType<typeof vi.fn>).mockImplementation((_repo, _sid, onEvent) => {
      queueMicrotask(() => onEvent({
        phase: 'done',
        result: {
          branches: ['main'],
          agent_branches: [],
          default_branch: 'main',
          matched_agent: '',
          history: 'shared',
          remote_fact_count: 69,
          local_fact_count: 270,
        },
      }));
      return () => {};
    });

    // Preview step → some local-only / remote-only counts to show.
    (streamPreview as unknown as ReturnType<typeof vi.fn>).mockImplementation((_repo, _sid, onEvent) => {
      queueMicrotask(() => onEvent({
        phase: 'done',
        result: { local_only: 209, remote_only: 8, shared_path: 61, dead_refs_found: 14 },
      }));
      return () => {};
    });

    // Apply step → backend no-op for shared history (returns zeros).
    (streamApply as unknown as ReturnType<typeof vi.fn>).mockImplementation(async (_repo, _sid, _strategy, _branch, onEvent) => {
      onEvent({
        phase: 'done',
        result: {
          total_facts: 0, from_local: 0, from_remote: 0,
          overwrites: 0, refs_resolved_from_history: 0, dangling_refs_dropped: 0,
        },
      });
    });

    render(<ConnectRemoteModal repo="knomit" onClose={() => {}} />);

    // Fill URL and trigger Test Connection.
    const urlInput = await screen.findByTestId('connect-remote-url-input') as HTMLInputElement;
    fireEvent.change(urlInput, { target: { value: 'https://example.com/repo.git' } });
    fireEvent.click(screen.getByTestId('connect-remote-test-btn'));

    // Wait until the "Continue" button (post-preview action) shows up.
    const applyBtn = await screen.findByTestId('connect-remote-apply-btn');
    expect(applyBtn.textContent).toBe('Continue');

    // The strategy radios MUST be hidden for shared history.
    expect(screen.queryByText(/Conflict strategy:/)).toBeNull();
    expect(screen.queryByText(/Local wins/)).toBeNull();
    expect(screen.queryByText(/Remote wins/)).toBeNull();

    // The shared-history notice should be visible in the apply section.
    expect(screen.getByTestId('shared-history-notice')).toBeInTheDocument();

    // Trigger apply and wait for the final "Connect" button to appear.
    fireEvent.click(applyBtn);
    const commitBtn = await screen.findByTestId('connect-remote-commit-btn');
    expect(commitBtn.textContent).toBe('Connect');

    // The misleading "0 total facts: 0 local, 0 remote" line MUST NOT render.
    expect(screen.queryByText(/0 total facts/)).toBeNull();
    expect(screen.queryByText(/Merge preview ready/)).toBeNull();

    // The "Try Different Strategy" escape hatch is meaningless without strategies.
    expect(screen.queryByText(/Try Different Strategy/)).toBeNull();

    // And the user-facing readiness message is the shared-history one.
    expect(screen.getByTestId('shared-history-ready')).toBeInTheDocument();
  });
});
