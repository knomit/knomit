import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent, act, within } from '@testing-library/react';
import { ActivityFacet } from './ActivityFacet';
import type { SessionTarget } from './useTargetSessions';
import { api } from './api';
import { FakeEventSource, installFakeEventSource, uninstallFakeEventSource } from './testEventSource';

// The sessions table and its reader, as the Activity facet mounts them: the
// sessions bound to ONE target (here repo `core`, uid u1), filtered by the
// server, with only that target's binding groups drawn. These were Manage's
// Sessions tab tests; the global view went with the tab.
const CORE: SessionTarget = { kind: 'repo', uid: 'u1' };

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
  localStorage.clear();
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

describe('Activity sessions', () => {
  it('renders one row per session with state, client, parent app, binding and relative last-seen', async () => {
    render(<ActivityFacet target={CORE} />);
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(3));
    const rows = screen.getAllByTestId('session-row');
    expect(rows[0]).toHaveTextContent('claude-code');
    expect(rows[0]).toHaveTextContent('claude');
    expect(rows[0]).toHaveTextContent('core');
    expect(rows[0]).toHaveTextContent('2 min ago');
    expect(rows[0]).toHaveTextContent('12');
    expect(rows[1]).toHaveTextContent('resumed');     // initialized=false
    expect(rows[2]).toHaveTextContent('ended');       // ended_at set
  });

  it('show hidden re-fetches with include=hidden', async () => {
    render(<ActivityFacet target={CORE} />);
    await waitFor(() => expect(api.listClientSessions).toHaveBeenCalledTimes(1));
    fireEvent.click(screen.getByLabelText('Show hidden'));
    await waitFor(() => expect(api.listClientSessions).toHaveBeenLastCalledWith({ binding: 'repo:u1', includeHidden: true }));
  });

  it('polls every 30 seconds while mounted and on window focus', async () => {
    render(<ActivityFacet target={CORE} />);
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
    render(<ActivityFacet target={CORE} />);
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(1));
    expect(screen.getByTestId('session-policy')).toHaveTextContent('never purged');
    expect(screen.getByTestId('session-policy')).not.toHaveTextContent('kept 0 d');
  });

  // The connect-time `ready` must cost NOTHING. The page has just read the
  // list; the server announces itself the moment the stream opens; treating
  // that as a change made every mount pay for two identical GETs, and no test
  // saw it because the fake never announced a connection.
  it('does not re-read for the ready the server sends on connect', async () => {
    render(<ActivityFacet target={CORE} />);
    const es = await stream();
    expect(es.url).toContain('/api/v1/sessions/events');
    await act(async () => { vi.advanceTimersByTime(6_000); });
    expect(api.listClientSessions).toHaveBeenCalledTimes(1);
  });

  // A session appearing or going away is what the reader is watching for, so
  // it is not worth throttling behind a window.
  it('re-reads immediately when a session initializes or ends', async () => {
    render(<ActivityFacet target={CORE} />);
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
    render(<ActivityFacet target={CORE} />);
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
    render(<ActivityFacet target={CORE} />);
    const es = await stream();
    await act(async () => { vi.advanceTimersByTime(2_000); });
    expect(api.listClientSessions).toHaveBeenCalledTimes(1);

    await act(async () => { es.emit('open'); es.emit('ready', {}); });
    expect(api.listClientSessions).toHaveBeenCalledTimes(2);
  });

  it('closes the stream on unmount', async () => {
    const { unmount } = render(<ActivityFacet target={CORE} />);
    await waitFor(() => expect(api.listClientSessions).toHaveBeenCalledTimes(1));
    const es = await stream();
    unmount();
    expect(es.closeCount).toBe(1);
  });

  it('keeps ages moving while an error banner is up', async () => {
    render(<ActivityFacet target={CORE} />);
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

  // Under borderCollapse:'separate' a border on a <tr> is not painted at all,
  // which would silently delete every row separator. The header rule is an
  // inset shadow precisely so 'collapse' can stay.
  it('keeps collapsed borders so the row separators survive the sticky header', async () => {
    render(<ActivityFacet target={CORE} />);
    await waitFor(() => expect(api.listClientSessions).toHaveBeenCalledTimes(1));
    const table = screen.getByTestId('activity-facet').querySelector('table') as HTMLTableElement;
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
    render(<ActivityFacet target={CORE} />);
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

// The server bounds the page. A cut list renders identically to a complete one,
// so the note is the only thing that stops "12 shown" being read as "12 exist".
describe('Activity sessions truncation', () => {
  it('shows a visible note naming the count when the page was cut', async () => {
    vi.mocked(api.listClientSessions).mockResolvedValue({
      truncated: true, policy: POLICY,
      sessions: [sess({ id: 'a' }), sess({ id: 'b' })],
    });
    render(<ActivityFacet target={CORE} />);
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
    render(<ActivityFacet target={CORE} />);
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(1));
    expect(screen.queryByTestId('session-truncated')).toBeNull();
  });

  // The flag has to follow the data: a page that was truncated and then is not
  // must drop the note, or it outlives the condition it describes.
  it('clears the note when a later poll is not truncated', async () => {
    vi.mocked(api.listClientSessions).mockResolvedValue({
      truncated: true, policy: POLICY, sessions: [sess({ id: 'a' })],
    });
    render(<ActivityFacet target={CORE} />);
    await waitFor(() => expect(screen.getByTestId('session-truncated')).toBeInTheDocument());

    vi.mocked(api.listClientSessions).mockResolvedValue({
      truncated: false, policy: POLICY, sessions: [sess({ id: 'a' })],
    });
    await act(async () => { vi.advanceTimersByTime(30_000); });
    await waitFor(() => expect(screen.queryByTestId('session-truncated')).toBeNull());
  });
});

// One session id can serve several concurrent callers, each holding its own
// handle, so the Binding cell shows the SET. It shows it GROUPED: one line per
// distinct target with a handle count, because a busy agent presents dozens of
// handles against two repos and eighteen identical lines said nothing the
// count does not. The handles themselves move into an expandable detail row,
// which is also the only place the per-handle ages and request counts the
// server already returns have ever been shown.
describe('Activity sessions binding groups', () => {
  const bindingRow = (over: Partial<import('./api').ClientSessionBindingRow> = {}) => ({
    handle: 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA', kind: 'repo', uid: 'u1', name: 'core', branch: '',
    first_seen_at: '2026-09-14T11:00:00Z', last_seen_at: '2026-09-14T11:58:00Z', request_count: 1,
    ...over,
  });
  const withBindings = (bindings: import('./api').ClientSessionBindingRow[], over: Partial<import('./api').ClientSession> = {}) =>
    vi.mocked(api.listClientSessions).mockResolvedValue({
      truncated: false, policy: POLICY, sessions: [sess({ bindings, ...over })],
    });

  it('collapses two handles on one target into a single line with a count', async () => {
    withBindings([
      bindingRow({ handle: 'HHHHHHbbbbbbbbbbbbbbbbbbbbbbbbbb' }),
      bindingRow({ handle: 'GGGGGGaaaaaaaaaaaaaaaaaaaaaaaaaa' }),
    ]);
    render(<ActivityFacet target={CORE} />);
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(1));

    // Two callers, one target: ONE line. The count is what carries the fact
    // that there are two of them.
    const groups = screen.getAllByTestId('session-binding-group');
    expect(groups).toHaveLength(1);
    expect(groups[0]).toHaveTextContent('core');
    expect(groups[0]).toHaveTextContent('×2');
    // The handles are in the detail row now, not on the summary line.
    expect(groups[0]).not.toHaveTextContent('HHHHHH');
  });

  it('draws only this target\'s line for a session bound here and elsewhere, with its own count', async () => {
    withBindings([
      bindingRow({ handle: 'B1AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA', uid: 'u2', name: 'docs' }),
      bindingRow({ handle: 'B2AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA', uid: 'u1', name: 'core' }),
      bindingRow({ handle: 'B3AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA', uid: 'u2', name: 'docs' }),
    ]);
    render(<ActivityFacet target={CORE} />);
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(1));

    const groups = screen.getAllByTestId('session-binding-group');
    expect(groups).toHaveLength(1);
    expect(groups[0]).toHaveTextContent('core');
    expect(groups[0]).not.toHaveTextContent('docs');
    // One handle on THIS target, so no count, whatever the session holds elsewhere.
    expect(groups[0]).not.toHaveTextContent('×');
  });

  it('opens a row per handle on click, with the full handle on hover, and closes again', async () => {
    withBindings([
      bindingRow({ handle: 'HHHHHHbbbbbbbbbbbbbbbbbbbbbbbbbb', request_count: 31 }),
      bindingRow({ handle: 'GGGGGGaaaaaaaaaaaaaaaaaaaaaaaaaa', request_count: 7, last_seen_at: '2026-09-14T11:00:00Z' }),
    ]);
    render(<ActivityFacet target={CORE} />);
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(1));

    expect(screen.queryByTestId('session-detail')).toBeNull();
    const toggle = screen.getByRole('button', { name: /2 handles/ });
    expect(toggle).toHaveAttribute('aria-expanded', 'false');

    fireEvent.click(toggle);
    const detail = screen.getByTestId('session-detail');
    expect(toggle).toHaveAttribute('aria-expanded', 'true');

    const lines = within(detail).getAllByTestId('session-detail-row');
    expect(lines).toHaveLength(2);
    // Shortened on screen, whole value in the title — 32 opaque characters
    // would dominate, but an operator correlating a log line needs all of it.
    expect(lines[0]).toHaveTextContent('HHHHHH');
    expect(lines[0]).not.toHaveTextContent('HHHHHHbbbbbbbbbbbbbbbbbbbbbbbbbb');
    expect(detail.querySelector('[title="HHHHHHbbbbbbbbbbbbbbbbbbbbbbbbbb"]')).not.toBeNull();
    // The per-handle numbers the server already returns and the table has
    // never shown.
    expect(lines[0]).toHaveTextContent('31');
    expect(lines[1]).toHaveTextContent('7');
    expect(lines[1]).toHaveTextContent('1 h ago');

    fireEvent.click(toggle);
    expect(screen.queryByTestId('session-detail')).toBeNull();
  });

  // The grouped line does not carry the handle, so a ONE-handle session would
  // have no way to reach it at all — and the handle is exactly what an
  // operator correlating a log line needs. One handle therefore still gets a
  // toggle; the label goes singular.
  it('offers the detail row for a single handle too, so the handle stays reachable', async () => {
    withBindings([bindingRow({ handle: 'ONLYONEaaaaaaaaaaaaaaaaaaaaaaaaa' })]);
    render(<ActivityFacet target={CORE} />);
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(1));

    expect(screen.getAllByTestId('session-binding-group')).toHaveLength(1);
    const toggle = screen.getByRole('button', { name: /1 handle$/ });
    expect(screen.queryByTestId('session-detail')).toBeNull();

    fireEvent.click(toggle);
    const detail = screen.getByTestId('session-detail');
    expect(within(detail).getAllByTestId('session-detail-row')).toHaveLength(1);
    expect(detail.querySelector('[title="ONLYONEaaaaaaaaaaaaaaaaaaaaaaaaa"]')).not.toBeNull();
  });

  it('shows the ordinary branch chip when the handle names one', async () => {
    withBindings([bindingRow({ handle: 'ONBRANCHaaaaaaaaaaaaaaaaaaaaaaaa', branch: 'main' })]);
    render(<ActivityFacet target={CORE} />);
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(1));
    expect(screen.getAllByTestId('session-binding-group')[0]).toHaveTextContent('main');
  });

  // "" means the target's own branch — there is nothing to say, and a chip
  // saying nothing is worse than no chip.
  it('shows no chip for a handle with no branch', async () => {
    withBindings([bindingRow({ handle: 'PLAINaaaaaaaaaaaaaaaaaaaaaaaaaaa', branch: '' })]);
    render(<ActivityFacet target={CORE} />);
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(1));
    expect(screen.getByTestId('session-bindings').querySelector('[data-testid="session-branch-chip"]')).toBeNull();
  });

  it('shows both chips when two handles on one target write to different branches', async () => {
    withBindings([
      bindingRow({ handle: 'D1AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA', branch: 'main' }),
      bindingRow({ handle: 'D2AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA', branch: 'agent/other' }),
    ]);
    render(<ActivityFacet target={CORE} />);
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(1));
    const group = screen.getAllByTestId('session-binding-group')[0];
    expect(within(group).getAllByTestId('session-branch-chip')).toHaveLength(2);
    expect(group).toHaveTextContent('main');
    expect(group).toHaveTextContent('agent/other');
  });

  it('falls back to the singular binding when the session presented no handle', async () => {
    withBindings([], { binding: { kind: 'repo', uid: 'u1', name: 'core' } });
    render(<ActivityFacet target={CORE} />);
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(1));
    // A URL-scoped caller presents no handle, so the cell shows what the row
    // itself says rather than going blank — with no count and no toggle.
    expect(screen.getByTestId('session-bindings')).toHaveTextContent('core');
    expect(screen.getByTestId('session-bindings')).not.toHaveTextContent('×');
    expect(screen.queryByRole('button', { name: /handle/ })).toBeNull();
  });

  it('renders an unresolvable target as kind:uid', async () => {
    withBindings([bindingRow({ handle: 'UNRESOLVEDaaaaaaaaaaaaaaaaaaaaaa', kind: 'lens', uid: 'l9', name: null })]);
    render(<ActivityFacet target={{ kind: 'lens', uid: 'l9' }} />);
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(1));
    expect(screen.getAllByTestId('session-binding-group')[0]).toHaveTextContent('lens:l9');
  });

  // A read-only server redacts the handle. The summary line never showed it,
  // so the case that can still break is the detail row: an absent handle has
  // to render as an em dash rather than an empty cell or a bare ellipsis.
  it('renders a read-only server\'s redacted rows without a handle', async () => {
    withBindings([
      bindingRow({ handle: '', branch: '' }),
      bindingRow({ handle: '', branch: '', uid: 'u1', name: 'core', request_count: 4 }),
    ]);
    render(<ActivityFacet target={CORE} />);
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(1));

    const group = screen.getAllByTestId('session-binding-group')[0];
    expect(group).toHaveTextContent('core');
    expect(group).not.toHaveTextContent('…');

    fireEvent.click(screen.getByRole('button', { name: /2 handles/ }));
    const lines = within(screen.getByTestId('session-detail')).getAllByTestId('session-detail-row');
    expect(lines[0]).toHaveTextContent('—');
    expect(lines[0]).not.toHaveTextContent('…');
  });
});

// The Branch column showed the branch the BRIDGE declares about itself, which
// it fills only in --repo mode: empty in lens and unscoped mode, and the same
// agent branch the binding already names when it is set.
describe('Activity sessions — no Branch column', () => {
  it('has no Branch header', async () => {
    render(<ActivityFacet target={CORE} />);
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(3));
    const head = screen.getByTestId('activity-facet').querySelector('thead') as HTMLElement;
    expect(head).not.toHaveTextContent('Branch');
    expect(head).toHaveTextContent('Binding');
    expect(head.querySelectorAll('th')).toHaveLength(7);
  });

  it('spans the empty state across the seven remaining columns', async () => {
    vi.mocked(api.listClientSessions).mockResolvedValue({ truncated: false, policy: POLICY, sessions: [] });
    render(<ActivityFacet target={CORE} />);
    await waitFor(() => expect(api.listClientSessions).toHaveBeenCalledTimes(1));
    const cell = screen.getByText(/No client sessions/) as HTMLTableCellElement;
    expect(cell.colSpan).toBe(7);
  });
});

// Show hidden survives a reload. It is a view preference, not state the server
// owns, so it lives in localStorage — and, like every other key this app
// stores, behind try/catch: a blocked or throwing store must cost the page
// nothing more than the preference.
describe('Activity sessions Show hidden persistence', () => {
  const KEY = 'knomit.sessions.showHidden';

  it('starts checked and asks for hidden rows on the FIRST fetch when remembered', async () => {
    localStorage.setItem(KEY, '1');
    render(<ActivityFacet target={CORE} />);
    await waitFor(() => expect(api.listClientSessions).toHaveBeenCalledTimes(1));
    // The first fetch, not a second one after a state settle: a page that
    // rendered unchecked and corrected itself would flash the wrong list.
    expect(api.listClientSessions).toHaveBeenNthCalledWith(1, { binding: 'repo:u1', includeHidden: true });
    expect(screen.getByLabelText('Show hidden')).toBeChecked();
  });

  it('writes the preference on toggle, both ways', async () => {
    render(<ActivityFacet target={CORE} />);
    await waitFor(() => expect(api.listClientSessions).toHaveBeenCalledTimes(1));
    fireEvent.click(screen.getByLabelText('Show hidden'));
    expect(localStorage.getItem(KEY)).toBe('1');
    fireEvent.click(screen.getByLabelText('Show hidden'));
    // '0' rather than a removal: an explicit off must beat any default a later
    // version might pick.
    expect(localStorage.getItem(KEY)).toBe('0');
  });

  it('renders unchecked, and still toggles, when localStorage throws', async () => {
    const get = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => { throw new Error('blocked'); });
    const set = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => { throw new Error('blocked'); });
    try {
      render(<ActivityFacet target={CORE} />);
      await waitFor(() => expect(api.listClientSessions).toHaveBeenCalledTimes(1));
      expect(screen.getByLabelText('Show hidden')).not.toBeChecked();
      // The write throws too; the checkbox must still move and the list must
      // still re-fetch.
      fireEvent.click(screen.getByLabelText('Show hidden'));
      await waitFor(() => expect(api.listClientSessions).toHaveBeenLastCalledWith({ binding: 'repo:u1', includeHidden: true }));
      expect(screen.getByLabelText('Show hidden')).toBeChecked();
    } finally {
      get.mockRestore();
      set.mockRestore();
    }
  });
});

// Expanded state is keyed by session id and lives for the page load. Keying by
// id is what lets it survive a poll, but it also means a stale id can outlive
// the row it named, so `load` reconciles the set against the ids it just read.
describe('Activity sessions expanded state', () => {
  const bindingRow = (over: Partial<import('./api').ClientSessionBindingRow> = {}) => ({
    handle: 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA', kind: 'repo', uid: 'u1', name: 'core', branch: '',
    first_seen_at: '2026-09-14T11:00:00Z', last_seen_at: '2026-09-14T11:58:00Z', request_count: 1,
    ...over,
  });
  const two = [
    bindingRow({ handle: 'H1AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA' }),
    bindingRow({ handle: 'H2AAAAAAAAAAAAAAAAAAAAAAAAAAAAAA' }),
  ];

  it('survives a poll that returns the same session', async () => {
    vi.mocked(api.listClientSessions).mockResolvedValue({
      truncated: false, policy: POLICY, sessions: [sess({ id: 'keep-me', bindings: two })],
    });
    render(<ActivityFacet target={CORE} />);
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(1));
    fireEvent.click(screen.getByRole('button', { name: /2 handles/ }));
    expect(screen.getByTestId('session-detail')).toBeInTheDocument();

    await act(async () => { vi.advanceTimersByTime(30_000); });
    expect(screen.getByTestId('session-detail')).toBeInTheDocument();
  });

  it('forgets a session that left the list, so its return does not spring open', async () => {
    vi.mocked(api.listClientSessions).mockResolvedValue({
      truncated: false, policy: POLICY, sessions: [sess({ id: 'comes-and-goes', bindings: two })],
    });
    render(<ActivityFacet target={CORE} />);
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(1));
    fireEvent.click(screen.getByRole('button', { name: /2 handles/ }));
    expect(screen.getByTestId('session-detail')).toBeInTheDocument();

    // Gone — purged, or filtered out by a Show hidden change.
    vi.mocked(api.listClientSessions).mockResolvedValue({
      truncated: false, policy: POLICY, sessions: [],
    });
    await act(async () => { vi.advanceTimersByTime(30_000); });
    await waitFor(() => expect(screen.queryAllByTestId('session-row')).toHaveLength(0));

    // Back again. The reader never asked for THIS row to be open.
    vi.mocked(api.listClientSessions).mockResolvedValue({
      truncated: false, policy: POLICY, sessions: [sess({ id: 'comes-and-goes', bindings: two })],
    });
    await act(async () => { vi.advanceTimersByTime(30_000); });
    await waitFor(() => expect(screen.getAllByTestId('session-row')).toHaveLength(1));
    expect(screen.queryByTestId('session-detail')).toBeNull();
    expect(screen.getByRole('button', { name: /2 handles/ })).toHaveAttribute('aria-expanded', 'false');
  });
});
