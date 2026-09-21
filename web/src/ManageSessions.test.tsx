import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent, act } from '@testing-library/react';
import { ManageSessions } from './ManageSessions';
import { api } from './api';
import { FakeEventSource, installFakeEventSource, uninstallFakeEventSource } from './testEventSource';

vi.mock('./api', async importOriginal => ({
  ...(await importOriginal<typeof import('./api')>()),
  api: { listClientSessions: vi.fn() },
}));

const POLICY = { dead_after_s: 3600, hidden_after_s: 10800, retention_s: 604800, live_window_s: 360, limit: 500, max_limit: 2000 };
const now = new Date('2026-09-14T12:00:00Z');
const sess = (over: Partial<import('./api').ClientSession>): import('./api').ClientSession => ({
  id: 'mcp-session-1', instance_id: 'abc', state: 'live' as const, transport: 'stdio' as const,
  binding: { kind: 'repo', uid: 'u1', name: 'core' }, bindings: [], branch: 'agent/h-1',
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
    truncated: false,
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
      truncated: false,
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

  // The tab strip already says "Sessions"; the heading that used to sit here
  // said it a second time. Scoped to the page's own subtree.
  it('renders no heading of its own — the tab strip already names the page', async () => {
    render(<ManageSessions />);
    await waitFor(() => expect(api.listClientSessions).toHaveBeenCalledTimes(1));
    expect(screen.getByTestId('manage-sessions').querySelector('h1, h2, h3')).toBeNull();
  });

  // Both payloads of the deleted heading row have to survive it.
  it('keeps the live count and Show hidden in the card label row', async () => {
    render(<ManageSessions />);
    await waitFor(() => expect(api.listClientSessions).toHaveBeenCalledTimes(1));
    const labelRow = screen.getByLabelText('Show hidden').closest('div');
    expect(labelRow).toHaveTextContent('MCP clients');
    expect(labelRow).toHaveTextContent(/\d+ live · \d+ shown/);
  });

  // The policy line moved into the card as its pinned footer. It must still be
  // rendered, and still inside the card rather than orphaned above it.
  it('pins the policy line inside the card', async () => {
    render(<ManageSessions />);
    await waitFor(() => expect(screen.getByTestId('session-policy')).toBeInTheDocument());
    const card = screen.getByTestId('manage-sessions').firstElementChild as HTMLDivElement;
    expect(card.contains(screen.getByTestId('session-policy'))).toBe(true);
  });

  // Same mechanism as the Logs card. At the flex default (min-height:auto) the
  // card floors at its MIN-CONTENT height, which propagates the table's full
  // height up through the wrapper — the card grows to fit every row and the
  // pane scrolls instead of the table. The floor lives on the root instead.
  it('pins the card to the pane instead of to the table it contains', async () => {
    render(<ManageSessions />);
    await waitFor(() => expect(api.listClientSessions).toHaveBeenCalledTimes(1));
    const root = screen.getByTestId('manage-sessions');
    const card = root.firstElementChild as HTMLDivElement;
    expect(card.style.flex).toBe('1 1 0%');
    expect(card.style.minHeight).toBe('0px');
    expect(root.style.height).toBe('100%');
    expect(parseInt(root.style.minHeight, 10)).toBeGreaterThan(200);
  });

  // An overflow on the card would absorb the overflow that is supposed to push
  // the root past the pane, disabling the floor's degradation entirely, and
  // nest a second scrollbar inside the table wrapper's. The card carried
  // overflowX:'auto' before this change, so this is a regression guard.
  it('leaves the card with no overflow of its own', async () => {
    render(<ManageSessions />);
    await waitFor(() => expect(api.listClientSessions).toHaveBeenCalledTimes(1));
    const card = screen.getByTestId('manage-sessions').firstElementChild as HTMLDivElement;
    // Assert we are looking at the CARD and not whatever else happens to be
    // first: without this the test passes vacuously against any structure
    // whose first child is not the card, which is exactly the structure this
    // change replaced.
    expect(card.querySelector('table')).not.toBeNull();
    expect(card.style.overflow).toBe('');
    expect(card.style.overflowX).toBe('');
    expect(card.style.overflowY).toBe('');
  });

  // Under borderCollapse:'separate' a border on a <tr> is not painted at all,
  // which would silently delete every row separator. The header rule is an
  // inset shadow precisely so 'collapse' can stay.
  it('keeps collapsed borders so the row separators survive the sticky header', async () => {
    render(<ManageSessions />);
    await waitFor(() => expect(api.listClientSessions).toHaveBeenCalledTimes(1));
    const table = screen.getByTestId('manage-sessions').querySelector('table') as HTMLTableElement;
    expect(table.style.borderCollapse).toBe('collapse');
    const th = table.querySelector('thead th') as HTMLElement;
    expect(th.style.position).toBe('sticky');
    expect(th.style.top).toBe('0px');
    const row = screen.getAllByTestId('session-row')[0];
    // jsdom serialises the colour to rgb().
    expect(row.style.borderTop).toBe('1px solid rgb(34, 34, 34)');
  });

  // jsdom does not lay out, so this pins the PROPERTY that produces the
  // spacing, not the rendered gap itself. The defect it guards: the label was
  // plain inline text where `{' '}` rendered as a real space, and turning it
  // into a flex container silently dropped that space, leaving the checkbox
  // flush against the "S" of "Show hidden" (overlapping it in WebKit).
  it('spaces the Show hidden checkbox from its words with a gap, not whitespace', async () => {
    render(<ManageSessions />);
    await waitFor(() => expect(api.listClientSessions).toHaveBeenCalledTimes(1));
    const input = screen.getByLabelText('Show hidden') as HTMLInputElement;
    const label = input.closest('label') as HTMLLabelElement;
    expect(label.style.display).toBe('flex');
    expect(label.style.gap).toBe('6px');
    // The dead whitespace node is gone rather than left to mislead. Asserting
    // the exact string also catches a `{' '}` being put back: that would make
    // textContent ' Show hidden' without restoring any visible space.
    expect(label.textContent).toBe('Show hidden');
    // No margin fallback exists — App.css resets `*` to margin:0 — so the gap
    // above is the only spacing mechanism. Named here so a later reader does
    // not assume a second one is holding it up.
    expect(input.style.margin).toBe('');
  });
});

// One session id can serve several concurrent callers, each holding its own
// handle, so the Binding cell shows the SET — one line per handle, most
// recently used first — not just the last one.
describe('ManageSessions binding set', () => {
  const bindingRow = (over: Partial<import('./api').ClientSessionBindingRow> = {}) => ({
    handle: 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA', kind: 'repo', uid: 'u1', name: 'core', branch: '',
    first_seen_at: '2026-09-14T11:00:00Z', last_seen_at: '2026-09-14T11:58:00Z', request_count: 1,
    ...over,
  });

  it('renders one line per handle, including two handles on the same repo', async () => {
    vi.mocked(api.listClientSessions).mockResolvedValue({
      truncated: false,
      policy: POLICY,
      sessions: [sess({
        bindings: [
          bindingRow({ handle: 'HHHHHHbbbbbbbbbbbbbbbbbbbbbbbbbb', name: 'core' }),
          bindingRow({ handle: 'GGGGGGaaaaaaaaaaaaaaaaaaaaaaaaaa', name: 'core' }),
        ],
      })],
    });
    render(<ManageSessions />);
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(1));

    const entries = screen.getAllByTestId('session-binding');
    expect(entries).toHaveLength(2);
    // Both name the same repo — the handle is what tells them apart, so it has
    // to be on screen or the two lines are indistinguishable.
    expect(entries[0]).toHaveTextContent('core');
    expect(entries[0]).toHaveTextContent('HHHHHH');
    expect(entries[1]).toHaveTextContent('GGGGGG');
  });

  it('shows the full handle on hover and the branch only when set', async () => {
    vi.mocked(api.listClientSessions).mockResolvedValue({
      truncated: false,
      policy: POLICY,
      sessions: [sess({
        bindings: [
          bindingRow({ handle: 'FULLHANDLEVALUE0000000000000000x', branch: 'main' }),
          bindingRow({ handle: 'SECONDHANDLE00000000000000000000', name: 'eng', kind: 'lens', branch: '' }),
        ],
      })],
    });
    render(<ManageSessions />);
    await waitFor(() => expect(screen.getAllByTestId('session-binding')).toHaveLength(2));
    const entries = screen.getAllByTestId('session-binding');

    // Shortened on screen, whole value in the title — 32 opaque characters
    // would dominate the row, but an operator correlating a log line needs all
    // of it.
    expect(entries[0]).toHaveTextContent('FULLHA');
    expect(entries[0]).not.toHaveTextContent('FULLHANDLEVALUE0000000000000000x');
    expect(entries[0].querySelector('[title="FULLHANDLEVALUE0000000000000000x"]')).not.toBeNull();

    expect(entries[0]).toHaveTextContent('@main');
    // "" means the target's own read branch — nothing to show.
    expect(entries[1]).not.toHaveTextContent('@');
  });

  it('falls back to the singular binding when the session presented no handle', async () => {
    vi.mocked(api.listClientSessions).mockResolvedValue({
      truncated: false,
      policy: POLICY,
      sessions: [sess({ bindings: [], binding: { kind: 'repo', uid: 'u1', name: 'core' } })],
    });
    render(<ManageSessions />);
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(1));
    // A URL-scoped caller presents no handle, so the cell shows what the row
    // itself says rather than going blank.
    expect(screen.queryAllByTestId('session-binding')).toHaveLength(0);
    expect(screen.getByTestId('session-bindings')).toHaveTextContent('core');
  });

  it('renders a read-only server\'s redacted rows without a handle', async () => {
    vi.mocked(api.listClientSessions).mockResolvedValue({
      truncated: false,
      policy: POLICY,
      sessions: [sess({ bindings: [bindingRow({ handle: '', branch: '' })] })],
    });
    render(<ManageSessions />);
    await waitFor(() => expect(screen.getAllByTestId('session-binding')).toHaveLength(1));
    const entry = screen.getAllByTestId('session-binding')[0];
    expect(entry).toHaveTextContent('core');
    expect(entry).not.toHaveTextContent('…');
  });
});

// The server bounds the page. A cut list renders identically to a complete one,
// so the note is the only thing that stops "12 shown" being read as "12 exist".
describe('ManageSessions truncation', () => {
  it('shows a visible note naming the count when the page was cut', async () => {
    vi.mocked(api.listClientSessions).mockResolvedValue({
      truncated: true, policy: POLICY,
      sessions: [sess({ id: 'a' }), sess({ id: 'b' })],
    });
    render(<ManageSessions />);
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(2));

    const note = screen.getByTestId('session-truncated');
    expect(note).toBeInTheDocument();
    // Names how many are on screen, so the reader knows what "more" is relative
    // to rather than being told only that something is missing.
    expect(note).toHaveTextContent('2');
    expect(note).toHaveTextContent(/more/i);
  });

  it('shows no note when the list is exhausted', async () => {
    vi.mocked(api.listClientSessions).mockResolvedValue({
      truncated: false, policy: POLICY, sessions: [sess({ id: 'a' })],
    });
    render(<ManageSessions />);
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(1));
    expect(screen.queryByTestId('session-truncated')).toBeNull();
  });

  // The flag has to follow the data: a page that was truncated and then is not
  // must drop the note, or it outlives the condition it describes.
  it('clears the note when a later poll is not truncated', async () => {
    vi.mocked(api.listClientSessions).mockResolvedValue({
      truncated: true, policy: POLICY, sessions: [sess({ id: 'a' })],
    });
    render(<ManageSessions />);
    await waitFor(() => expect(screen.getByTestId('session-truncated')).toBeInTheDocument());

    vi.mocked(api.listClientSessions).mockResolvedValue({
      truncated: false, policy: POLICY, sessions: [sess({ id: 'a' })],
    });
    await act(async () => { vi.advanceTimersByTime(30_000); });
    await waitFor(() => expect(screen.queryByTestId('session-truncated')).toBeNull());
  });
});

describe('ManageSessions — the branch a session actually writes to', () => {
  // The whole point of the `mounts` field: a URL-scoped mount names a branch in
  // its path, but a session inside an experiment writes somewhere else. The
  // table must state the STORED answer, never re-derive one from the URL — a
  // column that read the path would confidently show the wrong branch.
  it('shows the experiment for a session inside one', async () => {
    (api.listClientSessions as ReturnType<typeof vi.fn>).mockResolvedValue({
      sessions: [sess({
        branch: 'agent/h-1',
        mounts: [{
          mount: 'repo:u1', kind: 'repo', uid: 'u1', name: 'core',
          experiment: 'pr-237-ui', branch: 'exp/pr-237-ui',
          set_at: '2026-09-14T11:30:00Z',
        }],
      })],
      policy: POLICY,
    });
    render(<ManageSessions onLiveCount={() => {}} />);

    const cell = await screen.findByTestId('session-write-branch');
    expect(cell.textContent).toContain('pr-237-ui');
    // The client's own git branch is a DIFFERENT thing and must not be what
    // this column shows while an experiment is active.
    expect(cell.textContent).not.toContain('agent/h-1');
  });

  it('falls back to the reported branch when no experiment is open', async () => {
    (api.listClientSessions as ReturnType<typeof vi.fn>).mockResolvedValue({
      sessions: [sess({ branch: 'agent/h-1' })],
      policy: POLICY,
    });
    render(<ManageSessions onLiveCount={() => {}} />);

    const cell = await screen.findByTestId('session-write-branch');
    expect(cell.textContent).toContain('agent/h-1');
  });
});
