import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, cleanup, within } from '@testing-library/react';
import { ActivityFacet } from './ActivityFacet';
import { api } from './api';
import type { ClientSession } from './api';
import { installFakeEventSource, uninstallFakeEventSource, streamsMatching } from './testEventSource';

vi.mock('./api', async importOriginal => ({
  ...(await importOriginal<typeof import('./api')>()),
  api: { listClientSessions: vi.fn() },
}));

const POLICY = { dead_after_s: 3600, hidden_after_s: 10800, retention_s: 604800, live_window_s: 360, limit: 500, max_limit: 2000 };
const sess = (over: Partial<ClientSession>): ClientSession => ({
  id: 'mcp-session-1', instance_id: 'abc', state: 'live', transport: 'stdio',
  binding: { kind: 'repo', uid: 'u-ops', name: 'ops-kb' }, bindings: [], branch: '',
  client: { name: 'claude-code', version: '2.1', initialized: true },
  bridge: { host: 'h1', user: 'pba', cwd: '/w', pid: 42, parent: 'claude', parent_pid: 41, version: '1.4' },
  remote_addr: '127.0.0.1', user_agent: 'knomit-bridge/1.4',
  first_seen_at: '2026-09-14T11:00:00Z', last_seen_at: '2026-09-14T11:58:00Z', ended_at: null, request_count: 12,
  ...over,
});
const handle = (kind: string, uid: string, name: string, h: string) => ({
  handle: h, kind, uid, name, branch: '', first_seen_at: '2026-09-14T11:00:00Z', last_seen_at: '2026-09-14T11:58:00Z', request_count: 3,
});

const OPS = { kind: 'repo' as const, uid: 'u-ops' };

beforeEach(() => {
  localStorage.clear();
  installFakeEventSource();
  vi.mocked(api.listClientSessions).mockResolvedValue({ truncated: false, policy: POLICY, sessions: [sess({})] });
});
afterEach(() => {
  cleanup();
  uninstallFakeEventSource();
  vi.clearAllMocks();
});

describe('ActivityFacet', () => {
  it('asks the server for the repo target\'s sessions by binding', async () => {
    render(<ActivityFacet target={OPS} />);
    await waitFor(() => expect(api.listClientSessions).toHaveBeenCalled());
    expect(vi.mocked(api.listClientSessions).mock.calls[0][0]).toEqual({ binding: 'repo:u-ops', includeHidden: false });
  });

  it('asks by lens:<uid> for a lens target', async () => {
    render(<ActivityFacet target={{ kind: 'lens', uid: 'l-dev' }} />);
    await waitFor(() => expect(api.listClientSessions).toHaveBeenCalled());
    expect(vi.mocked(api.listClientSessions).mock.calls[0][0]).toEqual({ binding: 'lens:l-dev', includeHidden: false });
  });

  it('shows only the target\'s binding group for a session also bound elsewhere', async () => {
    vi.mocked(api.listClientSessions).mockResolvedValue({
      truncated: false, policy: POLICY,
      sessions: [sess({
        bindings: [
          handle('repo', 'u-ops', 'ops-kb', 'h-ops-1'),
          handle('repo', 'u-core', 'core-kb', 'h-core-1'),
          handle('repo', 'u-ops', 'ops-kb', 'h-ops-2'),
        ],
      })],
    });
    render(<ActivityFacet target={OPS} />);
    const row = await screen.findByTestId('session-row');
    const groups = within(row).getAllByTestId('session-binding-group');
    expect(groups).toHaveLength(1);
    expect(groups[0].textContent).toContain('ops-kb');
    expect(row.textContent).not.toContain('core-kb');
    // The handle count is this target's: two handles, not three.
    expect(within(row).getByTestId('session-handles-toggle').textContent).toContain('2 handles');
  });

  it('puts "N live · M shown" in the toolbar', async () => {
    vi.mocked(api.listClientSessions).mockResolvedValue({
      truncated: false, policy: POLICY,
      sessions: [sess({ id: 'a' }), sess({ id: 'b' }), sess({ id: 'c', state: 'idle' })],
    });
    render(<ActivityFacet target={OPS} />);
    const toolbar = screen.getByTestId('activity-toolbar');
    await waitFor(() => expect(within(toolbar).getByTestId('activity-counts').textContent).toBe('2 live · 3 shown'));
    expect(within(toolbar).getByText('Sessions')).toBeInTheDocument();
    expect(within(toolbar).getByLabelText('Show hidden')).toBeInTheDocument();
  });

  it('shows the truncation note when the page was cut', async () => {
    vi.mocked(api.listClientSessions).mockResolvedValue({ truncated: true, policy: POLICY, sessions: [sess({})] });
    render(<ActivityFacet target={OPS} />);
    expect(await within(screen.getByTestId('activity-toolbar')).findByTestId('session-truncated')).toBeInTheDocument();
  });

  it('shows no truncation note when the list is exhausted', async () => {
    render(<ActivityFacet target={OPS} />);
    await screen.findByTestId('session-row');
    expect(screen.queryByTestId('session-truncated')).toBeNull();
  });

  it('makes the policy line the last child of the table container', async () => {
    render(<ActivityFacet target={OPS} />);
    const policy = await screen.findByTestId('session-policy');
    expect(screen.getByTestId('sessions-table').lastElementChild).toBe(policy);
  });

  it('has no card: the table container is the body under the toolbar', async () => {
    render(<ActivityFacet target={OPS} />);
    await screen.findByTestId('session-row');
    const facet = screen.getByTestId('activity-facet');
    expect(Array.from(facet.children).map(c => c.getAttribute('data-testid'))).toEqual(['activity-toolbar', 'sessions-table']);
    // Bounded by its parent (the flex-min-content rule), so it scrolls inside.
    expect(screen.getByTestId('sessions-table').style.minHeight).toBe('0px');
  });

  it('opens one sessions stream while shown, and none after it goes', async () => {
    const { unmount } = render(<ActivityFacet target={OPS} />);
    await waitFor(() => expect(streamsMatching('/sessions/events').filter(s => !s.closedByClient)).toHaveLength(1));
    unmount();
    expect(streamsMatching('/sessions/events').filter(s => !s.closedByClient)).toHaveLength(0);
  });
});
