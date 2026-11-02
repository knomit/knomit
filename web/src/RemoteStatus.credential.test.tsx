import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor, cleanup } from '@testing-library/react';
import { RemoteCard } from './RemoteStatus';
import { useRemote } from './useRemote';
import type { OriginResponse, SecretView } from './api';

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>();
  return {
    ...actual,
    api: {
      getOrigin: vi.fn(), listBranchNames: vi.fn(), setOriginUpstream: vi.fn(),
      setOriginCredential: vi.fn(), listSecrets: vi.fn(), createSecret: vi.fn(),
    },
  };
});

const URL_ = 'https://github.com/knomit/kb.git';
const base: OriginResponse = {
  name: 'origin', url: URL_, branch: 'main', interval: 300, auth_method: 'token',
  last_sync_at: '2026-06-11T10:00:00Z', last_status: 'ok', last_error: null,
  push_interval: 300, last_push_at: null, last_push_status: null, last_push_error: null,
};
const linkedOrigin = (status: OriginResponse['secret_status'] = 'ok'): OriginResponse => ({
  ...base, secret_name: 'gh-pat', secret_status: status,
});
const secret = (over: Partial<SecretView>): SecretView => ({
  id: 's1', name: 'gh-pat', type: 'token', hosts: ['github.com'], expires_at: null, status: 'none',
  created_at: '', updated_at: '', linked_repos: 0, ...over,
});

function Harness({ onChanged = () => {} }: { onChanged?: () => void }) {
  const state = useRemote('kb');
  return <RemoteCard repo="kb" agentBranch="agent/h" readOnly={false} state={state}
    onConnect={() => {}} onDisconnect={() => {}} onChanged={onChanged} />;
}

async function mocked() {
  const { api } = await import('./api');
  return vi.mocked(api);
}

beforeEach(async () => {
  const api = await mocked();
  api.setOriginCredential.mockResolvedValue(undefined);
  api.listBranchNames.mockResolvedValue([]);
  api.listSecrets.mockResolvedValue([]);
});
afterEach(() => { cleanup(); vi.clearAllMocks(); });

describe('RemoteCard credential row', () => {
  it('replaces a pasted token through PUT /origin/credential, without secret_id', async () => {
    const api = await mocked();
    api.getOrigin.mockResolvedValue(base);
    render(<Harness />);
    expect(await screen.findByTestId('credential-kind')).toHaveTextContent('Pasted credential');
    fireEvent.click(screen.getByTestId('credential-replace'));
    fireEvent.change(screen.getByTestId('credential-pasted-token'), { target: { value: 'NEW-TOKEN' } });
    fireEvent.click(screen.getByTestId('credential-replace-save'));
    await waitFor(() => expect(api.setOriginCredential).toHaveBeenCalledWith('kb', { auth_method: 'token', token: 'NEW-TOKEN' }));
    expect(api.setOriginCredential.mock.calls[0][1]).not.toHaveProperty('secret_id');
  });

  it('links a stored secret with {secret_id}', async () => {
    const api = await mocked();
    api.getOrigin.mockResolvedValue(base);
    api.listSecrets.mockResolvedValue([secret({ id: 's1' })]);
    render(<Harness />);
    fireEvent.click(await screen.findByTestId('credential-link'));
    const select = await screen.findByTestId('credential-picker-select');
    expect(screen.getByTestId('credential-link-save')).toBeDisabled();
    fireEvent.change(select, { target: { value: 's1' } });
    fireEvent.click(screen.getByTestId('credential-link-save'));
    await waitFor(() => expect(api.setOriginCredential).toHaveBeenCalledWith('kb', { secret_id: 's1' }));
  });

  it('shows the linked secret, and Unlink cannot be confirmed until a replacement is chosen', async () => {
    const api = await mocked();
    api.getOrigin.mockResolvedValue(linkedOrigin());
    render(<Harness />);
    expect(await screen.findByTestId('credential-secret-name')).toHaveTextContent('gh-pat');
    expect(screen.queryByTestId('credential-replace')).toBeNull();
    fireEvent.click(screen.getByTestId('credential-unlink'));
    expect(screen.getByTestId('unlink-choice-pasted')).toHaveAttribute('aria-checked', 'false');
    expect(screen.getByTestId('unlink-choice-none')).toHaveAttribute('aria-checked', 'false');
    expect(screen.getByTestId('credential-unlink-confirm')).toBeDisabled();
    // Pasted, but nothing typed yet: still disabled.
    fireEvent.click(screen.getByTestId('unlink-choice-pasted'));
    expect(screen.getByTestId('credential-unlink-confirm')).toBeDisabled();
  });

  it('unlinks with a pasted credential: {secret_id: null} plus the credential', async () => {
    const api = await mocked();
    api.getOrigin.mockResolvedValue(linkedOrigin());
    render(<Harness />);
    fireEvent.click(await screen.findByTestId('credential-unlink'));
    fireEvent.click(screen.getByTestId('unlink-choice-pasted'));
    fireEvent.change(screen.getByTestId('unlink-pasted-method'), { target: { value: 'basic' } });
    fireEvent.change(screen.getByTestId('unlink-pasted-user'), { target: { value: 'me' } });
    fireEvent.change(screen.getByTestId('unlink-pasted-password'), { target: { value: 'pw' } });
    fireEvent.click(screen.getByTestId('credential-unlink-confirm'));
    await waitFor(() => expect(api.setOriginCredential).toHaveBeenCalledWith('kb', { secret_id: null, auth_method: 'basic', user: 'me', password: 'pw' }));
  });

  it('unlinks to no credential with {secret_id: null} alone', async () => {
    const api = await mocked();
    api.getOrigin.mockResolvedValue(linkedOrigin());
    render(<Harness />);
    fireEvent.click(await screen.findByTestId('credential-unlink'));
    fireEvent.click(screen.getByTestId('unlink-choice-none'));
    fireEvent.click(screen.getByTestId('credential-unlink-confirm'));
    await waitFor(() => expect(api.setOriginCredential).toHaveBeenCalledWith('kb', { secret_id: null }));
  });

  it('an expired linked secret shows the chip and the warning', async () => {
    const api = await mocked();
    api.getOrigin.mockResolvedValue(linkedOrigin('expired'));
    render(<Harness />);
    expect(await screen.findByTestId('credential-chip')).toHaveTextContent('expired');
    expect(screen.getByTestId('credential-warning')).toHaveTextContent(/has expired/);
  });

  it('shows the server refusal and keeps the form open', async () => {
    const api = await mocked();
    api.getOrigin.mockResolvedValue(base);
    api.listSecrets.mockResolvedValue([secret({ id: 's1' })]);
    api.setOriginCredential.mockRejectedValue(new Error('permission denied: admin required'));
    render(<Harness />);
    fireEvent.click(await screen.findByTestId('credential-link'));
    fireEvent.change(await screen.findByTestId('credential-picker-select'), { target: { value: 's1' } });
    fireEvent.click(screen.getByTestId('credential-link-save'));
    expect(await screen.findByTestId('credential-error')).toHaveTextContent('admin required');
    expect(screen.getByTestId('credential-link-box')).toBeInTheDocument();
  });
});

describe('RemoteCard credential row: link flag and form state', () => {
  it('a linked repo whose secret name cannot be read still shows as linked, named (unreadable), with no token replace', async () => {
    const api = await mocked();
    api.getOrigin.mockResolvedValue({ ...base, secret_linked: true, secret_status: undefined });
    render(<Harness />);
    expect(await screen.findByTestId('credential-secret-name')).toHaveTextContent('(unreadable)');
    expect(screen.queryByTestId('credential-replace')).toBeNull();
    expect(screen.getByTestId('credential-unlink')).toBeInTheDocument();
  });

  it('offers "Add token…" on a repo with no credential, "Replace token…" on a pasted one', async () => {
    const api = await mocked();
    api.getOrigin.mockResolvedValue({ ...base, auth_method: 'none' });
    render(<Harness />);
    expect(await screen.findByTestId('credential-replace')).toHaveTextContent('Add token…');
    cleanup();
    api.getOrigin.mockResolvedValue(base);
    render(<Harness />);
    expect(await screen.findByTestId('credential-replace')).toHaveTextContent('Replace token…');
  });

  it('Cancel forgets a typed token', async () => {
    const api = await mocked();
    api.getOrigin.mockResolvedValue(base);
    render(<Harness />);
    fireEvent.click(await screen.findByTestId('credential-replace'));
    fireEvent.change(screen.getByTestId('credential-pasted-token'), { target: { value: 'TYPED-SECRET' } });
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    fireEvent.click(screen.getByTestId('credential-replace'));
    expect(screen.getByTestId('credential-pasted-token')).toHaveValue('');
    expect(document.body.innerHTML).not.toContain('TYPED-SECRET');
  });
});
