import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent, cleanup } from '@testing-library/react';
import { RepoManager } from './RepoManager';
import { api } from './api';

// Manage is the server's own pages now: Fleet, Logs and Authorizations, as a
// tab strip with no entity rail. Everything about a repository or a lens moved
// into the target's facets and the rail. (The strip itself goes when the
// server pages join the shell.)

vi.mock('./api', async importOriginal => ({
  ...(await importOriginal<typeof import('./api')>()),
  api: {
    // null = this server has no OAuth issuer (404), so no Authorizations tab.
    listOAuthPending: vi.fn().mockResolvedValue(null),
    getFleet: vi.fn().mockResolvedValue(null),
    listFleetMembers: vi.fn().mockResolvedValue([]),
  },
}));

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(api.listOAuthPending).mockResolvedValue(null);
});
afterEach(() => { cleanup(); });

const tabs = () => screen.getAllByRole('tab').map(t => t.getAttribute('data-testid'));

describe('Manage — the server pages', () => {
  it('has exactly the Fleet and Logs tabs on a server with no OAuth issuer, and lands on Fleet', async () => {
    render(<RepoManager open serverReadOnly={false} />);
    await waitFor(() => expect(api.listOAuthPending).toHaveBeenCalled());
    expect(tabs()).toEqual(['repomgr-fleet', 'repomgr-logs']);
    expect(screen.getByTestId('repomgr-fleet')).toHaveAttribute('aria-selected', 'true');
  });

  it('has no entity rail, Knowledge or Sessions tab', async () => {
    render(<RepoManager open serverReadOnly={false} />);
    await screen.findByTestId('repomgr-fleet');
    // Control: the strip itself is there.
    expect(screen.getByRole('tablist', { name: 'Server pages' })).toBeInTheDocument();
    expect(screen.queryByTestId('repomgr-overview')).toBeNull();
    expect(screen.queryByTestId('repomgr-sessions')).toBeNull();
    expect(screen.queryByTestId('repomgr-new')).toBeNull();
    expect(screen.queryByTestId('repomgr-archived')).toBeNull();
  });

  it('opens the Logs page', async () => {
    render(<RepoManager open serverReadOnly={false} />);
    fireEvent.click(await screen.findByTestId('repomgr-logs'));
    expect(await screen.findByTestId('manage-logs')).toBeInTheDocument();
    expect(screen.getByTestId('repomgr-logs')).toHaveAttribute('aria-selected', 'true');
  });

  // The read-only demo refuses the log with a 403, so a Logs tab there would be
  // a control that cannot work — worse than an absent one.
  it('renders no Logs tab at all on a read-only instance', async () => {
    render(<RepoManager open serverReadOnly />);
    await screen.findByTestId('repomgr-fleet');
    expect(tabs()).toEqual(['repomgr-fleet']);
  });

  it('opens the Authorizations page, badged with the waiting count', async () => {
    vi.mocked(api.listOAuthPending).mockResolvedValue([
      { id: 'p1', client_id: 'kb', client_name: 'kb', redirect_uri: 'http://127.0.0.1:1/callback', scopes: ['read'], resource: 'http://localhost:1', remote_addr: '127.0.0.1:2', user_agent: 'ua', created_at: '2026-09-24T12:00:00Z', expires_at: '2026-09-24T12:10:00Z' },
    ]);
    render(<RepoManager open serverReadOnly={false} />);
    expect(await screen.findByTestId('repomgr-oauth-badge')).toHaveTextContent('1');
    expect(tabs()).toEqual(['repomgr-fleet', 'repomgr-logs', 'repomgr-oauth']);
    fireEvent.click(screen.getByTestId('repomgr-oauth'));
    expect(await screen.findByTestId('oauth-row')).toBeInTheDocument();
    expect(screen.getByTestId('repomgr-oauth')).toHaveAttribute('aria-selected', 'true');
  });

  it('renders no Authorizations tab on a read-only instance, and does not probe for one', async () => {
    vi.mocked(api.listOAuthPending).mockResolvedValue([]);
    render(<RepoManager open serverReadOnly />);
    await screen.findByTestId('repomgr-fleet');
    expect(screen.queryByTestId('repomgr-oauth')).toBeNull();
    expect(api.listOAuthPending).not.toHaveBeenCalled();
  });

  it('renders no badge at zero waiting', async () => {
    vi.mocked(api.listOAuthPending).mockResolvedValue([]);
    render(<RepoManager open serverReadOnly={false} />);
    expect(await screen.findByTestId('repomgr-oauth')).toBeInTheDocument();
    expect(screen.queryByTestId('repomgr-oauth-badge')).toBeNull();
  });

  it('renders nothing while closed', () => {
    render(<RepoManager open={false} serverReadOnly={false} />);
    expect(screen.queryByTestId('manage-surface')).toBeNull();
  });
});
