import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, act, fireEvent, cleanup, within } from '@testing-library/react';
import { ChatFacet } from './ChatFacet';
import { installFakeEventSource, uninstallFakeEventSource, streamsMatching, latestStream } from './testEventSource';
import { stubFleetFetch } from './fleetTestFetch';
import type { ChatSnapshot } from './chatApi';

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
    const { fx } = mount(snapshot({ items: [] }), { [`POST ${BASE}/turns`]: { status: 202, body: {} } });
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
      es.emit('text_delta', { text: 'hi ' });
      es.emit('text_delta', { text: 'there' });
      es.emit('usage', { usage: { input: 11, cache_read: 22, cache_write: 33, output: 44 }, cost_usd: 0.0123 });
      es.emit('turn_done', {});
    });
    expect(screen.getByTestId('chat-text')).toHaveTextContent('hi there');
    expect(screen.getByTestId('chat-usage').textContent).toBe('input 11 · cache read 22 · cache write 33 · output 44 · $0.0123');
    expect(screen.getByTestId('chat-send')).toBeInTheDocument();
  });

  it('a model without a price card shows "cost unknown"', async () => {
    mount(snapshot({ items: [{ kind: 'user', text: 'q' }] }));
    await screen.findByTestId('chat-turn-0');
    act(() => { latestStream('/chats/chat-a/events')!.emit('usage', { usage: { input: 11, cache_read: 22, cache_write: 33, output: 44 } }); });
    expect(screen.getByTestId('chat-usage').textContent).toBe('input 11 · cache read 22 · cache write 33 · output 44 · cost unknown');
  });

  it('shows the server\'s warning and a refusal as sent, keeps the partial text and offers no retry', async () => {
    mount(snapshot({ items: [{ kind: 'user', text: 'q' }] }));
    await screen.findByTestId('chat-turn-0');
    const es = latestStream('/chats/chat-a/events')!;
    act(() => {
      es.emit('text_delta', { text: 'partial answer' });
      es.emit('warning', { message: 'This chat is nearing 100,000 tokens. Past that, each message costs more. Start a new chat to save.', threshold: 95000 });
      es.emit('turn_error', { message: 'The model declined to answer.' });
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
