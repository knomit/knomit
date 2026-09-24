import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent, within } from '@testing-library/react';
import { ManageOAuth, defaultScopes } from './ManageOAuth';
import { api, type OAuthPending } from './api';

vi.mock('./api', async importOriginal => ({
  ...(await importOriginal<typeof import('./api')>()),
  api: {
    listOAuthPending: vi.fn(),
    approveOAuthPending: vi.fn(),
    denyOAuthPending: vi.fn(),
  },
}));

const now = new Date('2026-09-24T12:00:00Z');

// What Claude Code parks, as seen live in the phase 3b Q4 run: it asks for
// every scope the server advertises.
const claude = (over: Partial<OAuthPending> = {}): OAuthPending => ({
  id: 'YsKa_Y0SRYJwz2giIdaCrw2A6XpDaX2V0M8JqSFZN3M',
  client_id: 'https://claude.ai/oauth/claude-code-client-metadata',
  client_name: 'Claude Code',
  redirect_uri: 'http://localhost:52346/callback',
  scopes: ['read', 'write', 'push:own', 'merge:main', 'operator'],
  resource: 'http://localhost:19491/api/v1/repos/kb/mcp',
  remote_addr: '127.0.0.1:59219',
  user_agent: 'Mozilla/5.0 (Macintosh) Safari/605',
  created_at: '2026-09-24T11:58:00Z',
  expires_at: '2026-09-24T12:08:00Z',
  ...over,
});

beforeEach(() => {
  vi.useFakeTimers({ now, shouldAdvanceTime: true });
  vi.mocked(api.listOAuthPending).mockResolvedValue([claude()]);
  vi.mocked(api.approveOAuthPending).mockResolvedValue({ subject: 'laptop', grantsUnchanged: false });
  vi.mocked(api.denyOAuthPending).mockResolvedValue(undefined);
});
afterEach(() => {
  vi.useRealTimers();
  vi.clearAllMocks();
});

async function row() {
  await waitFor(() => expect(screen.getAllByTestId('oauth-row')).toHaveLength(1));
  return screen.getAllByTestId('oauth-row')[0];
}

describe('defaultScopes (D13: requested ∩ {read, write}, else read)', () => {
  it('narrows Claude Code\'s everything-request to read and write', () => {
    expect(defaultScopes(['read', 'write', 'push:own', 'merge:main', 'operator'])).toEqual(['read', 'write']);
  });
  it('falls back to read when nothing requested is read or write', () => {
    expect(defaultScopes(['operator'])).toEqual(['read']);
    expect(defaultScopes([])).toEqual(['read']);
  });
  it('keeps read alone', () => {
    expect(defaultScopes(['read'])).toEqual(['read']);
  });
});

describe('ManageOAuth', () => {
  it('shows what the operator judges by: client, redirect host first, resource, requested scopes, the opening browser, time left', async () => {
    render(<ManageOAuth />);
    const r = await row();
    expect(within(r).getByTestId('oauth-redirect-host')).toHaveTextContent('localhost:52346');
    expect(r).toHaveTextContent('Claude Code');
    expect(r).toHaveTextContent('https://claude.ai/oauth/claude-code-client-metadata');
    expect(r).toHaveTextContent('http://localhost:52346/callback');
    expect(r).toHaveTextContent('http://localhost:19491/api/v1/repos/kb/mcp');
    expect(within(r).getByTestId('oauth-requested')).toHaveTextContent('read write push:own merge:main operator');
    expect(r).toHaveTextContent('127.0.0.1:59219');
    expect(r).toHaveTextContent('Safari/605');
    expect(r).toHaveTextContent(/browser that opened/i);
    expect(r).toHaveTextContent('8 min left');
  });

  it('renders requester-supplied text as text, never markup', async () => {
    const hostile = '<img src=x onerror="window.pwned=1">‮edoc';
    vi.mocked(api.listOAuthPending).mockResolvedValue([claude({ client_name: hostile, redirect_uri: 'https://evil.example/cb' })]);
    const { container } = render(<ManageOAuth />);
    const r = await row();
    expect(container.querySelector('img')).toBeNull();
    expect(r.textContent).toContain(hostile);
    expect(within(r).getByTestId('oauth-redirect-host')).toHaveTextContent('evil.example');
  });

  it('pre-checks the D13 default, not what the client asked for', async () => {
    render(<ManageOAuth />);
    const r = await row();
    const checked = within(r).getAllByRole('checkbox').filter(c => (c as HTMLInputElement).checked).map(c => c.getAttribute('value'));
    expect(checked).toEqual(['read', 'write']);
  });

  it('approves with the subject and the checked scopes, then reloads', async () => {
    render(<ManageOAuth />);
    const r = await row();
    const approve = within(r).getByRole('button', { name: /approve/i });
    expect(approve).toBeDisabled(); // no subject yet
    fireEvent.change(within(r).getByLabelText(/subject/i), { target: { value: 'laptop' } });
    fireEvent.click(within(r).getByRole('checkbox', { name: 'write' })); // uncheck write
    fireEvent.click(approve);
    await waitFor(() => expect(api.approveOAuthPending).toHaveBeenCalledWith(claude().id, 'laptop', ['read']));
    await waitFor(() => expect(api.listOAuthPending).toHaveBeenCalledTimes(2));
  });

  it('after a re-approval, says the grants were left alone and names how to widen them (F19 3c R5)', async () => {
    vi.mocked(api.approveOAuthPending).mockResolvedValue({ subject: 'laptop', grantsUnchanged: true });
    vi.mocked(api.listOAuthPending).mockResolvedValueOnce([claude()]).mockResolvedValue([]);
    render(<ManageOAuth />);
    const r = await row();
    fireEvent.change(within(r).getByLabelText(/subject/i), { target: { value: 'laptop' } });
    fireEvent.click(within(r).getByRole('button', { name: /approve/i }));
    const note = await screen.findByTestId('oauth-grants-unchanged');
    expect(note).toHaveTextContent('host:laptop@token');
    expect(note).toHaveTextContent('knomit grants add host:laptop@token <perm>');
  });

  it('says nothing about grants after a first approval', async () => {
    vi.mocked(api.listOAuthPending).mockResolvedValueOnce([claude()]).mockResolvedValue([]);
    render(<ManageOAuth />);
    const r = await row();
    fireEvent.change(within(r).getByLabelText(/subject/i), { target: { value: 'laptop' } });
    fireEvent.click(within(r).getByRole('button', { name: /approve/i }));
    await waitFor(() => expect(api.listOAuthPending).toHaveBeenCalledTimes(2));
    expect(screen.queryByTestId('oauth-grants-unchanged')).toBeNull();
  });

  // 3b review N1: React escapes markup, but a U+202E in the user agent still
  // reorders the text around it on screen. The user agent is isolated in a
  // <bdi> so its direction cannot leak into the address or the time left.
  it('isolates the user agent so a bidi override cannot reorder its neighbours (3b N1)', async () => {
    vi.mocked(api.listOAuthPending).mockResolvedValue([claude({ user_agent: 'agent\u202eevil' })]);
    render(<ManageOAuth />);
    const r = await row();
    const ua = within(r).getByTestId('oauth-user-agent');
    expect(ua.tagName).toBe('BDI');
    expect(ua.textContent).toBe('agent\u202eevil');
  });

  it('cannot approve with no scope checked', async () => {
    render(<ManageOAuth />);
    const r = await row();
    fireEvent.change(within(r).getByLabelText(/subject/i), { target: { value: 'laptop' } });
    fireEvent.click(within(r).getByRole('checkbox', { name: 'read' }));
    fireEvent.click(within(r).getByRole('checkbox', { name: 'write' }));
    expect(within(r).getByRole('button', { name: /approve/i })).toBeDisabled();
  });

  it('denies', async () => {
    render(<ManageOAuth />);
    const r = await row();
    fireEvent.click(within(r).getByRole('button', { name: /deny/i }));
    await waitFor(() => expect(api.denyOAuthPending).toHaveBeenCalledWith(claude().id));
  });

  it('shows the server\'s refusal of an approval', async () => {
    vi.mocked(api.approveOAuthPending).mockRejectedValue(new Error('oauth: subject must be 1-128 characters'));
    render(<ManageOAuth />);
    const r = await row();
    fireEvent.change(within(r).getByLabelText(/subject/i), { target: { value: 'bad subject' } });
    fireEvent.click(within(r).getByRole('button', { name: /approve/i }));
    await waitFor(() => expect(within(r).getByRole('alert')).toHaveTextContent('subject must be'));
  });

  it('says so when nothing is waiting', async () => {
    vi.mocked(api.listOAuthPending).mockResolvedValue([]);
    render(<ManageOAuth />);
    expect(await screen.findByText(/no authorization requests are waiting/i)).toBeInTheDocument();
  });

  it('shows why listing was refused', async () => {
    vi.mocked(api.listOAuthPending).mockRejectedValue(new Error('the anonymous loopback principal does not hold admin'));
    render(<ManageOAuth />);
    expect(await screen.findByRole('alert')).toHaveTextContent('does not hold admin');
  });

  it('reports the count so the tab can badge it', async () => {
    const onCount = vi.fn();
    render(<ManageOAuth onCount={onCount} />);
    await waitFor(() => expect(onCount).toHaveBeenCalledWith(1));
  });
});
