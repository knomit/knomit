import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup, waitFor, within } from '@testing-library/react';
import { AccessPage } from './AccessPage';
import { SecretRefusal, SECRETS_UNAVAILABLE, type SecretView } from './api';
import { expiryBadge } from './secrets';

// Access → Secrets, driven through the page that hosts it: the list column
// names each secret, the detail column shows and changes the one picked.

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
const renderSecrets = () => render(<AccessPage tab="secrets" onTab={vi.fn()} oauthAvailable={false} authCount={null} onAuthCount={vi.fn()} />);
const open = async (name: string) => {
  fireEvent.click(await screen.findByTestId(`secret-row-${name}`));
  return screen.findByTestId('secret-editor');
};

describe('expiryBadge', () => {
  it('flags only what needs attention in the list, and always says something in the detail', () => {
    const now = Date.now();
    const soon = new Date(now + 10 * 86_400_000 + 3_600_000).toISOString();
    expect(expiryBadge({ status: 'expired', expires_at: '2020-01-01T00:00:00Z' })).toMatchObject({ tone: 'bad', text: 'expired' });
    expect(expiryBadge({ status: 'expiring', expires_at: soon })).toMatchObject({ tone: 'warn', text: '10 days' });
    expect(expiryBadge({ status: 'expiring', expires_at: soon }, true)).toMatchObject({ tone: 'warn', text: 'expires in 10 days' });
    expect(expiryBadge({ status: 'ok', expires_at: '2099-01-01T00:00:00Z' })).toBeNull();
    expect(expiryBadge({ status: 'none', expires_at: null })).toBeNull();
    expect(expiryBadge({ status: 'none', expires_at: null }, true)).toMatchObject({ tone: 'ok', text: 'active', title: 'No expiry date is set.' });
  });
});

describe('Access → Secrets', () => {
  it('lists every secret by name, with its hosts, its use, and a badge only when its expiry needs attention', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([
      secret({ id: 'a', name: 'old', status: 'expired', expires_at: '2026-10-01T00:00:00Z', linked_repos: 2 }),
      secret({ id: 'b', name: 'soon', status: 'expiring', expires_at: new Date(Date.now() + 5 * 86_400_000 + 3_600_000).toISOString() }),
      secret({ id: 'c', name: 'fine', status: 'ok', expires_at: '2099-01-01T00:00:00Z', hosts: ['gitlab.com', 'x.example'] }),
    ]);
    renderSecrets();
    expect(await screen.findByTestId('secret-status-old')).toHaveTextContent('expired');
    expect(screen.getByTestId('secret-status-soon')).toHaveTextContent('5 days');
    expect(screen.queryByTestId('secret-status-fine')).toBeNull();
    expect(screen.getByTestId('secret-row-old')).toHaveTextContent('2 repos');
    expect(screen.getByTestId('secret-row-soon')).toHaveTextContent('unused');
    expect(screen.getByTestId('secret-row-fine')).toHaveTextContent('gitlab.com, x.example');
    // Sorted by name.
    const rows = within(screen.getByTestId('secrets-list')).getAllByRole('button').map(b => b.getAttribute('data-testid'));
    expect(rows).toEqual(['secret-row-fine', 'secret-row-old', 'secret-row-soon']);
  });

  it('finds by name or host', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([
      secret({ id: 'a', name: 'gh', hosts: ['github.com'] }),
      secret({ id: 'b', name: 'lab', hosts: ['gitlab.com'] }),
    ]);
    renderSecrets();
    await screen.findByTestId('secret-row-gh');
    fireEvent.change(screen.getByLabelText('Find a secret or host'), { target: { value: 'gitlab' } });
    expect(screen.queryByTestId('secret-row-gh')).toBeNull();
    expect(screen.getByTestId('secret-row-lab')).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText('Find a secret or host'), { target: { value: 'nothing' } });
    expect(screen.getByTestId('secrets-list')).toHaveTextContent('Nothing matches “nothing”');
  });

  it('says what a secret is for when there are none, and points at the + rather than offering a second button', async () => {
    renderSecrets();
    const empty = await screen.findByTestId('secrets-empty');
    expect(empty).toHaveTextContent('No stored secrets yet');
    expect(empty).toHaveTextContent('Add one with the + button above.');
    expect(within(empty).queryByRole('button')).toBeNull();
    expect(within(screen.getByTestId('secrets-overview')).queryByRole('button', { name: /add/i })).toBeNull();
    // The + is the one way to add: a named, focusable button, with nothing else in the page called that.
    const add = screen.getByRole('button', { name: 'Add a secret' });
    expect(add).toBe(screen.getByTestId('secret-add'));
    expect(add).toHaveAttribute('title', 'Add a secret');
    add.focus();
    expect(add).toHaveFocus();
    fireEvent.click(add);
    expect(screen.getByTestId('secret-create')).toBeInTheDocument();
  });

  it('offers no add button on the overview, only the + in the list header', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([secret({ id: 'a', name: 'gh' })]);
    renderSecrets();
    const o = await screen.findByTestId('secrets-overview');
    await screen.findByTestId('secret-row-gh');
    expect(within(o).queryByRole('button', { name: /add/i })).toBeNull();
    expect(screen.getAllByRole('button', { name: /add a secret/i })).toEqual([screen.getByTestId('secret-add')]);
  });

  it('counts what needs attention on the overview, and opens one from there', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([
      secret({ id: 'a', name: 'old', status: 'expired', expires_at: '2026-10-01T00:00:00Z', linked_repos: 1 }),
      secret({ id: 'b', name: 'fine' }),
    ]);
    renderSecrets();
    const o = await screen.findByTestId('secrets-overview');
    await waitFor(() => expect(o).toHaveTextContent('Needs attention'));
    fireEvent.click(within(o).getByRole('button', { name: /old/ }));
    expect(await screen.findByTestId('secret-editor')).toHaveTextContent('old');
  });

  it('creates a secret with the right body, lands on it, and does not keep the value in the DOM', async () => {
    const api = await mocked();
    api.createSecret.mockResolvedValue(secret({ id: 'new', name: 'gh' }));
    renderSecrets();
    fireEvent.click(await screen.findByTestId('secret-add'));
    expect(screen.getByTestId('secret-row-new')).toHaveTextContent('New secret');
    type('secret-name', 'gh');
    type('secret-hosts', 'github.com, gitlab.com');
    type('secret-expiry', '2027-02-01');
    type('secret-value', 'TOKEN-123');
    api.listSecrets.mockResolvedValue([secret({ id: 'new', name: 'gh' })]);
    fireEvent.click(screen.getByTestId('secret-form-submit'));
    await waitFor(() => expect(api.createSecret).toHaveBeenCalledWith({
      name: 'gh', type: 'token', hosts: ['github.com', 'gitlab.com'], expires_at: '2027-02-01T00:00:00Z', value: 'TOKEN-123',
    }));
    await waitFor(() => expect(screen.queryByTestId('secret-form-create')).toBeNull());
    expect(await screen.findByTestId('secret-editor')).toHaveTextContent('gh');
    expect(screen.queryByTestId('secret-row-new')).toBeNull();
    expect(document.body.innerHTML).not.toContain('TOKEN-123');
  });

  it('asks for a username and password for that type, and a key for SSH', async () => {
    renderSecrets();
    fireEvent.click(await screen.findByTestId('secret-add'));
    fireEvent.change(screen.getByTestId('secret-type'), { target: { value: 'basic' } });
    expect(screen.getByTestId('secret-username')).toBeInTheDocument();
    expect(screen.getByTestId('secret-password')).toHaveAttribute('type', 'password');
    fireEvent.change(screen.getByTestId('secret-type'), { target: { value: 'ssh_key' } });
    expect(screen.getByTestId('secret-key').tagName).toBe('TEXTAREA');
    expect(screen.getByTestId('secret-passphrase')).toHaveAttribute('type', 'password');
  });

  it('never shows a value, and shows an SSH key\'s fingerprint', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([secret({ type: 'ssh_key', fingerprint: 'SHA256:abc' })]);
    const ed = await (renderSecrets(), open('gh-pat'));
    expect(within(ed).getByLabelText('hidden value')).toBeInTheDocument();
    expect(within(ed).getByTestId('secret-fingerprint')).toHaveTextContent('SHA256:abc');
  });

  it('disables Delete while repos are linked, and names them', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([secret({ linked_repos: 1 })]);
    api.getSecret.mockResolvedValue({ ...secret({ linked_repos: 1 }), repos: [{ name: 'core-kb', uid: 'u', state: 'active' }] });
    renderSecrets();
    await open('gh-pat');
    expect(screen.getByTestId('secret-delete')).toBeDisabled();
    await waitFor(() => expect(screen.getByTestId('secret-delete-why')).toHaveTextContent(/core-kb/));
    expect(screen.getByTestId('secret-linked-repos')).toHaveTextContent('core-kb');
  });

  it('deletes an unused secret after a confirm, and goes back to the overview', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([secret()]);
    api.deleteSecret.mockResolvedValue(undefined);
    renderSecrets();
    await open('gh-pat');
    fireEvent.click(screen.getByTestId('secret-delete'));
    fireEvent.click(screen.getByTestId('secret-delete-confirm'));
    await waitFor(() => expect(api.deleteSecret).toHaveBeenCalledWith('s1'));
    expect(await screen.findByTestId('secrets-overview')).toBeInTheDocument();
  });

  it('replaces the value with PATCH and clears the input', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([secret()]);
    api.patchSecret.mockResolvedValue(secret());
    renderSecrets();
    await open('gh-pat');
    fireEvent.click(screen.getByTestId('secret-replace'));
    expect(screen.getByTestId('secret-value')).toHaveValue('');
    type('secret-value', 'NEW-VALUE');
    fireEvent.click(screen.getByTestId('secret-form-submit'));
    await waitFor(() => expect(api.patchSecret).toHaveBeenCalledWith('s1', { value: 'NEW-VALUE' }));
    await waitFor(() => expect(screen.queryByTestId('secret-form-replace')).toBeNull());
    expect(screen.getByTestId('secret-editor-note')).toHaveTextContent('Value replaced');
    expect(document.body.innerHTML).not.toContain('NEW-VALUE');
  });

  it('sends the whole new hosts list, and on a host-in-use refusal names the repos and saves nothing', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([secret({ hosts: ['github.com', 'old.example'] })]);
    api.patchSecret.mockRejectedValue(new SecretRefusal('Host(s) old.example are used by: core-kb. Unlink those repositories or change their URL first.', 409, ['core-kb'], ['old.example']));
    renderSecrets();
    await open('gh-pat');
    fireEvent.click(screen.getByTestId('secret-hosts-edit'));
    fireEvent.click(screen.getByRole('button', { name: 'Remove old.example' }));
    type('secret-host-new', 'gitlab.com');
    fireEvent.click(screen.getByTestId('secret-host-add'));
    fireEvent.click(screen.getByTestId('secret-hosts-save'));
    await waitFor(() => expect(api.patchSecret).toHaveBeenCalledWith('s1', { hosts: ['github.com', 'gitlab.com'] }));
    expect(await screen.findByTestId('secret-editor-error')).toHaveTextContent('core-kb');
    // Cancel: the stored list, as it was.
    fireEvent.click(within(screen.getByTestId('secret-editor')).getAllByRole('button', { name: 'Cancel' })[0]);
    expect(screen.getByTestId('secret-host-old.example')).toBeInTheDocument();
    expect(screen.queryByTestId('secret-host-gitlab.com')).toBeNull();
  });

  it('keeps the last host: a secret needs at least one', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([secret()]);
    renderSecrets();
    await open('gh-pat');
    fireEvent.click(screen.getByTestId('secret-hosts-edit'));
    expect(screen.getByRole('button', { name: 'Remove github.com' })).toBeDisabled();
  });

  it('renames and changes the expiry', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([secret()]);
    api.patchSecret.mockResolvedValue(secret());
    renderSecrets();
    await open('gh-pat');
    fireEvent.click(screen.getByTestId('secret-rename-start'));
    type('secret-rename', 'github');
    fireEvent.click(screen.getByTestId('secret-rename-save'));
    await waitFor(() => expect(api.patchSecret).toHaveBeenCalledWith('s1', { name: 'github' }));
    fireEvent.click(await screen.findByTestId('secret-expiry-edit-start'));
    type('secret-expiry-edit', '2027-03-01');
    fireEvent.click(screen.getByTestId('secret-expiry-save'));
    await waitFor(() => expect(api.patchSecret).toHaveBeenCalledWith('s1', { expires_at: '2027-03-01T00:00:00Z' }));
  });

  it('clears an expiry', async () => {
    const api = await mocked();
    api.listSecrets.mockResolvedValue([secret({ status: 'ok', expires_at: '2099-01-01T00:00:00Z' })]);
    api.patchSecret.mockResolvedValue(secret());
    renderSecrets();
    await open('gh-pat');
    fireEvent.click(screen.getByTestId('secret-expiry-edit-start'));
    fireEvent.click(screen.getByTestId('secret-expiry-clear'));
    await waitFor(() => expect(api.patchSecret).toHaveBeenCalledWith('s1', { expires_at: null }));
  });

  it('says the instance key cannot be read on a 503', async () => {
    const api = await mocked();
    api.listSecrets.mockRejectedValue(new SecretRefusal(SECRETS_UNAVAILABLE, 503));
    renderSecrets();
    expect(await screen.findByTestId('secrets-error')).toHaveTextContent('Secret storage is unavailable: the instance key cannot be read.');
  });
});
