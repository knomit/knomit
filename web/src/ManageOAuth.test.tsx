import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent, within } from '@testing-library/react';
import { AccessPage } from './AccessPage';
import { defaultScopes, redirectHost } from './oauthAccess';
import { api, type OAuthGrant, type OAuthPending } from './api';

// Access → Authorizations, driven through the page that hosts it: the list
// groups the requests WAITING and the logins APPROVED; the detail shows the
// one picked, or the overview with nothing picked.

vi.mock('./api', async importOriginal => ({
  ...(await importOriginal<typeof import('./api')>()),
  api: {
    listSecrets: vi.fn(),
    listOAuthPending: vi.fn(),
    approveOAuthPending: vi.fn(),
    denyOAuthPending: vi.fn(),
    listOAuthGrants: vi.fn(),
    revokeOAuthGrant: vi.fn(),
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

const login = (over: Partial<OAuthGrant> = {}): OAuthGrant => ({
  id: 'fam-1',
  client_id: 'https://claude.ai/oauth/claude-code-client-metadata',
  client_name: 'Claude Code',
  subject: 'laptop',
  scopes: ['read', 'write'],
  resource: 'http://localhost:19491/api/v1/repos/kb/mcp',
  approved_by: 'bridge:uid:501@local',
  created_at: '2026-09-12T12:00:00Z',
  last_used_at: '2026-09-24T11:20:00Z',
  refresh_expires_at: '2026-10-26T12:00:00Z',
  ...over,
});

beforeEach(() => {
  vi.useFakeTimers({ now, shouldAdvanceTime: true });
  vi.mocked(api.listSecrets).mockResolvedValue([]);
  vi.mocked(api.listOAuthPending).mockResolvedValue([claude()]);
  vi.mocked(api.listOAuthGrants).mockResolvedValue([]);
  vi.mocked(api.approveOAuthPending).mockResolvedValue({ subject: 'laptop', grantsUnchanged: false });
  vi.mocked(api.denyOAuthPending).mockResolvedValue(undefined);
  vi.mocked(api.revokeOAuthGrant).mockResolvedValue(undefined);
});
afterEach(() => {
  vi.useRealTimers();
  vi.clearAllMocks();
});

function renderAuth(onAuthCount = vi.fn()) {
  return render(<AccessPage tab="auth" onTab={vi.fn()} oauthAvailable authCount={null} onAuthCount={onAuthCount} />);
}

/** Opens the first waiting request and returns its detail. */
async function request() {
  fireEvent.click((await screen.findAllByTestId('oauth-row-item'))[0]);
  return screen.findByTestId('oauth-row');
}

/** Opens the first approved login and returns its detail. */
async function approved() {
  fireEvent.click((await screen.findAllByTestId('oauth-grant-item'))[0]);
  return screen.findByTestId('oauth-grant');
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

describe('redirectHost', () => {
  it('is the host and port of a URL, and the text itself when it is not one', () => {
    expect(redirectHost('http://localhost:52346/callback')).toBe('localhost:52346');
    expect(redirectHost('not a url')).toBe('not a url');
  });
});

describe('Authorizations — the list', () => {
  it('lists a waiting request by its client, where its code goes, and the time left', async () => {
    renderAuth();
    const row = (await screen.findAllByTestId('oauth-row-item'))[0];
    expect(row).toHaveTextContent('Claude Code');
    expect(row).toHaveTextContent('→ localhost:52346');
    expect(row).toHaveTextContent('8 min left');
    expect(screen.getByTestId('auth-group-waiting')).toHaveTextContent(/Waiting/);
  });

  it('lists the approved logins by client, subject and scopes, falling back to the client id', async () => {
    vi.mocked(api.listOAuthGrants).mockResolvedValue([login(), login({ id: 'fam-2', client_name: '', client_id: 'zed-assistant', subject: 'ci' })]);
    renderAuth();
    const rows = await screen.findAllByTestId('oauth-grant-item');
    expect(rows[0]).toHaveTextContent('Claude Code');
    expect(rows[0]).toHaveTextContent('as laptop · read write');
    expect(rows[1]).toHaveTextContent('zed-assistant');
    expect(screen.getByTestId('auth-group-approved')).toBeInTheDocument();
  });

  it('draws no Approved group when the server lists no logins (no issuer: null)', async () => {
    vi.mocked(api.listOAuthGrants).mockResolvedValue(null);
    renderAuth();
    await screen.findAllByTestId('oauth-row-item');
    expect(screen.queryByTestId('auth-group-approved')).toBeNull();
  });

  it('says what the section is for when nothing waits and nothing is approved', async () => {
    vi.mocked(api.listOAuthPending).mockResolvedValue([]);
    renderAuth();
    expect(await screen.findByTestId('auth-empty')).toHaveTextContent('No clients yet');
  });

  it('says "Nothing waiting" in its group while logins are approved', async () => {
    vi.mocked(api.listOAuthPending).mockResolvedValue([]);
    vi.mocked(api.listOAuthGrants).mockResolvedValue([login()]);
    renderAuth();
    expect(await screen.findByTestId('auth-group-waiting')).toHaveTextContent('Nothing waiting.');
    expect(screen.queryByTestId('auth-empty')).toBeNull();
  });

  it('shows why listing was refused', async () => {
    vi.mocked(api.listOAuthPending).mockRejectedValue(new Error('the anonymous loopback principal does not hold admin'));
    renderAuth();
    expect(await screen.findByRole('alert')).toHaveTextContent('does not hold admin');
  });

  it('reports the count so the section and the server tab can show it', async () => {
    const onCount = vi.fn();
    renderAuth(onCount);
    await waitFor(() => expect(onCount).toHaveBeenCalledWith(1));
  });

  it('finds by client or subject', async () => {
    vi.mocked(api.listOAuthGrants).mockResolvedValue([login()]);
    renderAuth();
    await screen.findAllByTestId('oauth-grant-item');
    fireEvent.change(screen.getByLabelText('Find a client or subject'), { target: { value: 'laptop' } });
    expect(screen.queryByTestId('oauth-row-item')).toBeNull();
    expect(screen.getAllByTestId('oauth-grant-item')).toHaveLength(1);
    expect(screen.getByTestId('auth-group-waiting')).toHaveTextContent('No match.');
  });
});

describe('Authorizations — the overview', () => {
  it('counts what waits and what is approved, and opens a request from there', async () => {
    vi.mocked(api.listOAuthGrants).mockResolvedValue([login()]);
    renderAuth();
    const o = await screen.findByTestId('auth-overview');
    await waitFor(() => expect(o).toHaveTextContent('Waiting for you'));
    expect(o).toHaveTextContent(/1\s*waiting/);
    expect(o).toHaveTextContent(/1\s*approved/);
    fireEvent.click(within(o).getByRole('button', { name: /Claude Code/ }));
    expect(await screen.findByTestId('oauth-row')).toBeInTheDocument();
  });
});

describe('Authorizations — a waiting request', () => {
  it('shows what the operator judges by: redirect host first, client, resource, requested scopes, the opening browser, time left', async () => {
    renderAuth();
    const r = await request();
    expect(within(r).getByTestId('oauth-redirect-host')).toHaveTextContent('localhost:52346');
    expect(r).toHaveTextContent('Claude Code');
    expect(r).toHaveTextContent('https://claude.ai/oauth/claude-code-client-metadata');
    expect(r).toHaveTextContent('http://localhost:52346/callback');
    expect(r).toHaveTextContent('http://localhost:19491/api/v1/repos/kb/mcp');
    expect(r).toHaveTextContent(/loopback/);
    expect(within(r).getByTestId('oauth-requested')).toHaveTextContent('readwritepush:ownmerge:mainoperator');
    expect(r).toHaveTextContent('127.0.0.1:59219');
    expect(r).toHaveTextContent('Safari/605');
    expect(r).toHaveTextContent(/browser that opened/i);
    expect(r).toHaveTextContent('8 min left');
  });

  it('renders requester-supplied text as text, never markup', async () => {
    const hostile = '<img src=x onerror="window.pwned=1">‮edoc';
    vi.mocked(api.listOAuthPending).mockResolvedValue([claude({ client_name: hostile, redirect_uri: 'https://evil.example/cb' })]);
    const { container } = renderAuth();
    const r = await request();
    expect(container.querySelector('img')).toBeNull();
    expect(r.textContent).toContain(hostile);
    expect(within(r).getByTestId('oauth-redirect-host')).toHaveTextContent('evil.example');
    expect(r).toHaveTextContent('A web address. Approve only if you recognise it.');
  });

  // 3b review N1: React escapes markup, but a U+202E in the user agent still
  // reorders the text around it on screen. The user agent is isolated in a
  // <bdi> so its direction cannot leak into the address or the time left.
  it('isolates the user agent so a bidi override cannot reorder its neighbours (3b N1)', async () => {
    vi.mocked(api.listOAuthPending).mockResolvedValue([claude({ user_agent: 'agent‮evil' })]);
    renderAuth();
    const ua = within(await request()).getByTestId('oauth-user-agent');
    expect(ua.tagName).toBe('BDI');
    expect(ua.textContent).toBe('agent‮evil');
  });

  it('pre-checks the D13 default, not what the client asked for', async () => {
    renderAuth();
    const r = await request();
    const checked = within(r).getAllByRole('checkbox').filter(c => (c as HTMLInputElement).checked).map(c => c.getAttribute('value'));
    expect(checked).toEqual(['read', 'write']);
  });

  it('approves with the subject and the checked scopes, then reloads and says who it signs in as', async () => {
    vi.mocked(api.listOAuthPending).mockResolvedValueOnce([claude()]).mockResolvedValue([]);
    renderAuth();
    const r = await request();
    const approve = within(r).getByTestId('oauth-approve');
    expect(approve).toBeDisabled(); // no subject yet
    fireEvent.change(within(r).getByLabelText(/subject/i), { target: { value: 'laptop' } });
    fireEvent.click(within(r).getByRole('checkbox', { name: 'write' })); // uncheck write
    fireEvent.click(approve);
    await waitFor(() => expect(api.approveOAuthPending).toHaveBeenCalledWith(claude().id, 'laptop', ['read']));
    const note = await screen.findByTestId('oauth-notice');
    expect(note).toHaveTextContent('Approved Claude Code. It signs in as host:laptop@token.');
    expect(screen.getByTestId('auth-overview')).toBeInTheDocument();
    expect(vi.mocked(api.listOAuthPending).mock.calls.length).toBeGreaterThanOrEqual(2);
  });

  it('after a re-approval, says the grants were left alone and names how to widen them (F19 3c R5)', async () => {
    vi.mocked(api.approveOAuthPending).mockResolvedValue({ subject: 'laptop', grantsUnchanged: true });
    vi.mocked(api.listOAuthPending).mockResolvedValueOnce([claude()]).mockResolvedValue([]);
    renderAuth();
    const r = await request();
    fireEvent.change(within(r).getByLabelText(/subject/i), { target: { value: 'laptop' } });
    fireEvent.click(within(r).getByTestId('oauth-approve'));
    const note = await screen.findByTestId('oauth-notice');
    expect(note).toHaveTextContent('host:laptop@token');
    expect(note).toHaveTextContent('knomit grants add host:laptop@token <perm>');
  });

  it('cannot approve with no scope checked', async () => {
    renderAuth();
    const r = await request();
    fireEvent.change(within(r).getByLabelText(/subject/i), { target: { value: 'laptop' } });
    fireEvent.click(within(r).getByRole('checkbox', { name: 'read' }));
    fireEvent.click(within(r).getByRole('checkbox', { name: 'write' }));
    expect(within(r).getByTestId('oauth-approve')).toBeDisabled();
  });

  it('denies after a confirm', async () => {
    renderAuth();
    const r = await request();
    fireEvent.click(within(r).getByTestId('oauth-deny'));
    expect(api.denyOAuthPending).not.toHaveBeenCalled();
    fireEvent.click(within(r).getByTestId('oauth-deny-confirm'));
    await waitFor(() => expect(api.denyOAuthPending).toHaveBeenCalledWith(claude().id));
    expect(await screen.findByTestId('oauth-notice')).toHaveTextContent('Denied Claude Code');
  });

  it('shows the server\'s refusal of an approval', async () => {
    vi.mocked(api.approveOAuthPending).mockRejectedValue(new Error('oauth: subject must be 1-128 characters'));
    renderAuth();
    const r = await request();
    fireEvent.change(within(r).getByLabelText(/subject/i), { target: { value: 'bad subject' } });
    fireEvent.click(within(r).getByTestId('oauth-approve'));
    await waitFor(() => expect(within(r).getByRole('alert')).toHaveTextContent('subject must be'));
  });

  it('says the request is no longer waiting when it leaves the queue while picked', async () => {
    vi.mocked(api.listOAuthPending).mockResolvedValueOnce([claude()]).mockResolvedValue([]);
    renderAuth();
    await request();
    vi.advanceTimersByTime(5_000);
    expect(await screen.findByTestId('oauth-gone')).toHaveTextContent('No longer waiting');
  });
});

describe('Authorizations — an approved login', () => {
  beforeEach(() => {
    vi.mocked(api.listOAuthGrants).mockResolvedValue([login()]);
  });

  it('shows what it may do and its history: who approved it, when it last got a token, when it lapses', async () => {
    renderAuth();
    const g = await approved();
    expect(g).toHaveTextContent('Claude Code');
    expect(g).toHaveTextContent('host:laptop@token');
    expect(g).toHaveTextContent('readwrite');
    expect(g).toHaveTextContent('bridge:uid:501@local');
    expect(g).toHaveTextContent('40 min ago');
    expect(g).toHaveTextContent('in 32 days, then the client must ask again');
    expect(g).not.toHaveTextContent(/unless the client refreshes/);
  });

  it('says "not yet" for a login that never got a token, and names an unknown approver', async () => {
    vi.mocked(api.listOAuthGrants).mockResolvedValue([login({ last_used_at: null, approved_by: '' })]);
    renderAuth();
    const g = await approved();
    expect(g).toHaveTextContent(/Last used\s*not yet/);
    expect(g).toHaveTextContent('by unknown');
  });

  it('warns when its sign-in lapses within a week', async () => {
    vi.mocked(api.listOAuthGrants).mockResolvedValue([login({ refresh_expires_at: '2026-09-27T12:00:00Z' })]);
    renderAuth();
    expect((await screen.findAllByTestId('oauth-grant-item'))[0]).toHaveTextContent('3 days');
    expect(await approved()).toHaveTextContent('sign-in lapses in 3 days');
  });

  it('revokes after a confirm, then reloads and says so', async () => {
    renderAuth();
    const g = await approved();
    fireEvent.click(within(g).getByTestId('oauth-revoke'));
    expect(api.revokeOAuthGrant).not.toHaveBeenCalled();
    vi.mocked(api.listOAuthGrants).mockResolvedValue([]);
    fireEvent.click(within(g).getByTestId('oauth-revoke-confirm'));
    await waitFor(() => expect(api.revokeOAuthGrant).toHaveBeenCalledWith('fam-1'));
    expect(await screen.findByTestId('oauth-notice')).toHaveTextContent('Revoked Claude Code (host:laptop@token).');
    await waitFor(() => expect(screen.queryByTestId('oauth-grant-item')).toBeNull());
  });

  it('shows the server\'s refusal of a revoke and stays on the login', async () => {
    vi.mocked(api.revokeOAuthGrant).mockRejectedValue(new Error('oauth: no such approved login'));
    renderAuth();
    const g = await approved();
    fireEvent.click(within(g).getByTestId('oauth-revoke'));
    fireEvent.click(within(g).getByTestId('oauth-revoke-confirm'));
    await waitFor(() => expect(within(g).getByRole('alert')).toHaveTextContent('no such approved login'));
    expect(screen.getByTestId('oauth-grant')).toBeInTheDocument();
  });
});
