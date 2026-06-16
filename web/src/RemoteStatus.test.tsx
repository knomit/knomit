import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { RemoteStatus } from './RemoteStatus';
import { api } from './api';

vi.mock('./api', () => ({
  api: { getOrigin: vi.fn(), deleteOrigin: vi.fn(), setOriginUpstream: vi.fn(), listBranchNames: vi.fn() },
}));
type Fn = ReturnType<typeof vi.fn>;

describe('RemoteStatus', () => {
  beforeEach(() => vi.clearAllMocks());

  it('shows "Connect a remote" when not connected', async () => {
    (api.getOrigin as unknown as Fn).mockResolvedValueOnce(null);
    const onConnect = vi.fn();
    render(<RemoteStatus repo="work" agentBranch="agent/host-1" readOnly={false} onConnect={onConnect} onChanged={() => {}} />);
    const btn = await screen.findByTestId('remote-connect');
    expect(btn).toBeInTheDocument();
    fireEvent.click(btn);
    expect(onConnect).toHaveBeenCalled();
  });

  it('shows status + actions when connected, and confirms before disconnecting', async () => {
    (api.getOrigin as unknown as Fn).mockResolvedValueOnce({
      name: 'origin', url: 'https://github.com/knomit/knowkb.git', branch: 'main', auth_method: 'token',
      last_sync_at: '2026-06-11T10:00:00Z', last_status: 'ok', last_error: null,
    });
    (api.deleteOrigin as unknown as Fn).mockResolvedValueOnce(undefined);
    const onChanged = vi.fn();
    render(<RemoteStatus repo="work" agentBranch="agent/host-1" readOnly={false} onConnect={() => {}} onChanged={onChanged} />);

    await screen.findByText('https://github.com/knomit/knowkb.git');
    expect(screen.getByText(/last sync/)).toBeInTheDocument();

    // Disconnect requires an inline confirm (no browser prompt).
    fireEvent.click(screen.getByTestId('remote-disconnect'));
    expect(api.deleteOrigin).not.toHaveBeenCalled();
    fireEvent.click(screen.getByTestId('disconnect-confirm'));
    await waitFor(() => expect(api.deleteOrigin).toHaveBeenCalledWith('work'));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
  });

  it('warns when the upstream branch equals this machine\'s agent branch', async () => {
    (api.getOrigin as unknown as Fn).mockResolvedValueOnce({
      name: 'origin', url: 'https://github.com/knomit/knomit-kb.git', branch: 'agent/host-1', auth_method: 'token',
      last_sync_at: null, last_status: null, last_error: null,
    });
    render(<RemoteStatus repo="work" agentBranch="agent/host-1" readOnly={false} onConnect={() => {}} onChanged={() => {}} />);

    await screen.findByTestId('upstream-warning');
    expect(screen.getByTestId('upstream-warning-icon')).toBeInTheDocument();
    expect(screen.getByText(/not pulled/)).toBeInTheDocument();
  });

  it('does NOT warn when the upstream is a real consensus branch', async () => {
    (api.getOrigin as unknown as Fn).mockResolvedValueOnce({
      name: 'origin', url: 'https://github.com/knomit/knomit-kb.git', branch: 'main', auth_method: 'token',
      last_sync_at: null, last_status: null, last_error: null,
    });
    render(<RemoteStatus repo="work" agentBranch="agent/host-1" readOnly={false} onConnect={() => {}} onChanged={() => {}} />);

    await screen.findByText('https://github.com/knomit/knomit-kb.git');
    expect(screen.queryByTestId('upstream-warning')).not.toBeInTheDocument();
  });

  it('changes the upstream branch via the inline editor', async () => {
    (api.getOrigin as unknown as Fn).mockResolvedValueOnce({
      name: 'origin', url: 'https://github.com/knomit/knomit-kb.git', branch: 'agent/host-1', auth_method: 'token',
      last_sync_at: null, last_status: null, last_error: null,
    }).mockResolvedValueOnce({
      name: 'origin', url: 'https://github.com/knomit/knomit-kb.git', branch: 'main', auth_method: 'token',
      last_sync_at: null, last_status: null, last_error: null,
    });
    (api.listBranchNames as unknown as Fn).mockResolvedValueOnce(['main', 'agent/host-1']);
    (api.setOriginUpstream as unknown as Fn).mockResolvedValueOnce(undefined);
    const onChanged = vi.fn();
    render(<RemoteStatus repo="work" agentBranch="agent/host-1" readOnly={false} onConnect={() => {}} onChanged={onChanged} />);

    fireEvent.click(await screen.findByTestId('upstream-change'));
    const input = await screen.findByTestId('upstream-input') as HTMLInputElement;
    expect(input.value).toBe('main'); // defaults to main
    fireEvent.click(screen.getByTestId('upstream-save'));

    await waitFor(() => expect(api.setOriginUpstream).toHaveBeenCalledWith('work', 'main'));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
  });
});
