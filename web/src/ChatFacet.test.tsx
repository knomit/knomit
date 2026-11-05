import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, act, fireEvent, cleanup, within } from '@testing-library/react';
import { ChatFacet } from './ChatFacet';
import { installFakeEventSource, uninstallFakeEventSource, streamsMatching, latestStream } from './testEventSource';
import { stubFleetFetch } from './fleetTestFetch';
import { SEND_TIMEOUT_MS } from './chatApi';
import type { ChatSnapshot } from './chatApi';
import { CHAT_SEND_TIMEOUT, toolsInContextLine } from './chatCopy';

const CHAT = { id: 'chat-a', provider: 'anthropic', model: 'claude-test-x' };
const BASE = '/api/v1/chats/chat-a';

const snapshot = (over: Partial<ChatSnapshot> = {}): ChatSnapshot => ({
  ...CHAT, running: false, grants: [], pending_approvals: [],
  items: [
    { kind: 'user', text: 'what repos are there?' },
    { kind: 'assistant', text: 'let me look' },
    { kind: 'tool_call', id: 'tu_1', name: 'knomit_repos', state: 'done' },
    { kind: 'assistant', text: 'There is **one** repo.' },
  ],
  ...over,
});

const CALL_1 = { name: 'knomit_repos', arguments: {}, result: 'alpha  beta', is_error: false, elided: false };

function mount(snap: ChatSnapshot = snapshot(), extra: Record<string, Parameters<typeof stubFleetFetch>[0][string]> = {}, facet: 'chat' | 'settings' = 'chat') {
  const fx = stubFleetFetch({
    [`GET ${BASE}`]: { body: snap },
    [`GET ${BASE}/calls/tu_1`]: { body: CALL_1 },
    ...extra,
  });
  const onGone = vi.fn();
  const utils = render(<ChatFacet chat={CHAT} facet={facet} onGone={onGone} />);
  return { fx, onGone, ...utils };
}
const callsTo = (fx: ReturnType<typeof stubFleetFetch>, part: string) => fx.calls.filter(c => c.route.includes(part));

beforeEach(() => { installFakeEventSource(); });
afterEach(() => { cleanup(); uninstallFakeEventSource(); vi.unstubAllGlobals(); });

describe('ChatFacet', () => {
  it('puts the chat panel in the left slot and the result panel in the right slot', async () => {
    mount();
    await screen.findByTestId('chat-turn-0');
    // No selected result: the chat panel only.
    expect(screen.getByTestId('chat-panel')).toBeInTheDocument();
    expect(screen.queryByTestId('chat-result-panel')).toBeNull();

    fireEvent.click(screen.getByTestId('chat-tool-group-toggle'));
    fireEvent.click(screen.getByTestId('chat-tool-select-tu_1'));
    const result = await screen.findByTestId('chat-result-panel');
    const panel = screen.getByTestId('chat-panel');
    const columns = screen.getByTestId('chat-columns');
    expect(columns).toContainElement(panel);
    expect(columns).toContainElement(result);
    // Browse's order: the list, then the fact.
    expect(panel.compareDocumentPosition(result) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    // And in FacetColumns' own slots: the left (index) column holds the chat.
    expect(within(screen.getByTestId('facet-index-column')).getByTestId('chat-panel')).toBe(panel);
    expect(within(screen.getByTestId('settings-scroll')).getByTestId('chat-result-panel')).toBe(result);
  });

  it('stacks below FACET_NARROW_PX, as Browse\'s columns do', async () => {
    const desc = Object.getOwnPropertyDescriptor(HTMLElement.prototype, 'clientWidth');
    Object.defineProperty(HTMLElement.prototype, 'clientWidth', { configurable: true, get: () => 375 });
    try {
      mount();
      await screen.findByTestId('chat-turn-0');
      expect(screen.getByTestId('chat-columns')).toHaveAttribute('data-layout', 'stacked');
    } finally {
      if (desc) Object.defineProperty(HTMLElement.prototype, 'clientWidth', desc);
      else delete (HTMLElement.prototype as unknown as Record<string, unknown>).clientWidth;
    }
  });

  it('a group is collapsed by default and fetches a result only on select', async () => {
    const { fx } = mount();
    await screen.findByTestId('chat-turn-0');
    const toggle = screen.getByTestId('chat-tool-group-toggle');
    expect(toggle).toHaveTextContent('Used Knomit');
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    expect(screen.queryByTestId('chat-tool-row-tu_1')).toBeNull();
    expect(callsTo(fx, '/calls/')).toHaveLength(0);

    fireEvent.click(toggle);
    const row = screen.getByTestId('chat-tool-row-tu_1');
    expect(row).toHaveTextContent('knomit: Knomit repos ▸');
    // Opening the group is not selecting a row.
    expect(callsTo(fx, '/calls/')).toHaveLength(0);

    fireEvent.click(screen.getByTestId('chat-tool-select-tu_1'));
    const result = await screen.findByTestId('chat-result-panel');
    await waitFor(() => expect(within(result).getByTestId('chat-result-text')).toHaveTextContent('alpha beta'));
    expect(callsTo(fx, '/calls/').map(c => c.route)).toEqual([`GET ${BASE}/calls/tu_1`]);
    // The result is in the result panel, not in the chat panel.
    expect(within(screen.getByTestId('chat-panel')).queryByText(/alpha/)).toBeNull();

    // Selecting the row again closes the panel without another fetch.
    fireEvent.click(screen.getByTestId('chat-tool-select-tu_1'));
    expect(screen.queryByTestId('chat-result-panel')).toBeNull();
    expect(callsTo(fx, '/calls/')).toHaveLength(1);
  });

  it('a running call cannot be selected yet', async () => {
    const { fx } = mount(snapshot({ running: true, items: [{ kind: 'user', text: 'q' }, { kind: 'tool_call', id: 'tu_1', name: 'knomit_repos', state: 'running' }] }));
    await screen.findByTestId('chat-turn-0');
    fireEvent.click(screen.getByTestId('chat-tool-group-toggle'));
    expect(screen.getByTestId('chat-tool-select-tu_1')).toBeDisabled();
    expect(callsTo(fx, '/calls/')).toHaveLength(0);
  });

  it('final text renders as markdown below the group; the text before the group is inside it', async () => {
    mount();
    await screen.findByTestId('chat-turn-0');
    const final = screen.getByTestId('chat-text');
    expect(final).toHaveTextContent('There is one repo.');
    expect(final.querySelector('strong')).toHaveTextContent('one');
    const group = screen.getByTestId('chat-tool-group');
    expect(group.compareDocumentPosition(final) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    // "let me look" is the interstitial: hidden with the collapsed group.
    expect(screen.queryByText('let me look')).toBeNull();
    fireEvent.click(screen.getByTestId('chat-tool-group-toggle'));
    expect(within(group).getByText('let me look')).toBeInTheDocument();
  });

  it('streams a turn: the message, the events, and the turn\'s usage with its cost', async () => {
    const { fx } = mount(snapshot({ items: [] }), { [`POST ${BASE}/turns`]: { status: 202, body: { turn: 0 } } });
    await waitFor(() => expect(callsTo(fx, `GET ${BASE}`).length).toBeGreaterThan(0));
    const es = latestStream('/chats/chat-a/events')!;
    await waitFor(() => expect(screen.getByTestId('chat-input')).toBeInTheDocument());

    fireEvent.change(screen.getByTestId('chat-input'), { target: { value: 'hello' } });
    fireEvent.click(screen.getByTestId('chat-send'));
    await waitFor(() => expect(callsTo(fx, '/turns')).toHaveLength(1));
    expect(callsTo(fx, '/turns')[0].body).toEqual({ text: 'hello' });
    expect(screen.getByTestId('chat-user')).toHaveTextContent('hello');
    expect(screen.getByTestId('chat-stop')).toBeInTheDocument();

    act(() => {
      es.emit('text_delta', { text: 'hi ', turn: 0 });
      es.emit('text_delta', { text: 'there', turn: 0 });
      es.emit('usage', { usage: { input: 11, cache_read: 22, cache_write: 33, output: 44 }, cost_usd: 0.0123, turn: 0 });
      es.emit('turn_done', { turn: 0 });
    });
    expect(screen.getByTestId('chat-text')).toHaveTextContent('hi there');
    expect(screen.getByTestId('chat-usage').textContent).toBe('input 11 · cache read 22 · cache write 33 · output 44 · $0.0123');
    expect(screen.getByTestId('chat-send')).toBeInTheDocument();
  });

  it('shows the tools line of an adaptive chat from the events, then from the snapshot', async () => {
    const withTools = snapshot({ items: [{ kind: 'user', text: 'q', tools: { loaded: 1, evicted: 0, loaded_now: 3, total: 13, tokens: 4100 } }] });
    mount(withTools);
    await screen.findByTestId('chat-turn-0');
    expect(screen.getByTestId('chat-tools-line').textContent).toBe(toolsInContextLine(3, 13, 4100));
    expect(screen.getByTestId('chat-tools-line').textContent).toBe('Tools in context: 3 of 13 (about 4.1K tokens)');

    // A live change before the turn ends updates it at once.
    act(() => { latestStream('/chats/chat-a/events')!.emit('tools', { turn: 0, reason: 'pressure', loaded: [], evicted: ['knomit_query'], loaded_now: ['a', 'b'], total: 13, tokens: 950 }); });
    expect(screen.getByTestId('chat-tools-line').textContent).toBe('Tools in context: 2 of 13 (about 950 tokens)');
  });

  it('a chat that keeps every tool shows no tools line', async () => {
    mount(snapshot({ items: [{ kind: 'user', text: 'q' }] }));
    await screen.findByTestId('chat-turn-0');
    act(() => { latestStream('/chats/chat-a/events')!.emit('usage', { usage: { input: 1, cache_read: 0, cache_write: 0, output: 1 }, turn: 0 }); });
    expect(screen.getByTestId('chat-usage')).toBeInTheDocument();
    expect(screen.queryByTestId('chat-tools-line')).toBeNull();
  });

  it('a model without a price card shows "cost unknown"', async () => {
    mount(snapshot({ items: [{ kind: 'user', text: 'q' }] }));
    await screen.findByTestId('chat-turn-0');
    act(() => { latestStream('/chats/chat-a/events')!.emit('usage', { usage: { input: 11, cache_read: 22, cache_write: 33, output: 44 }, turn: 0 }); });
    expect(screen.getByTestId('chat-usage').textContent).toBe('input 11 · cache read 22 · cache write 33 · output 44 · cost unknown');
  });

  it('shows the server\'s warning and a refusal as sent, keeps the partial text and offers no retry', async () => {
    mount(snapshot({ items: [{ kind: 'user', text: 'q' }] }));
    await screen.findByTestId('chat-turn-0');
    const es = latestStream('/chats/chat-a/events')!;
    act(() => {
      es.emit('text_delta', { text: 'partial answer', turn: 0 });
      es.emit('warning', { message: 'This chat is nearing 100,000 tokens. Past that, each message costs more. Start a new chat to save.', threshold: 95000, turn: 0 });
      es.emit('turn_error', { message: 'The model declined to answer.', turn: 0 });
    });
    expect(screen.getByTestId('chat-warning').textContent).toBe('This chat is nearing 100,000 tokens. Past that, each message costs more. Start a new chat to save.');
    expect(screen.getByTestId('chat-turn-error').textContent).toBe('The model declined to answer.');
    expect(screen.getByTestId('chat-text')).toHaveTextContent('partial answer');
    // The only buttons are the group-less panel's: Send. No retry.
    expect(screen.getAllByRole('button').map(b => b.textContent)).toEqual(['Send']);
  });

  it('every SSE connection goes through openEventStream, and is closed on unmount', async () => {
    const { unmount } = mount();
    await screen.findByTestId('chat-turn-0');
    const streams = streamsMatching('/chats/chat-a/events');
    expect(streams).toHaveLength(1);
    expect(streams[0].url).toBe(`${BASE}/events`);
    unmount();
    expect(streams[0].closedByClient).toBe(true);
  });

  it('ready refetches the transcript, and a reconnect resets to the server\'s view', async () => {
    const { fx } = mount();
    await screen.findByTestId('chat-turn-0');
    expect(callsTo(fx, `GET ${BASE}`).filter(c => c.route === `GET ${BASE}`)).toHaveLength(1);
    fx.set(`GET ${BASE}`, { body: snapshot({ items: [{ kind: 'user', text: 'after reconnect' }] }) });
    act(() => { latestStream('/chats/chat-a/events')!.emit('ready', {}); });
    await waitFor(() => expect(screen.getByTestId('chat-user')).toHaveTextContent('after reconnect'));
    expect(screen.getAllByTestId(/^chat-turn-/)).toHaveLength(1);
  });

  it('chat_end hands the chat back as gone', async () => {
    const { onGone } = mount();
    await screen.findByTestId('chat-turn-0');
    act(() => { latestStream('/chats/chat-a/events')!.emit('chat_end', { reason: 'deleted' }); });
    expect(onGone).toHaveBeenCalledTimes(1);
    expect(onGone).toHaveBeenCalledWith('chat-a');
  });

  it('a transcript the server does not have (404) hands the chat back as gone', async () => {
    const { onGone } = mount(snapshot(), { [`GET ${BASE}`]: { status: 404, body: { detail: 'no such chat' } } });
    await waitFor(() => expect(onGone).toHaveBeenCalledWith('chat-a'));
  });

  it('pagehide ends the chat', async () => {
    const { fx } = mount();
    await screen.findByTestId('chat-turn-0');
    const fetchMock = vi.mocked(globalThis.fetch);
    fetchMock.mockClear();
    act(() => { window.dispatchEvent(new Event('pagehide')); });
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe(BASE);
    expect(init?.method).toBe('DELETE');
    expect(init?.keepalive).toBe(true);
    expect(fx.calls.some(c => c.route === `DELETE ${BASE}`)).toBe(true);
  });

  it('Settings shows the grants and keeps the chat mounted underneath', async () => {
    const { rerender, fx } = mount(snapshot(), {
      [`GET ${BASE}/grants`]: { body: { _embedded: { grants: [{ target: 'alpha', kind: 'repo', tool: 'knomit_learn', action: 'learn' }] } } },
    });
    await screen.findByTestId('chat-turn-0');
    fireEvent.change(screen.getByTestId('chat-input'), { target: { value: 'draft kept' } });
    rerender(<ChatFacet chat={CHAT} facet="settings" onGone={vi.fn()} />);
    expect(await screen.findByTestId('chat-grant-row')).toHaveTextContent('alphaknomit_learnlearn');
    expect(callsTo(fx, '/grants')).toHaveLength(1);
    expect(screen.getByTestId('chat-facet')).toHaveStyle({ display: 'none' });
    rerender(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} />);
    expect(screen.queryByTestId('chat-settings')).toBeNull();
    expect(screen.getByTestId('chat-input')).toHaveValue('draft kept');
  });
});

// The reviewer's N1, N2 and N4 at the page: a turn end that races a snapshot
// read, Stop, a cancel, and repeated frames.
describe('ChatFacet: turn ends and repeats', () => {
  it('N3: a turn_done that arrives while a snapshot read is in flight cannot leave the chat running: one read follows the end, and the stale answer is not applied', async () => {
    const live = snapshot({ running: true, items: [{ kind: 'user', text: 'q' }, { kind: 'assistant', text: 'part' }] });
    const settled = snapshot({ running: false, items: [{ kind: 'user', text: 'q' }, { kind: 'assistant', text: 'part and the rest' }] });
    // read 0 (ready) lands at once and makes turn 0 known; read 1 is cut while the
    // turn is still running and held; read 2 is what the turn end must start.
    const st = scriptedReads([live, live, settled], 1);
    render(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} />);
    await screen.findByTestId('chat-stop');
    act(() => { latestStream('/chats/chat-a/events')!.emit('ready', {}); });
    await waitFor(() => expect(st.held).toBe(true));
    expect(st.n).toBe(2);

    act(() => { latestStream('/chats/chat-a/events')!.emit('turn_done', { turn: 0 }); });
    await act(async () => { st.release(); });
    await waitFor(() => expect(screen.getByTestId('chat-text')).toHaveTextContent('part and the rest'));
    expect(screen.queryByTestId('chat-stop')).toBeNull();
    expect(screen.getByTestId('chat-send')).toBeInTheDocument();
    // Exactly one read followed the end (coalesced with nothing else).
    await act(async () => { await new Promise(r => setTimeout(r, 20)); });
    expect(st.n).toBe(3);
  });

  it('N1: Stop clears the running state here, even when the stream never reports an end', async () => {
    const { fx } = mount(snapshot({ running: true, items: [{ kind: 'user', text: 'q' }] }), { [`POST ${BASE}/cancel`]: { status: 204 } });
    await screen.findByTestId('chat-stop');
    fireEvent.click(screen.getByTestId('chat-stop'));
    await waitFor(() => expect(screen.getByTestId('chat-send')).toBeInTheDocument());
    expect(screen.queryByTestId('chat-stop')).toBeNull();
    expect(callsTo(fx, '/cancel')).toHaveLength(1);
  });

  it('N1: Stop clears it even when the cancel request fails', async () => {
    mount(snapshot({ running: true, items: [{ kind: 'user', text: 'q' }] }), { [`POST ${BASE}/cancel`]: { status: 500, body: { detail: 'boom' } } });
    fireEvent.click(await screen.findByTestId('chat-stop'));
    await waitFor(() => expect(screen.getByTestId('chat-send')).toBeInTheDocument());
    await waitFor(() => expect(screen.getByTestId('chat-action-error')).toHaveTextContent('boom'));
  });

  it('N2: a cancelled turn shows no error and keeps its partial text', async () => {
    mount(snapshot({ running: true, items: [{ kind: 'user', text: 'q' }] }));
    await screen.findByTestId('chat-turn-0');
    act(() => {
      const es = latestStream('/chats/chat-a/events')!;
      es.emit('text_delta', { text: 'partial', turn: 0 });
      es.emit('turn_error', { message: 'llm: the turn was cancelled', cancelled: true, turn: 0 });
    });
    expect(screen.queryByTestId('chat-turn-error')).toBeNull();
    expect(screen.queryByText(/cancelled/)).toBeNull();
    expect(screen.getByTestId('chat-text')).toHaveTextContent('partial');
    expect(screen.getByTestId('chat-send')).toBeInTheDocument();
  });

  it('N4: a repeated usage frame is counted once, and a repeated running frame leaves a done call done', async () => {
    mount(snapshot({ running: true, items: [{ kind: 'user', text: 'q' }, { kind: 'tool_call', id: 'tu_1', name: 'knomit_repos', state: 'done' }] }));
    await screen.findByTestId('chat-turn-0');
    const es = latestStream('/chats/chat-a/events')!;
    const u = { usage: { input: 11, cache_read: 22, cache_write: 33, output: 44 }, cost_usd: 0.5, seq: 3 };
    act(() => {
      es.emit('usage', { ...u, turn: 0 });
      es.emit('usage', { ...u, turn: 0 });
      es.emit('tool_call', { id: 'tu_1', name: 'knomit_repos', state: 'running', turn: 0 });
    });
    expect(screen.getByTestId('chat-usage').textContent).toBe('input 11 · cache read 22 · cache write 33 · output 44 · $0.5000');
    fireEvent.click(screen.getByTestId('chat-tool-group-toggle'));
    expect(screen.getByTestId('chat-tool-row-tu_1')).toHaveAttribute('data-state', 'done');
    expect(screen.getByTestId('chat-tool-select-tu_1')).toBeEnabled();
  });
});

// Round 3: a failed turn racing a snapshot read, and a quick follow-up send.
describe('ChatFacet: turn_error races and fast follow-ups', () => {
  it('a turn_error that arrives while a snapshot read is in flight cannot leave the chat running', async () => {
    const live = snapshot({ running: true, items: [{ kind: 'user', text: 'q' }, { kind: 'assistant', text: 'part' }] });
    const settled = snapshot({ running: false, items: [{ kind: 'user', text: 'q', error: 'The model declined to answer.' }, { kind: 'assistant', text: 'part and more' }] });
    const st = scriptedReads([live, live, settled], 1);
    render(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} />);
    await screen.findByTestId('chat-stop');
    act(() => { latestStream('/chats/chat-a/events')!.emit('ready', {}); });
    await waitFor(() => expect(st.held).toBe(true));

    act(() => { latestStream('/chats/chat-a/events')!.emit('turn_error', { message: 'The model declined to answer.', turn: 0 }); });
    await act(async () => { st.release(); }); // the stale read answers last
    await waitFor(() => expect(screen.getByTestId('chat-text')).toHaveTextContent('part and more'));
    expect(screen.queryByTestId('chat-stop')).toBeNull();
    expect(screen.getByTestId('chat-send')).toBeInTheDocument();
    // The refusal comes from the snapshot, which carries how each turn ended.
    expect(screen.getByTestId('chat-turn-error').textContent).toBe('The model declined to answer.');
    await act(async () => { await new Promise(r => setTimeout(r, 20)); });
    expect(st.n).toBe(3);
  });

  it('a follow-up whose POST is still unanswered survives the snapshot read a turn end starts, and is not duplicated once the server lists it', async () => {
    const fx = stubFleetFetch({
      [`GET ${BASE}`]: { body: snapshot({ running: false, items: [{ kind: 'user', text: 'first' }, { kind: 'assistant', text: 'answer' }] }) },
      [`GET ${BASE}/calls/tu_1`]: { body: CALL_1 },
    });
    const inner = globalThis.fetch;
    let acceptPost!: () => void;
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
      if (url === `${BASE}/turns`) return new Promise<Response>(res => { acceptPost = () => res(new Response('{"turn":2}', { status: 202 })); });
      return inner(input, init);
    }));
    render(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} />);
    await screen.findByTestId('chat-turn-0');
    fireEvent.change(screen.getByTestId('chat-input'), { target: { value: 'second' } });
    fireEvent.click(screen.getByTestId('chat-send'));
    await waitFor(() => expect(screen.getAllByTestId('chat-user')).toHaveLength(2));

    // The first turn's end reaches the page now and starts a snapshot read; the
    // server has not answered the POST, so that read cannot list the follow-up.
    const reads = () => fx.calls.filter(c => c.route === `GET ${BASE}`).length;
    const before = reads();
    act(() => { latestStream('/chats/chat-a/events')!.emit('turn_done', { turn: 0 }); });
    await waitFor(() => expect(reads()).toBe(before + 1));
    await act(async () => { await new Promise(r => setTimeout(r, 20)); });
    expect(screen.getAllByTestId('chat-user').map(e => e.textContent)).toEqual(['first', 'second']);

    // The POST is answered 202; the server has the turn. A read started now lists it.
    await act(async () => { acceptPost(); });
    fx.set(`GET ${BASE}`, { body: snapshot({ running: true, items: [
      { kind: 'user', text: 'first' }, { kind: 'assistant', text: 'answer' }, { kind: 'user', text: 'second' },
    ] }) });
    act(() => { latestStream('/chats/chat-a/events')!.emit('ready', {}); });
    await waitFor(() => expect(reads()).toBe(before + 2));
    await act(async () => { await new Promise(r => setTimeout(r, 20)); });
    expect(screen.getAllByTestId('chat-user').map(e => e.textContent)).toEqual(['first', 'second']);
    expect(screen.getByTestId('chat-stop')).toBeInTheDocument(); // running comes from the snapshot
  });

  it('N2: a send that is never answered counts as failed after the bound: the turn is taken back out and the error shown', async () => {
    mount(snapshot({ running: false, items: [{ kind: 'user', text: 'first' }, { kind: 'assistant', text: 'answer' }] }));
    await screen.findByTestId('chat-turn-0');
    const inner = globalThis.fetch;
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
      if (url === `${BASE}/turns`) {
        // Never answers; only the page's own abort ends it.
        return new Promise<Response>((_res, rej) => {
          init?.signal?.addEventListener('abort', () => rej(new DOMException('aborted', 'AbortError')));
        });
      }
      return inner(input, init);
    }));
    fireEvent.change(screen.getByTestId('chat-input'), { target: { value: 'second' } });
    vi.useFakeTimers({ shouldAdvanceTime: false });
    try {
      fireEvent.click(screen.getByTestId('chat-send'));
      expect(screen.getAllByTestId('chat-user').map(e => e.textContent)).toEqual(['first', 'second']);
      await act(async () => { await vi.advanceTimersByTimeAsync(SEND_TIMEOUT_MS - 1); });
      // Still waiting just before the bound.
      expect(screen.getAllByTestId('chat-user')).toHaveLength(2);
      await act(async () => { await vi.advanceTimersByTimeAsync(2); });
    } finally {
      vi.useRealTimers();
    }
    await waitFor(() => expect(screen.getAllByTestId('chat-user').map(e => e.textContent)).toEqual(['first']));
    expect(screen.getByTestId('chat-action-error').textContent).toBe(`${BASE}/turns → ${CHAT_SEND_TIMEOUT}`);
    expect(screen.getByTestId('chat-send')).toBeInTheDocument();
  });

  it.each([[409, 'a turn is running'], [500, 'store unavailable']])('a send the server answers %i takes the unaccepted turn back out and shows the error', async (status, detail) => {
    mount(snapshot({ running: false, items: [{ kind: 'user', text: 'first' }, { kind: 'assistant', text: 'answer' }] }),
      { [`POST ${BASE}/turns`]: { status, body: { detail } } });
    await screen.findByTestId('chat-turn-0');
    fireEvent.change(screen.getByTestId('chat-input'), { target: { value: 'second' } });
    fireEvent.click(screen.getByTestId('chat-send'));
    await waitFor(() => expect(screen.getByTestId('chat-action-error')).toHaveTextContent(detail));
    expect(screen.getByTestId('chat-action-error')).toHaveTextContent(String(status));
    expect(screen.getAllByTestId('chat-user').map(e => e.textContent)).toEqual(['first']);
    expect(screen.queryByTestId('chat-stop')).toBeNull();
    expect(screen.getByTestId('chat-send')).toBeInTheDocument();
  });
});

describe('ChatFacet: the snapshot is the whole truth', () => {
  it('a reconnect keeps every earlier turn\'s usage line, cost and warning', async () => {
    const items = [
      { kind: 'user' as const, text: 'one', usage: { input: 1, cache_read: 2, cache_write: 3, output: 4 }, cost_usd: 0.25, warning: 'The server warns here.' },
      { kind: 'assistant' as const, text: 'A' },
      { kind: 'user' as const, text: 'two', usage: { input: 5, cache_read: 6, cache_write: 7, output: 8 } },
      { kind: 'assistant' as const, text: 'B' },
    ];
    mount(snapshot({ items }));
    await screen.findByTestId('chat-turn-1');
    const lines = () => screen.getAllByTestId('chat-usage').map(e => e.textContent);
    const expected = [
      'input 1 · cache read 2 · cache write 3 · output 4 · $0.2500',
      'input 5 · cache read 6 · cache write 7 · output 8 · cost unknown',
    ];
    expect(lines()).toEqual(expected);
    expect(screen.getByTestId('chat-warning').textContent).toBe('The server warns here.');

    // The stream reconnects: `ready` again, the transcript read again.
    act(() => { latestStream('/chats/chat-a/events')!.emit('ready', {}); });
    await act(async () => { await new Promise(r => setTimeout(r, 20)); });
    expect(lines()).toEqual(expected);
    expect(screen.getAllByTestId('chat-warning')).toHaveLength(1);
  });
});

// Round 5: live events are routed by the turn index they name.
describe('ChatFacet: events are routed by turn index', () => {
  const learnApproval = (turn: number) => ({
    call_id: 'tu_9', tool: 'knomit_learn', action: 'learn', target: 'alpha', target_kind: 'repo',
    destructive: false, grantable: true, arguments: {}, turn,
  });
  /** fetch with POST /turns held until the test answers it. */
  function holdPosts() {
    const inner = globalThis.fetch;
    const h = { answer: undefined as unknown as (turn: number) => void, posts: 0 };
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
      if (url === `${BASE}/turns`) {
        h.posts++;
        return new Promise<Response>(res => { h.answer = (turn: number) => res(new Response(JSON.stringify({ turn }), { status: 202 })); });
      }
      return inner(input, init);
    }));
    return h;
  }
  const sendText = (t: string) => {
    fireEvent.change(screen.getByTestId('chat-input'), { target: { value: t } });
    fireEvent.click(screen.getByTestId('chat-send'));
  };

  it('the late 202: the new turn\'s first words, a tool call and its approval arrive before the 202 and all land on the new turn', async () => {
    stubFleetFetch({
      [`GET ${BASE}`]: { body: snapshot({ items: [{ kind: 'user', text: 'first' }, { kind: 'assistant', text: 'answer' }] }) },
    });
    const post = holdPosts();
    render(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} />);
    await screen.findByTestId('chat-turn-0');
    sendText('second');
    await waitFor(() => expect(post.posts).toBe(1));

    // The server accepted the POST (turn index 2) and is already working.
    const es = latestStream('/chats/chat-a/events')!;
    act(() => {
      es.emit('text_delta', { text: 'new words', turn: 2 });
      es.emit('tool_call', { id: 'tu_9', name: 'knomit_learn', state: 'running', turn: 2 });
      es.emit('approval', learnApproval(2));
      es.emit('usage', { usage: { input: 5, cache_read: 0, cache_write: 0, output: 1 }, seq: 1, turn: 2 });
    });
    expect(within(screen.getByTestId('chat-turn-1')).getByTestId('chat-approval-tu_9')).toBeInTheDocument();
    expect(within(screen.getByTestId('chat-turn-1')).getByTestId('chat-usage')).toHaveTextContent('input 5');
    // The previous turn is untouched: its text and (absent) usage.
    expect(within(screen.getByTestId('chat-turn-0')).queryByTestId('chat-usage')).toBeNull();
    expect(within(screen.getByTestId('chat-turn-0')).queryByTestId('chat-approval-tu_9')).toBeNull();
    expect(screen.getByTestId('chat-turn-0')).not.toHaveTextContent('new words');

    // Now the 202 arrives, naming the same index: nothing moves.
    await act(async () => { post.answer(2); });
    expect(within(screen.getByTestId('chat-turn-1')).getByTestId('chat-approval-tu_9')).toBeInTheDocument();
    expect(screen.getAllByTestId('chat-user').map(e => e.textContent)).toEqual(['first', 'second']);
  });

  it('the first message of a chat: the approval shows in its row although the 202 has not arrived', async () => {
    stubFleetFetch({ [`GET ${BASE}`]: { body: snapshot({ items: [] }) } });
    const post = holdPosts();
    render(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} />);
    await waitFor(() => expect(screen.getByTestId('chat-input')).toBeInTheDocument());
    await act(async () => { await new Promise(r => setTimeout(r, 10)); });
    sendText('hello');
    await waitFor(() => expect(post.posts).toBe(1));

    const es = latestStream('/chats/chat-a/events')!;
    act(() => {
      es.emit('tool_call', { id: 'tu_9', name: 'knomit_learn', state: 'running', turn: 0 });
      es.emit('approval', learnApproval(0));
    });
    expect(within(screen.getByTestId('chat-tool-row-tu_9')).getByTestId('chat-approval-tu_9')).toBeInTheDocument();
    expect(screen.getAllByTestId('chat-user').map(e => e.textContent)).toEqual(['hello']);
  });

  it('a late event from the stopped turn still lands on that turn after a new local turn exists', async () => {
    stubFleetFetch({
      [`GET ${BASE}`]: { body: snapshot({ running: true, items: [{ kind: 'user', text: 'first' }, { kind: 'assistant', text: 'partial' }] }) },
      [`POST ${BASE}/cancel`]: { status: 204 },
    });
    const post = holdPosts();
    render(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} />);
    fireEvent.click(await screen.findByTestId('chat-stop'));
    await waitFor(() => expect(screen.getByTestId('chat-send')).toBeInTheDocument());
    sendText('again');
    await waitFor(() => expect(post.posts).toBe(1));

    act(() => { latestStream('/chats/chat-a/events')!.emit('text_delta', { text: ' and a late tail', turn: 0 }); });
    expect(screen.getByTestId('chat-turn-0')).toHaveTextContent('partial and a late tail');
    expect(screen.getByTestId('chat-turn-1')).not.toHaveTextContent('late tail');
  });

  it('an event for a turn started elsewhere, with no local turn, makes the page read the snapshot', async () => {
    const fx = stubFleetFetch({ [`GET ${BASE}`]: { body: snapshot({ items: [{ kind: 'user', text: 'first' }, { kind: 'assistant', text: 'answer' }] }) } });
    render(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} />);
    await screen.findByTestId('chat-turn-0');
    const reads = () => fx.calls.filter(c => c.route === `GET ${BASE}`).length;
    const before = reads();
    fx.set(`GET ${BASE}`, { body: snapshot({ running: true, items: [
      { kind: 'user', text: 'first' }, { kind: 'assistant', text: 'answer' }, { kind: 'user', text: 'typed in another tab' },
    ] }) });
    act(() => { latestStream('/chats/chat-a/events')!.emit('text_delta', { text: 'x', turn: 2 }); });
    await waitFor(() => expect(reads()).toBe(before + 1));
    await waitFor(() => expect(screen.getAllByTestId('chat-user').map(e => e.textContent)).toEqual(['first', 'typed in another tab']));
  });

  it('N2: after a send times out the page reads the snapshot, so a turn the server did append (and its approval) shows up', async () => {
    const fx = stubFleetFetch({
      [`GET ${BASE}`]: { body: snapshot({ items: [{ kind: 'user', text: 'first' }, { kind: 'assistant', text: 'answer' }] }) },
    });
    const inner = globalThis.fetch;
    vi.stubGlobal('fetch', vi.fn((input: string | URL | Request, init?: RequestInit) => {
      const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
      if (url === `${BASE}/turns`) {
        return new Promise<Response>((_res, rej) => { init?.signal?.addEventListener('abort', () => rej(new DOMException('aborted', 'AbortError'))); });
      }
      return inner(input, init);
    }));
    render(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} />);
    await screen.findByTestId('chat-turn-0');
    const reads = () => fx.calls.filter(c => c.route === `GET ${BASE}`).length;
    const before = reads();
    // The server did take the turn, and it is waiting for approval.
    fx.set(`GET ${BASE}`, { body: snapshot({
      running: true,
      items: [
        { kind: 'user', text: 'first' }, { kind: 'assistant', text: 'answer' },
        { kind: 'user', text: 'second' }, { kind: 'tool_call', id: 'tu_9', name: 'knomit_learn', state: 'running' },
      ],
      pending_approvals: [learnApproval(2)],
    }) });
    vi.useFakeTimers({ shouldAdvanceTime: false });
    try {
      sendText('second');
      await act(async () => { await vi.advanceTimersByTimeAsync(SEND_TIMEOUT_MS + 1); });
    } finally {
      vi.useRealTimers();
    }
    await waitFor(() => expect(reads()).toBe(before + 1));
    expect(await screen.findByTestId('chat-approval-tu_9')).toBeInTheDocument();
    expect(screen.getAllByTestId('chat-user').map(e => e.textContent)).toEqual(['first', 'second']);
    expect(screen.getByTestId('chat-action-error').textContent).toBe(`${BASE}/turns → ${CHAT_SEND_TIMEOUT}`);
  });
});

/** GET /chats/{id} answers from `bodies`, one per read, in order; the read at
 *  `holdAt` waits until `release()`. `reads()` counts every read. */
function scriptedReads(bodies: ChatSnapshot[], holdAt: number) {
  const st = { n: 0, release: () => {}, held: false };
  vi.stubGlobal('fetch', vi.fn((input: string | URL | Request, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    if (url === BASE && !init?.method) {
      const i = st.n++;
      const res = () => new Response(JSON.stringify(bodies[Math.min(i, bodies.length - 1)]), { status: 200 });
      if (i === holdAt) { st.held = true; return new Promise<Response>(r => { st.release = () => r(res()); }); }
      return Promise.resolve(res());
    }
    return Promise.resolve(new Response('{}', { status: 404 }));
  }));
  return st;
}

describe('ChatFacet: snapshot reads are coalesced', () => {
  it('N1: while a read is in flight, 5 unknown-turn events start no other read; after it resolves exactly one more follows', async () => {
    const first = snapshot({ items: [{ kind: 'user', text: 'first' }, { kind: 'assistant', text: 'answer' }] });
    const second = snapshot({ running: true, items: [
      { kind: 'user', text: 'first' }, { kind: 'assistant', text: 'answer' }, { kind: 'user', text: 'typed in another tab' },
    ] });
    // read 0 (ready) is stalled; the follow-up must be read 1.
    const st = scriptedReads([first, second], 0);
    render(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} />);
    await waitFor(() => expect(st.held).toBe(true));
    expect(st.n).toBe(1);

    // Five events for turns the page does not know, while that read is out.
    act(() => {
      const es = latestStream('/chats/chat-a/events')!;
      for (let k = 0; k < 5; k++) es.emit('text_delta', { text: 'x', turn: 10 + k });
    });
    await act(async () => { await new Promise(r => setTimeout(r, 20)); });
    expect(st.n).toBe(1); // no second GET while the first is in flight

    await act(async () => { st.release(); });
    await waitFor(() => expect(screen.getAllByTestId('chat-user').map(e => e.textContent)).toEqual(['first', 'typed in another tab']));
    await act(async () => { await new Promise(r => setTimeout(r, 20)); });
    expect(st.n).toBe(2); // exactly one follow-up for the five triggers
  });
});
