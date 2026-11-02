import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup, waitFor, within } from '@testing-library/react';
import { SecretsTab } from './SecretsTab';
import { SecretRefusal, SECRETS_UNAVAILABLE, type SecretView } from './api';

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>();
  return {
    ...actual,
    api: {
      listSecrets: vi.fn(), getSecret: vi.fn(), createSecret: vi.fn(),
      patchSecret: vi.fn(), deleteSecret: vi.fn(),
    },
  };
});

const secret = (over: Partial<SecretView> = {}): SecretView => ({
  id: 's1', name: 'gh-pat', type: 'token', hosts: ['github.com'], expires_at: null, status: 'none',
  created_at: '2026-11-01T00:00:00Z', updated_at: '2026-11-01T00:00:00Z', linked_repos: 0, ...over,
});

async function mocked() {
  const { api } = await import('./api');
  return vi.mocked(api);
}

beforeEach(async () => {
  const api = await mocked();
  api.listSecrets.mockResolvedValue([]);
  api.getSecret.mockImplementation(async (id: string) => ({ ...secret({ id }), repos: [] }));
});
afterEach(() => { cleanup(); vi.clearAllMocks(); });

const type = (testid: string, value: string) => fireEvent.change(screen.getByTestId(testid), { target: { value } });

describe('SecretsTab', () => {
  it('lists every secret with its status chip and linked count', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([
      secret({ id: 'a', name: 'old', status: 'expired', expires_at: '2026-10-01T00:00:00Z', linked_repos: 2 }),
      secret({ id: 'b', name: 'soon', status: 'expiring', expires_at: '2026-11-10T00:00:00Z' }),
      secret({ id: 'c', name: 'fine', status: 'ok', expires_at: '2027-01-01T00:00:00Z' }),
    ]);
    render(<SecretsTab />);
    expect(await screen.findByTestId('secret-status-old')).toHaveTextContent('expired');
    expect(screen.getByTestId('secret-status-soon')).toHaveTextContent('expiring');
    expect(screen.queryByTestId('secret-status-fine')).toBeNull();
    expect(screen.getByTestId('secret-linked-old')).toHaveTextContent('2');
    expect(within(screen.getByTestId('secret-row-old')).getByText('2026-10-01')).toBeInTheDocument();
  });

  it('creates a secret with the right body and does not keep the value in the DOM', async () => {
    const api = await mocked();
    api.createSecret.mockResolvedValue(secret({ id: 'new', name: 'gh' }));
    render(<SecretsTab />);
    fireEvent.click(await screen.findByTestId('secret-add'));
    type('secret-name', 'gh');
    type('secret-hosts', 'github.com, gitlab.com');
    type('secret-expiry', '2027-02-01');
    type('secret-value', 'TOKEN-123');
    fireEvent.click(screen.getByTestId('secret-form-submit'));
    await waitFor(() => expect(api.createSecret).toHaveBeenCalledWith({
      name: 'gh', type: 'token', hosts: ['github.com', 'gitlab.com'], expires_at: '2027-02-01T00:00:00Z', value: 'TOKEN-123',
    }));
    await waitFor(() => expect(screen.queryByTestId('secret-form-create')).toBeNull());
    expect(document.body.innerHTML).not.toContain('TOKEN-123');
  });

  it('asks for a username and password for that type, and a key for SSH', async () => {
    render(<SecretsTab />);
    fireEvent.click(await screen.findByTestId('secret-add'));
    fireEvent.change(screen.getByTestId('secret-type'), { target: { value: 'basic' } });
    expect(screen.getByTestId('secret-username')).toBeInTheDocument();
    expect(screen.getByTestId('secret-password')).toHaveAttribute('type', 'password');
    fireEvent.change(screen.getByTestId('secret-type'), { target: { value: 'ssh_key' } });
    expect(screen.getByTestId('secret-key').tagName).toBe('TEXTAREA');
    expect(screen.getByTestId('secret-passphrase')).toHaveAttribute('type', 'password');
  });

  it('disables Delete while repos are linked, and says why', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([secret({ linked_repos: 1 })]);
    api.getSecret.mockResolvedValue({ ...secret({ linked_repos: 1 }), repos: [{ name: 'core-kb', uid: 'u', state: 'active' }] });
    render(<SecretsTab />);
    fireEvent.click(await screen.findByTestId('secret-edit-gh-pat'));
    expect(screen.getByTestId('secret-delete')).toBeDisabled();
    await waitFor(() => expect(screen.getByTestId('secret-delete-why')).toHaveTextContent(/core-kb/));
  });

  it('deletes an unused secret after a confirm', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([secret()]);
    api.deleteSecret.mockResolvedValue(undefined);
    render(<SecretsTab />);
    fireEvent.click(await screen.findByTestId('secret-edit-gh-pat'));
    fireEvent.click(screen.getByTestId('secret-delete'));
    fireEvent.click(screen.getByTestId('secret-delete-confirm'));
    await waitFor(() => expect(api.deleteSecret).toHaveBeenCalledWith('s1'));
  });

  it('replaces the value with PATCH and clears the input', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([secret()]);
    api.patchSecret.mockResolvedValue(secret());
    render(<SecretsTab />);
    fireEvent.click(await screen.findByTestId('secret-edit-gh-pat'));
    fireEvent.click(screen.getByTestId('secret-replace'));
    expect(screen.getByTestId('secret-value')).toHaveValue('');
    type('secret-value', 'NEW-VALUE');
    fireEvent.click(screen.getByTestId('secret-form-submit'));
    await waitFor(() => expect(api.patchSecret).toHaveBeenCalledWith('s1', { value: 'NEW-VALUE' }));
    await waitFor(() => expect(screen.queryByTestId('secret-form-replace')).toBeNull());
    expect(document.body.innerHTML).not.toContain('NEW-VALUE');
  });

  it('sends the whole new hosts list, and on a host-in-use refusal names the repos and keeps the old list', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([secret({ hosts: ['github.com', 'old.example'] })]);
    api.patchSecret.mockRejectedValue(new SecretRefusal('Host(s) old.example are used by: core-kb. Unlink those repositories or change their URL first.', 409, ['core-kb'], ['old.example']));
    render(<SecretsTab />);
    fireEvent.click(await screen.findByTestId('secret-edit-gh-pat'));
    fireEvent.click(screen.getByTestId('secret-host-remove-old.example'));
    type('secret-host-new', 'gitlab.com');
    fireEvent.click(screen.getByTestId('secret-host-add'));
    fireEvent.click(screen.getByTestId('secret-hosts-save'));
    await waitFor(() => expect(api.patchSecret).toHaveBeenCalledWith('s1', { hosts: ['github.com', 'gitlab.com'] }));
    expect(await screen.findByTestId('secret-editor-error')).toHaveTextContent('core-kb');
    expect(screen.getByTestId('secret-host-old.example')).toBeInTheDocument();
    expect(screen.queryByTestId('secret-host-gitlab.com')).toBeNull();
  });

  it('renames and changes the expiry', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([secret()]);
    api.patchSecret.mockResolvedValue(secret());
    render(<SecretsTab />);
    fireEvent.click(await screen.findByTestId('secret-edit-gh-pat'));
    type('secret-rename', 'github');
    fireEvent.click(screen.getByTestId('secret-rename-save'));
    await waitFor(() => expect(api.patchSecret).toHaveBeenCalledWith('s1', { name: 'github' }));
    type('secret-expiry-edit', '2027-03-01');
    fireEvent.click(screen.getByTestId('secret-expiry-save'));
    await waitFor(() => expect(api.patchSecret).toHaveBeenCalledWith('s1', { expires_at: '2027-03-01T00:00:00Z' }));
  });

  it('says the instance key cannot be read on a 503', async () => {
    const api = await mocked();
    api.listSecrets.mockRejectedValue(new SecretRefusal(SECRETS_UNAVAILABLE, 503));
    render(<SecretsTab />);
    expect(await screen.findByTestId('secrets-error')).toHaveTextContent('Secret storage is unavailable: the instance key cannot be read.');
  });
});
