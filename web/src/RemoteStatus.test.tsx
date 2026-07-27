import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { RemoteCard } from './RemoteStatus';
import { useRemote } from './useRemote';
import { api } from './api';

vi.mock('./api', () => ({
  api: { getOrigin: vi.fn(), deleteOrigin: vi.fn(), setOriginUpstream: vi.fn(), listBranchNames: vi.fn() },
}));
type Fn = ReturnType<typeof vi.fn>;

// RemoteCard is the display half of the remote; RepoDetail owns the loaded
// state (its ⋯ menu needs it too). Harness pairs them the way RepoDetail does.
// Disconnect now lives in that menu — covered by RepoManager.test.tsx.
function Harness({ repo, agentBranch, readOnly, onConnect, onChanged }: {
  repo: string; agentBranch: string; readOnly: boolean; onConnect: () => void; onChanged: () => void;
}) {
  const state = useRemote(repo);
  return <RemoteCard repo={repo} agentBranch={agentBranch} readOnly={readOnly} state={state}
    onConnect={onConnect} onDisconnect={() => {}} onChanged={onChanged} />;
}

describe('RemoteCard', () => {
  beforeEach(() => vi.clearAllMocks());

  // An unconnected repo has no remote state, so there is no card to show —
  // "Connect a remote…" lives in the detail pane's ⋯ menu instead.
  it('renders nothing when there is no remote', async () => {
    (api.getOrigin as unknown as Fn).mockResolvedValueOnce(null);
    const { container } = render(<Harness repo="work" agentBranch="agent/host-1" readOnly={false} onConnect={() => {}} onChanged={() => {}} />);
    await waitFor(() => expect(container).toBeEmptyDOMElement());
  });

  it('shows the url and sync status when connected, with card-local actions', async () => {
    (api.getOrigin as unknown as Fn).mockResolvedValueOnce({
      name: 'origin', url: 'https://github.com/knomit/knowkb.git', branch: 'main', auth_method: 'token',
      last_sync_at: '2026-06-11T10:00:00Z', last_status: 'ok', last_error: null,
    });
    render(<Harness repo="work" agentBranch="agent/host-1" readOnly={false} onConnect={() => {}} onChanged={() => {}} />);

    await screen.findByText('https://github.com/knomit/knowkb.git');
    expect(screen.getByText(/last sync/)).toBeInTheDocument();
    // Reconnect/disconnect edit THIS card's data, so they sit on the card.
    expect(screen.getByTestId('remote-reconnect')).toBeInTheDocument();
    expect(screen.getByTestId('remote-disconnect')).toBeInTheDocument();
  });

  it('hides the card actions in read-only mode', async () => {
    (api.getOrigin as unknown as Fn).mockResolvedValueOnce({
      name: 'origin', url: 'https://github.com/knomit/knowkb.git', branch: 'main', auth_method: 'token',
      last_sync_at: null, last_status: null, last_error: null,
    });
    render(<Harness repo="work" agentBranch="agent/host-1" readOnly onConnect={() => {}} onChanged={() => {}} />);
    await screen.findByText('https://github.com/knomit/knowkb.git');
    expect(screen.queryByTestId('remote-reconnect')).not.toBeInTheDocument();
    expect(screen.queryByTestId('remote-disconnect')).not.toBeInTheDocument();
  });

  it('renders a sync failure (status "error", not "failed") with the error text', async () => {
    (api.getOrigin as unknown as Fn).mockResolvedValueOnce({
      name: 'origin', url: 'https://github.com/knomit/knomit-kb.git', branch: 'main', auth_method: 'token',
      last_sync_at: null, last_status: 'error', last_error: 'authentication required: Invalid username or token',
      last_push_at: null, last_push_status: null, last_push_error: null,
    });
    render(<Harness repo="work" agentBranch="agent/host-1" readOnly={false} onConnect={() => {}} onChanged={() => {}} />);
    const line = await screen.findByTestId('sync-line');
    expect(line).toHaveTextContent(/sync failed/);
    expect(line).toHaveTextContent(/Invalid username or token/);
  });

  it('renders a push failure on its own line (previously had no push line)', async () => {
    (api.getOrigin as unknown as Fn).mockResolvedValueOnce({
      name: 'origin', url: 'https://github.com/knomit/knomit-kb.git', branch: 'main', auth_method: 'token',
      last_sync_at: '2026-06-11T10:00:00Z', last_status: 'ok', last_error: null,
      last_push_at: null, last_push_status: 'error', last_push_error: 'Permission to knomit/knomit-kb.git denied to knomit.',
    });
    render(<Harness repo="work" agentBranch="agent/host-1" readOnly={false} onConnect={() => {}} onChanged={() => {}} />);
    const line = await screen.findByTestId('push-line');
    expect(line).toHaveTextContent(/push failed/);
    expect(line).toHaveTextContent(/Permission to knomit\/knomit-kb\.git denied/);
  });

  it('warns when the upstream branch equals this machine\'s agent branch', async () => {
    (api.getOrigin as unknown as Fn).mockResolvedValueOnce({
      name: 'origin', url: 'https://github.com/knomit/knomit-kb.git', branch: 'agent/host-1', auth_method: 'token',
      last_sync_at: null, last_status: null, last_error: null,
    });
    render(<Harness repo="work" agentBranch="agent/host-1" readOnly={false} onConnect={() => {}} onChanged={() => {}} />);

    await screen.findByTestId('upstream-warning');
    expect(screen.getByTestId('upstream-warning-icon')).toBeInTheDocument();
    expect(screen.getByText(/not pulled/)).toBeInTheDocument();
  });

  it('does NOT warn when the upstream is a real consensus branch', async () => {
    (api.getOrigin as unknown as Fn).mockResolvedValueOnce({
      name: 'origin', url: 'https://github.com/knomit/knomit-kb.git', branch: 'main', auth_method: 'token',
      last_sync_at: null, last_status: null, last_error: null,
    });
    render(<Harness repo="work" agentBranch="agent/host-1" readOnly={false} onConnect={() => {}} onChanged={() => {}} />);

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
    render(<Harness repo="work" agentBranch="agent/host-1" readOnly={false} onConnect={() => {}} onChanged={onChanged} />);

    fireEvent.click(await screen.findByTestId('upstream-change'));
    const input = await screen.findByTestId('upstream-input') as HTMLInputElement;
    expect(input.value).toBe('main'); // defaults to main
    fireEvent.click(screen.getByTestId('upstream-save'));

    await waitFor(() => expect(api.setOriginUpstream).toHaveBeenCalledWith('work', 'main'));
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
  });
});
