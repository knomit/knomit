import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent, act } from '@testing-library/react';
import { ManageSessions } from './ManageSessions';
import { api } from './api';
import { FakeEventSource, installFakeEventSource, uninstallFakeEventSource } from './testEventSource';

vi.mock('./api', async importOriginal => ({
  ...(await importOriginal<typeof import('./api')>()),
  api: { listClientSessions: vi.fn() },
}));

const POLICY = { dead_after_s: 3600, hidden_after_s: 10800, retention_s: 604800, live_window_s: 360 };
const now = new Date('2026-09-14T12:00:00Z');
const sess = (over: Partial<import('./api').ClientSession>): import('./api').ClientSession => ({
  id: 'mcp-session-1', instance_id: 'abc', state: 'live' as const, transport: 'stdio' as const,
  binding: { kind: 'repo', uid: 'u1', name: 'core' }, branch: 'agent/h-1',
  client: { name: 'claude-code', version: '2.1', initialized: true },
  bridge: { host: 'h1', user: 'pba', cwd: '/home/pba/p', pid: 42, parent: 'claude', parent_pid: 41, version: '1.4' },
  remote_addr: '127.0.0.1', user_agent: 'knomit-bridge/1.4',
  first_seen_at: '2026-09-14T11:00:00Z', last_seen_at: '2026-09-14T11:58:00Z', ended_at: null, request_count: 12,
  ...over,
});

beforeEach(() => {
  installFakeEventSource();
  vi.useFakeTimers({ now, shouldAdvanceTime: true }); // waitFor needs real progress
  vi.mocked(api.listClientSessions).mockResolvedValue({
    policy: POLICY,
    sessions: [
      sess({}),
      sess({ id: 'mcp-session-2', state: 'dead', client: { name: '', version: '', initialized: false }, last_seen_at: '2026-09-14T09:00:00Z' }),
      sess({ id: 'mcp-session-3', state: 'dead', ended_at: '2026-09-14T11:30:00Z', transport: 'http', binding: { kind: 'lens', uid: 'l9', name: null } }),
    ],
  });
});
afterEach(() => {
  uninstallFakeEventSource();
  vi.useRealTimers();
  vi.clearAllMocks();
});

/** The page's change stream, once it exists. */
async function stream(): Promise<FakeEventSource> {
  await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
  return FakeEventSource.instances[0];
}

describe('ManageSessions', () => {
  it('renders one row per session with state, client, parent app, binding and relative last-seen', async () => {
    render(<ManageSessions />);
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(3));
    const rows = screen.getAllByTestId('session-row');
    expect(rows[0]).toHaveTextContent('claude-code');
    expect(rows[0]).toHaveTextContent('claude');
    expect(rows[0]).toHaveTextContent('core');
    expect(rows[0]).toHaveTextContent('2 min ago');
    expect(rows[0]).toHaveTextContent('12');
    expect(rows[1]).toHaveTextContent('resumed');     // initialized=false
    expect(rows[2]).toHaveTextContent('ended');       // ended_at set
    expect(rows[2]).toHaveTextContent('l9');          // unresolvable binding shows the uid
  });

  it('show hidden re-fetches with include=hidden', async () => {
    render(<ManageSessions />);
    await waitFor(() => expect(api.listClientSessions).toHaveBeenCalledTimes(1));
    fireEvent.click(screen.getByLabelText('Show hidden'));
    await waitFor(() => expect(api.listClientSessions).toHaveBeenLastCalledWith({ binding: undefined, includeHidden: true }));
  });

  it('polls every 30 seconds while mounted and on window focus', async () => {
    render(<ManageSessions />);
    await waitFor(() => expect(api.listClientSessions).toHaveBeenCalledTimes(1));
    await act(async () => { vi.advanceTimersByTime(30_000); });
    expect(api.listClientSessions).toHaveBeenCalledTimes(2);
    await act(async () => { window.dispatchEvent(new Event('focus')); });
    expect(api.listClientSessions).toHaveBeenCalledTimes(3);
  });

  it('says "never purged" when retention is disabled, not "kept 0 d"', async () => {
    vi.mocked(api.listClientSessions).mockResolvedValue({
      policy: { ...POLICY, retention_s: 0 },
      sessions: [sess({})],
    });
    render(<ManageSessions />);
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(1));
    expect(screen.getByTestId('session-policy')).toHaveTextContent('never purged');
    expect(screen.getByTestId('session-policy')).not.toHaveTextContent('kept 0 d');
  });

  // The connect-time `ready` must cost NOTHING. The page has just read the
  // list; the server announces itself the moment the stream opens; treating
  // that as a change made every mount pay for two identical GETs, and no test
  // saw it because the fake never announced a connection.
  it('does not re-read for the ready the server sends on connect', async () => {
    render(<ManageSessions />);
    const es = await stream();
    expect(es.url).toContain('/api/v1/sessions/events');
    await act(async () => { vi.advanceTimersByTime(6_000); });
    expect(api.listClientSessions).toHaveBeenCalledTimes(1);
  });

  // A session appearing or going away is what the reader is watching for, so
  // it is not worth throttling behind a window.
  it('re-reads immediately when a session initializes or ends', async () => {
    render(<ManageSessions />);
    const es = await stream();
    await act(async () => { vi.advanceTimersByTime(2_000); });
    expect(api.listClientSessions).toHaveBeenCalledTimes(1);

    await act(async () => { es.emit('session', { id: 'mcp-session-9', kind: 'init' }); });
    expect(api.listClientSessions).toHaveBeenCalledTimes(2);

    await act(async () => { vi.advanceTimersByTime(2_000); });
    await act(async () => { es.emit('session', { id: 'mcp-session-9', kind: 'end' }); });
    expect(api.listClientSessions).toHaveBeenCalledTimes(3);
  });

  // THE load-amplification bound. Every MCP request from every connected
  // client publishes a `touch`, and a touch only moves last-seen, the request
  // count and idle→live. A Manage tab left open must not turn a busy agent's
  // traffic into a list read per request.
  it('collapses a storm of touches into one read per 5s window', async () => {
    render(<ManageSessions />);
    const es = await stream();
    await act(async () => { vi.advanceTimersByTime(2_000); });
    expect(api.listClientSessions).toHaveBeenCalledTimes(1);

    await act(async () => {
      for (let i = 0; i < 20; i++) { es.emit('session', { id: `s${i}`, kind: 'touch' }); vi.advanceTimersByTime(250); }
    });
    // Trailing edge only — no leading read for a touch.
    expect(api.listClientSessions).toHaveBeenCalledTimes(2);

    await act(async () => { vi.advanceTimersByTime(5_000); });
    expect(api.listClientSessions).toHaveBeenCalledTimes(2);
  });

  // A LATER ready is a reconnect, and the gap may have dropped events, so it
  // is the one ready that does cost a read.
  it('re-reads on a reconnect, which is any ready after the first', async () => {
    render(<ManageSessions />);
    const es = await stream();
    await act(async () => { vi.advanceTimersByTime(2_000); });
    expect(api.listClientSessions).toHaveBeenCalledTimes(1);

    await act(async () => { es.emit('open'); es.emit('ready', {}); });
    expect(api.listClientSessions).toHaveBeenCalledTimes(2);
  });

  it('closes the stream on unmount', async () => {
    const { unmount } = render(<ManageSessions />);
    await waitFor(() => expect(api.listClientSessions).toHaveBeenCalledTimes(1));
    const es = await stream();
    unmount();
    expect(es.closeCount).toBe(1);
  });

  it('keeps ages moving while an error banner is up', async () => {
    render(<ManageSessions />);
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(3));
    expect(screen.getAllByTestId('session-row')[0]).toHaveTextContent('2 min ago');

    // Every poll from here fails. The rows on screen are still the last good
    // data, so their ages must keep advancing rather than freezing at the
    // last success — ten minutes of failed polls must show as ten minutes.
    vi.mocked(api.listClientSessions).mockRejectedValue(new Error('boom'));
    await act(async () => { vi.advanceTimersByTime(600_000); });

    await waitFor(() => expect(screen.getByTestId('session-error')).toBeInTheDocument());
    expect(screen.getAllByTestId('session-row')[0]).toHaveTextContent('12 min ago');
  });
});
