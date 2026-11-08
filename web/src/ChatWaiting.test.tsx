import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, act, fireEvent, cleanup, waitFor, within } from '@testing-library/react';
import { ChatFacet } from './ChatFacet';
import { installFakeEventSource, uninstallFakeEventSource, chatStream } from './testEventSource';
import { stubFleetFetch } from './fleetTestFetch';
import type { ChatSnapshot, ChatViewItem } from './chatApi';
import { CHAT_CONTINUE_MESSAGE } from './chatCopy';
import { SEND_TIMEOUT_MS } from './chatApi';
import { chatReducer, initialChatState, snapshotState, formatDuration, noticeCount, refusalReason } from './chatState';
import type { ChatEvent } from './chatState';

// Chat UX v2, PR 2: waiting for the reply, Stop, and notices. Each test asserts
// WHICH value, not that something shows.

const CHAT = { id: 'chat-a', provider: 'anthropic', model: 'claude-test-x', name: 'Haiku 5.5' };
const BASE = '/api/v1/chats/chat-a';

const snap = (items: ChatViewItem[], over: Partial<ChatSnapshot> = {}): ChatSnapshot =>
  ({ ...CHAT, running: false, grants: [], pending_approvals: [], items, ...over });

/** The fetch seam, with POST /turns held until the test answers it. */
function mount(s: ChatSnapshot, opts: { facet?: 'chat' | 'settings'; onNewChat?: () => void; routes?: Parameters<typeof stubFleetFetch>[0] } = {}) {
  const fx = stubFleetFetch({
    [`GET ${BASE}`]: { body: s },
    [`GET ${BASE}/context`]: { body: { window: 1_000_000, spill: false } },
    [`POST ${BASE}/cancel`]: { status: 204 },
    ...opts.routes,
  });
  const inner = globalThis.fetch;
  let posted: { input: string | URL | Request; init: RequestInit } | null = null;
  const turn: { resolve: (r: Response) => void; pending: boolean } = { resolve: () => {}, pending: false };
  vi.stubGlobal('fetch', vi.fn((input: string | URL | Request, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    if (init?.method === 'POST' && url.endsWith('/turns')) {
      void inner(input, init); // recorded by the stub
      posted = { input, init };
      turn.pending = true;
      return new Promise<Response>((r, rej) => {
        turn.resolve = r;
        init.signal?.addEventListener('abort', () => rej(new DOMException('aborted', 'AbortError')));
      });
    }
    return inner(input, init);
  }));
  const onNewChat = opts.onNewChat ?? vi.fn();
  render(<ChatFacet chat={CHAT} facet={opts.facet ?? 'chat'} onGone={vi.fn()} onNewChat={onNewChat} />);
  const answer = (status: number, body: unknown) => act(async () => {
    turn.resolve(new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } }));
    await new Promise(r => setTimeout(r, 0));
  });
  const calls = (part: string) => fx.calls.filter(c => c.route.includes(part));
  // The POST the page gave up on (aborted) still reaches the server, as a request that sat in a queue would: deliver it now.
  const late = () => inner(posted!.input, { ...posted!.init, signal: undefined });
  return { fx, answer, calls, onNewChat, late };
}

beforeEach(() => { installFakeEventSource(); });
afterEach(() => { vi.useRealTimers(); cleanup(); uninstallFakeEventSource(); vi.unstubAllGlobals(); });

const type = (text: string) => fireEvent.change(screen.getByTestId('chat-input'), { target: { value: text } });
const composer = () => screen.getByTestId('chat-input') as HTMLTextAreaElement;
const send = (text: string) => { type(text); fireEvent.click(screen.getByTestId('chat-send')); };
// findBy/waitFor resolve at the DOM commit, BEFORE its passive effects run. ChatPanel
// registers its window Esc listener (and points stopRef at the current stop) in such an
// effect, so an Esc fired straight after findBy can land before the chat listens. Flush first.
const settled = () => act(async () => {});

describe('Sending: the POST has not been answered', () => {
  it('draws the strip at 60% with a dashed rule and the #161718 ground, says Sending…, and offers Stop, not Cancel', async () => {
    mount(snap([]));
    await screen.findByTestId('chat-input');
    send('Rank them');
    const strip = screen.getByTestId('chat-user');
    expect(strip.getAttribute('data-sending')).toBe('true');
    expect(strip.style.opacity).toBe('0.6');
    expect(strip.style.borderLeftStyle).toBe('dashed');
    expect(strip.style.background).toBe('rgb(22, 23, 24)');
    expect(screen.getByTestId('chat-sending').textContent).toBe('Sending…');
    // One control: Stop. There is no Cancel button anywhere.
    expect(screen.getByTestId('chat-stop').textContent).toBe('StopEsc');
    expect(screen.queryByText(/cancel/i)).toBeNull();
    // Nothing of the accepted state yet.
    expect(screen.queryByTestId('chat-thinking')).toBeNull();
  });

  it('the strip turns solid and the Sending… line goes once the server accepts', async () => {
    const { answer } = mount(snap([]));
    await screen.findByTestId('chat-input');
    send('Rank them');
    await answer(202, { turn: 0 });
    const strip = screen.getByTestId('chat-user');
    expect(strip.getAttribute('data-sending')).toBeNull();
    expect(strip.style.opacity).toBe('');
    expect(screen.queryByTestId('chat-sending')).toBeNull();
    expect(screen.getByTestId('chat-thinking')).toBeInTheDocument();
  });
});

describe('Stop', () => {
  it('Stop before acceptance withdraws the send BY ITS ID and puts the exact text back in the composer', async () => {
    const { calls, answer } = mount(snap([]));
    await screen.findByTestId('chat-input');
    send('Retract only the ones in knomit-kb');
    expect(composer().value).toBe('');
    const sendId = (calls('/turns')[0].body as { send_id: string }).send_id;
    expect(sendId).toBeTruthy();

    fireEvent.click(screen.getByTestId('chat-stop'));
    expect(composer().value).toBe('Retract only the ones in knomit-kb');
    expect(screen.queryByTestId('chat-user')).toBeNull();
    expect(screen.getByTestId('chat-send')).toBeInTheDocument();
    await waitFor(() => expect(calls('/cancel')).toHaveLength(1));
    expect(calls('/cancel')[0].body).toEqual({ send_id: sendId });

    // The send is then refused by the server (withdrawn): no "Not sent" line, the person chose this.
    await answer(409, { title: 'Send withdrawn', detail: 'the send was stopped before the server accepted it' });
    expect(screen.queryByTestId('chat-not-sent')).toBeNull();
    expect(composer().value).toBe('Retract only the ones in knomit-kb');
  });

  it('a Stop that LOST the race to acceptance: the server listed the turn, so the restored text is taken out of the composer (no duplicate)', async () => {
    const { fx, calls, answer } = mount(snap([]));
    await screen.findByTestId('chat-input');
    send('Retract only the ones in knomit-kb');
    fireEvent.click(screen.getByTestId('chat-stop'));
    expect(composer().value).toBe('Retract only the ones in knomit-kb');
    await waitFor(() => expect(calls('/cancel')).toHaveLength(1));
    // The server had already taken the turn (and the cancel ended it): the POST answers 202, the snapshot lists it under OUR send id.
    const sendId = (calls('/turns')[0].body as { send_id: string }).send_id;
    fx.set(`GET ${BASE}`, { body: snap([{ kind: 'user', text: 'Retract only the ones in knomit-kb', send_id: sendId, cancelled: true, error: 'The turn was cancelled.' }]) });
    await answer(202, { turn: 0 });
    await waitFor(() => expect(composer().value).toBe(''));
    expect(screen.getAllByTestId('chat-user').map(e => e.textContent)).toEqual(['Retract only the ones in knomit-kb']);
    expect(screen.queryByTestId('chat-not-sent')).toBeNull();
  });

  it('a Stop that lost the race, but the snapshot holds only ANOTHER tab\'s user item: the restored text stays (it is the only copy)', async () => {
    const { fx, calls, answer } = mount(snap([{ kind: 'user', text: 'earlier' }, { kind: 'assistant', text: 'a' }]));
    await screen.findByTestId('chat-turn-0');
    send('my words');
    fireEvent.click(screen.getByTestId('chat-stop'));
    await waitFor(() => expect(calls('/cancel')).toHaveLength(1));
    // One MORE user item than before, but it is another tab's send, not ours: a count would have taken it for ours.
    fx.set(`GET ${BASE}`, { body: snap([{ kind: 'user', text: 'earlier' }, { kind: 'assistant', text: 'a' }, { kind: 'user', text: 'typed in another tab', send_id: 'other-tab-1' }]) });
    await answer(202, { turn: 2 });
    await new Promise(r => setTimeout(r, 50));
    expect(composer().value).toBe('my words');
    expect(screen.queryByTestId('chat-not-sent')).toBeNull();
  });

  it('when the person typed more after the restored text, only the restored part goes', async () => {
    const { fx, calls, answer } = mount(snap([]));
    await screen.findByTestId('chat-input');
    send('first draft');
    fireEvent.click(screen.getByTestId('chat-stop'));
    type('first draft\nand more');
    await waitFor(() => expect(calls('/cancel')).toHaveLength(1));
    const sendId = (calls('/turns')[0].body as { send_id: string }).send_id;
    fx.set(`GET ${BASE}`, { body: snap([{ kind: 'user', text: 'first draft', send_id: sendId, cancelled: true, error: 'The turn was cancelled.' }]) });
    await answer(202, { turn: 0 });
    await waitFor(() => expect(composer().value).toBe('and more'));
  });

  it('Stop before acceptance keeps what was typed since, after the restored text', async () => {
    const { answer } = mount(snap([]));
    await screen.findByTestId('chat-input');
    send('first draft');
    type('and more');
    fireEvent.click(screen.getByTestId('chat-stop'));
    expect(composer().value).toBe('first draft\nand more');
    await answer(409, {});
  });

  it('Esc anywhere in the chat stops a running turn: the body, and the composer', async () => {
    const { calls } = mount(snap([{ kind: 'user', text: 'q' }], { running: true }));
    await screen.findByTestId('chat-stop');
    await settled();
    fireEvent.keyDown(document.body, { key: 'Escape' });
    await waitFor(() => expect(calls('/cancel')).toHaveLength(1));
    expect(calls('/cancel')[0].body).toBeUndefined();
    expect(screen.queryByTestId('chat-stop')).toBeNull();
    expect(screen.getByTestId('chat-send')).toBeInTheDocument();
  });

  it('Esc typed in the composer stops it too', async () => {
    const { calls } = mount(snap([{ kind: 'user', text: 'q' }], { running: true }));
    await screen.findByTestId('chat-stop');
    await settled();
    fireEvent.keyDown(screen.getByTestId('chat-input'), { key: 'Escape' });
    await waitFor(() => expect(calls('/cancel')).toHaveLength(1));
  });

  it('Esc does nothing in a chat that is not on screen, or when nothing runs', async () => {
    const hidden = mount(snap([{ kind: 'user', text: 'q' }], { running: true }), { facet: 'settings' });
    await waitFor(() => expect(hidden.calls('GET ' + BASE)).not.toHaveLength(0));
    fireEvent.keyDown(document.body, { key: 'Escape' });
    await new Promise(r => setTimeout(r, 20));
    expect(hidden.calls('/cancel')).toHaveLength(0);
    cleanup();
    const idle = mount(snap([{ kind: 'user', text: 'q' }, { kind: 'assistant', text: 'a' }]));
    await screen.findByTestId('chat-turn-0');
    fireEvent.keyDown(document.body, { key: 'Escape' });
    await new Promise(r => setTimeout(r, 20));
    expect(idle.calls('/cancel')).toHaveLength(0);
  });

  it('after a stop: the partial answer stays, "Stopped by you" follows, then Edit and resend and Continue', async () => {
    const { calls } = mount(snap([
      { kind: 'user', text: 'Which incidents involve MCP?', cancelled: true, error: 'The turn was cancelled.' },
      { kind: 'assistant', text: 'Three incidents involve MCP servers directly: and' },
    ]));
    await screen.findByTestId('chat-turn-0');
    expect(screen.getByTestId('chat-text').textContent).toBe('Three incidents involve MCP servers directly: and');
    expect(screen.getByTestId('chat-stopped').textContent).toContain('Stopped by you');
    expect(screen.queryByTestId('chat-turn-error')).toBeNull();
    expect(screen.getByTestId('chat-edit-resend').textContent).toBe('Edit and resend');
    expect(screen.getByTestId('chat-continue').textContent).toBe('Continue');

    fireEvent.click(screen.getByTestId('chat-edit-resend'));
    expect(composer().value).toBe('Which incidents involve MCP?');

    fireEvent.click(screen.getByTestId('chat-continue'));
    await waitFor(() => expect(calls('/turns')).toHaveLength(1));
    expect((calls('/turns')[0].body as { text: string }).text).toBe('Continue.');
    expect(CHAT_CONTINUE_MESSAGE).toBe('Continue.');
  });

  it('the buttons are on the latest stopped turn only; the divider is on each', async () => {
    mount(snap([
      { kind: 'user', text: 'one', cancelled: true, error: 'The turn was cancelled.' }, { kind: 'assistant', text: 'A' },
      { kind: 'user', text: 'two', cancelled: true, error: 'The turn was cancelled.' }, { kind: 'assistant', text: 'B' },
    ]));
    await screen.findByTestId('chat-turn-1');
    expect(screen.getAllByTestId('chat-stopped')).toHaveLength(2);
    expect(screen.getAllByTestId('chat-edit-resend')).toHaveLength(1);
    expect(within(screen.getByTestId('chat-turn-1')).getByTestId('chat-edit-resend')).toBeInTheDocument();
  });

  it('Stop on a running turn marks it stopped at once, and the snapshot that follows is the truth', async () => {
    const { fx } = mount(snap([{ kind: 'user', text: 'q' }, { kind: 'assistant', text: 'part' }], { running: true }));
    await screen.findByTestId('chat-stop');
    fireEvent.click(screen.getByTestId('chat-stop'));
    expect(screen.getByTestId('chat-stopped')).toBeInTheDocument();
    // The server then says the turn finished normally (the stop lost the race): not stopped.
    fx.set(`GET ${BASE}`, { body: snap([{ kind: 'user', text: 'q' }, { kind: 'assistant', text: 'part. done.' }]) });
    await act(async () => { chatStream('chat-a')!.emit('turn_done', { turn: 0 }); await new Promise(r => setTimeout(r, 20)); });
    expect(screen.queryByTestId('chat-stopped')).toBeNull();
  });
});

describe('a send that times out: the snapshot decides, by send_id', () => {
  const EARLIER: ChatViewItem[] = [{ kind: 'user', text: 'earlier', send_id: 'old-1' }, { kind: 'assistant', text: 'a' }];

  /** Send `text` into a non-empty chat, let the 15 s bound pass with the POST unanswered, and return what the page did. */
  async function timeOut(after: (sendId: string) => ChatViewItem[], routes?: Parameters<typeof stubFleetFetch>[0], extraMs = 0) {
    const m = mount(snap(EARLIER), { routes });
    await screen.findByTestId('chat-turn-0');
    vi.useFakeTimers({ shouldAdvanceTime: false });
    try {
      send('my words');
      const sendId = (m.calls('/turns')[0].body as { send_id: string }).send_id;
      m.fx.set(`GET ${BASE}`, { body: snap(after(sendId)) });
      await act(async () => { await vi.advanceTimersByTimeAsync(SEND_TIMEOUT_MS + 1 + extraMs); });
      return { ...m, sendId };
    } finally {
      vi.useRealTimers();
    }
  }

  it('(a) the snapshot holds ANOTHER tab\'s user item and not ours: the text is restored and "Not sent" shows', async () => {
    const { calls, sendId } = await timeOut(() => [...EARLIER, { kind: 'user', text: 'typed in another tab', send_id: 'other-tab-1' }]);
    await waitFor(() => expect(screen.getByTestId('chat-not-sent')).toBeInTheDocument());
    await waitFor(() => expect(composer().value).toBe('my words'));
    expect(screen.getByTestId('chat-not-sent').textContent).toBe('Not sent: no answerRetry');
    // The send was stopped by name, so a late arrival cannot start a turn.
    expect(calls('/cancel').map(c => c.body)).toEqual([{ send_id: sendId }]);
    // Both the person's earlier turn and the other tab's are in the transcript, ours is not.
    expect(screen.getAllByTestId('chat-user').map(e => e.textContent)).toEqual(['earlier', 'typed in another tab']);
  });

  it('(b) a non-empty transcript in which OURS landed: the text is not restored and nothing says "Not sent"', async () => {
    const { calls, sendId } = await timeOut(id => [...EARLIER, { kind: 'user', text: 'my words', send_id: id }]);
    await waitFor(() => expect(screen.getAllByTestId('chat-user')).toHaveLength(2));
    await new Promise(r => setTimeout(r, 50));
    expect(composer().value).toBe('');
    expect(screen.queryByTestId('chat-not-sent')).toBeNull();
    expect(screen.getAllByTestId('chat-user').map(e => e.textContent)).toEqual(['earlier', 'my words']);
    expect(calls('/cancel').map(c => c.body)).toEqual([{ send_id: sendId }]);
  });

  it('(c) after the timeout the late POST really arrives at the server and is refused with 409 Send withdrawn: no duplicate turn, the restored text stays', async () => {
    // A server that keeps what the cancel named, as the real one does.
    const withdrawn = new Set<string>();
    const { calls, late, sendId } = await timeOut(() => EARLIER, {
      [`POST ${BASE}/cancel`]: body => { withdrawn.add((body as { send_id: string }).send_id); return { status: 204 }; },
      [`POST ${BASE}/turns`]: body => (withdrawn.has((body as { send_id: string }).send_id)
        ? { status: 409, body: { title: 'Send withdrawn', detail: 'the send was stopped before the server accepted it' } }
        : { status: 202, body: { turn: 2 } }),
    });
    await waitFor(() => expect(screen.getByTestId('chat-not-sent')).toBeInTheDocument());
    await waitFor(() => expect(composer().value).toBe('my words'));
    // Now the aborted request is delivered, late.
    const res = await late();
    expect(res.status).toBe(409);
    expect(((await res.json()) as { title: string }).title).toBe('Send withdrawn');
    expect(calls('/turns').map(c => (c.body as { send_id: string }).send_id)).toEqual([sendId, sendId]);
    expect(screen.getAllByTestId('chat-user').map(e => e.textContent)).toEqual(['earlier']);
    expect(composer().value).toBe('my words');
    expect(screen.getByTestId('chat-not-sent').textContent).toBe('Not sent: no answerRetry');
  });

  it('a cancel that fails once is retried (500 ms later, same send_id); when it lands the text is restored as usual', async () => {
    let n = 0;
    const { calls, sendId } = await timeOut(() => EARLIER, {
      [`POST ${BASE}/cancel`]: () => (++n === 1 ? { status: 500, body: { detail: 'blip' } } : { status: 204 }),
    }, 500);
    await waitFor(() => expect(screen.getByTestId('chat-not-sent')).toBeInTheDocument());
    await waitFor(() => expect(composer().value).toBe('my words'));
    expect(calls('/cancel').map(c => c.body)).toEqual([{ send_id: sendId }, { send_id: sendId }]);
  });

  it('a cancel that keeps failing (1 try + 3 retries at 500, 1000, 2000 ms) does NOT restore the text as "Not sent": the POST may still land', async () => {
    const { calls, sendId } = await timeOut(() => EARLIER, {
      [`POST ${BASE}/cancel`]: { status: 500, body: { detail: 'cannot cancel' } },
    }, 500 + 1000 + 2000);
    await waitFor(() => expect(calls('/cancel')).toHaveLength(4));
    expect(calls('/cancel').map(c => c.body)).toEqual(Array(4).fill({ send_id: sendId }));
    await waitFor(() => expect(screen.getByTestId('chat-action-error').textContent).toContain('cannot cancel'));
    expect(screen.queryByTestId('chat-not-sent')).toBeNull();
    expect(composer().value).toBe('');
    // The send is still on screen as sending (not lost, not duplicated), and Stop can withdraw it again.
    expect(screen.getAllByTestId('chat-user').map(e => e.textContent)).toEqual(['earlier', 'my words']);
    expect(screen.getByTestId('chat-sending')).toBeInTheDocument();
  });
});

describe('the thinking placeholder', () => {
  const T0 = new Date('2026-11-06T10:00:00Z').getTime();
  const fake = () => vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval'], now: T0 });
  const tick = (ms: number) => act(async () => { await vi.advanceTimersByTimeAsync(ms); });

  it('the counter starts at ACCEPTANCE, not at Send: the seconds spent sending are not counted', async () => {
    fake();
    const { answer } = mount(snap([], { expected_first_token_ms: 0 }));
    await screen.findByTestId('chat-input');
    send('hello');
    await tick(3000); // three seconds in flight
    expect(screen.queryByTestId('chat-thinking')).toBeNull();
    await answer(202, { turn: 0 });
    expect(screen.getByTestId('chat-thinking-label').textContent).toBe('Thinking…');
    expect(screen.getByTestId('chat-thinking-counter').textContent).toBe('0s');
    await tick(4000);
    expect(screen.getByTestId('chat-thinking-counter').textContent).toBe('4s');
  });

  it('says "No response yet" only after the provider\'s expected first-token time (here 5000 ms), and goes grey', async () => {
    fake();
    const { answer } = mount(snap([], { expected_first_token_ms: 5000 }));
    await screen.findByTestId('chat-input');
    send('hello');
    await answer(202, { turn: 0 });
    await tick(4900);
    expect(screen.getByTestId('chat-thinking-label').textContent).toBe('Thinking…');
    expect(screen.getByTestId('chat-thinking').getAttribute('data-stalled')).toBeNull();
    await tick(2100); // 7s: past 5000
    expect(screen.getByTestId('chat-thinking-label').textContent).toBe('No response yet');
    expect(screen.getByTestId('chat-thinking-counter').textContent).toBe('7s');
    expect(screen.getByTestId('chat-thinking').getAttribute('data-stalled')).toBe('true');
  });

  it('streaming reasoning is a sign of life: past the expected time WITH thinking text it still says Thinking…, not No response yet', async () => {
    fake();
    const { answer } = mount(snap([], { expected_first_token_ms: 5000 }));
    await screen.findByTestId('chat-input');
    send('hello');
    await answer(202, { turn: 0 });
    const es = chatStream('chat-a')!;
    await tick(1000);
    act(() => { es.emit('thinking_delta', { text: 'Comparing blast radius…', turn: 0 }); });
    await tick(9000); // 10 s in, twice the expected 5000 ms
    expect(screen.getByTestId('chat-thinking-counter').textContent).toBe('10s');
    expect(screen.getByTestId('chat-thinking-label').textContent).toBe('Thinking…');
    expect(screen.getByTestId('chat-thinking').getAttribute('data-stalled')).toBeNull();
  });

  it('with no observation yet (0), it never says "No response yet", however long it waits', async () => {
    fake();
    const { answer } = mount(snap([], {}));
    await screen.findByTestId('chat-input');
    send('hello');
    await answer(202, { turn: 0 });
    await tick(600_000);
    expect(screen.getByTestId('chat-thinking-label').textContent).toBe('Thinking…');
    expect(screen.getByTestId('chat-thinking-counter').textContent).toBe('600s');
  });

  it('streams the reasoning summary under it and folds it away when the answer starts', async () => {
    const { answer } = mount(snap([]));
    await screen.findByTestId('chat-input');
    send('hello');
    await answer(202, { turn: 0 });
    const es = chatStream('chat-a')!;
    act(() => { es.emit('thinking_delta', { text: 'Comparing blast radius: ', turn: 0 }); es.emit('thinking_delta', { text: 'the registry reaches every tenant.', turn: 0 }); });
    expect(screen.getByTestId('chat-thinking-summary').textContent).toBe('Comparing blast radius: the registry reaches every tenant.');
    act(() => { es.emit('text_delta', { text: 'Answer', turn: 0 }); });
    expect(screen.queryByTestId('chat-thinking')).toBeNull();
    expect(screen.queryByTestId('chat-thinking-summary')).toBeNull();
    expect(screen.getByTestId('chat-text').textContent).toBe('Answer');
  });

  it('caps the streamed summary at four lines, scrolls inside it, and follows the newest line', async () => {
    const heights = vi.spyOn(HTMLElement.prototype, 'scrollHeight', 'get').mockReturnValue(500);
    try {
      const { answer } = mount(snap([]));
      await screen.findByTestId('chat-input');
      send('hello');
      await answer(202, { turn: 0 });
      const es = chatStream('chat-a')!;
      act(() => { es.emit('thinking_delta', { text: 'line one\nline two\nline three\n', turn: 0 }); });
      const box = screen.getByTestId('chat-thinking-summary');
      // 4 lines of 12.5px text at line-height 1.55 = 77.5px, scrolling inside.
      expect(box.style.maxHeight).toBe('77.5px');
      expect(box.style.overflowY).toBe('auto');
      expect(box.scrollTop).toBe(500);
      // New text arrives: it scrolls to the newest again.
      box.scrollTop = 10;
      act(() => { es.emit('thinking_delta', { text: 'line four\nline five', turn: 0 }); });
      expect(screen.getByTestId('chat-thinking-summary').scrollTop).toBe(500);
      // It still folds away when the answer starts.
      act(() => { es.emit('text_delta', { text: 'Answer', turn: 0 }); });
      expect(screen.queryByTestId('chat-thinking-summary')).toBeNull();
    } finally {
      heights.mockRestore();
    }
  });

  it('the first tool_call replaces the placeholder', async () => {
    const { answer } = mount(snap([]));
    await screen.findByTestId('chat-input');
    send('hello');
    await answer(202, { turn: 0 });
    expect(screen.getByTestId('chat-thinking')).toBeInTheDocument();
    act(() => { chatStream('chat-a')!.emit('tool_call', { id: 't1', name: 'knomit_repos', state: 'running', turn: 0 }); });
    expect(screen.queryByTestId('chat-thinking')).toBeNull();
    expect(screen.getByTestId('chat-tool-group')).toBeInTheDocument();
  });

  it('a page that joins a waiting turn mid-way counts from when it first saw it', async () => {
    fake();
    mount(snap([{ kind: 'user', text: 'q' }], { running: true }));
    await screen.findByTestId('chat-thinking');
    expect(screen.getByTestId('chat-thinking-counter').textContent).toBe('0s');
    await tick(2000);
    expect(screen.getByTestId('chat-thinking-counter').textContent).toBe('2s');
  });
});

describe('live tool detail', () => {
  it('a running query shows its title (no "Searching"); done adds the result size and duration', async () => {
    mount(snap([{ kind: 'user', text: 'q' }], { running: true }));
    await screen.findByTestId('chat-thinking');
    const es = chatStream('chat-a')!;
    act(() => { es.emit('tool_call', { id: 't1', name: 'knomit_query', title: 'Query', key_arg: 'OWASP Q3', state: 'running', started_ms: 1_000_000, arguments: { text: 'OWASP Q3', binding: 'h1' }, turn: 0 }); });
    expect(screen.getByTestId('chat-activity-summary').textContent).toBe('Query');
    expect(screen.getByTestId('chat-activity-steps').textContent).toBe('1 step');

    act(() => { es.emit('tool_call', { id: 't1', name: 'knomit_query', title: 'Query', key_arg: 'OWASP Q3', state: 'done', duration_ms: 1900, started_ms: 1_000_000, ended_ms: 1_001_900, summary: { count: 24 }, turn: 0 }); });
    expect(screen.getByTestId('chat-activity-steps').textContent).toBe('1 step · 1.9s');
    fireEvent.click(screen.getByTestId('chat-tool-group-toggle'));
    expect(screen.getByTestId('chat-tool-arg-t1').textContent).toBe('OWASP Q3');
    expect(screen.getByTestId('chat-tool-size-t1').textContent).toBe('24 results');
    expect(screen.getByTestId('chat-tool-time-t1').textContent).toBe('1.9s');
  });

  it('a snapshot carries the same: the result size and duration on the row, the WALL time in the line', async () => {
    mount(snap([
      { kind: 'user', text: 'q' },
      { kind: 'tool_call', id: 't1', name: 'knomit_query', state: 'done', arguments: { text: 'a' }, duration_ms: 1900, started_ms: 1_000_000, ended_ms: 1_001_900, summary: { count: 1 } },
      { kind: 'tool_call', id: 't2', name: 'knomit_query', state: 'done', arguments: { text: 'b' }, duration_ms: 22_000, started_ms: 1_001_000, ended_ms: 1_023_000, summary: { count: 24 } },
      { kind: 'assistant', text: 'done' },
    ]));
    await screen.findByTestId('chat-turn-0');
    // Overlapping calls: 1.9s + 22s summed would read 24s; first start to last end is 23s.
    expect(screen.getByTestId('chat-activity-steps').textContent).toBe('2 steps · 23s');
    fireEvent.click(screen.getByTestId('chat-tool-group-toggle'));
    expect(screen.getByTestId('chat-tool-size-t1').textContent).toBe('1 result');
    expect(screen.getByTestId('chat-tool-size-t2').textContent).toBe('24 results');
    expect(screen.getByTestId('chat-tool-time-t2').textContent).toBe('22s');
  });

  it('the line shows the wall time across a gap between calls, not the sum of the tools', async () => {
    mount(snap([
      { kind: 'user', text: 'q' },
      { kind: 'tool_call', id: 't1', name: 'knomit_query', state: 'done', arguments: {}, duration_ms: 1000, started_ms: 2_000_000, ended_ms: 2_001_000 },
      { kind: 'tool_call', id: 't2', name: 'knomit_query', state: 'done', arguments: {}, duration_ms: 1000, started_ms: 2_005_000, ended_ms: 2_006_000 },
      { kind: 'assistant', text: 'done' },
    ]));
    await screen.findByTestId('chat-turn-0');
    // The tools ran 2s in all; from the first start to the last end took 6s.
    expect(screen.getByTestId('chat-activity-steps').textContent).toBe('2 steps · 6s');
  });

  // SABOTAGE: drop the `!c.endedMs` check in wallDurationMs -> red (a running call would count as ended at 0).
  it('a group with a call still running shows no time yet, though an earlier call in it has finished', async () => {
    mount(snap([
      { kind: 'user', text: 'q' },
      { kind: 'tool_call', id: 't1', name: 'knomit_query', state: 'done', arguments: {}, duration_ms: 1000, started_ms: 3_000_000, ended_ms: 3_001_000 },
      { kind: 'tool_call', id: 't2', name: 'knomit_query', state: 'running', arguments: {}, started_ms: 3_002_000 },
    ], { running: true }));
    await screen.findByTestId('chat-turn-0');
    expect(screen.getByTestId('chat-activity-steps').textContent).toBe('2 steps');
    // ...and once the running call ends, the whole span shows.
    act(() => { chatStream('chat-a')!.emit('tool_call', { id: 't2', name: 'knomit_query', state: 'done', duration_ms: 1000, started_ms: 3_002_000, ended_ms: 3_009_000, turn: 0 }); });
    expect(screen.getByTestId('chat-activity-steps').textContent).toBe('2 steps · 9s');
  });

  it('a server that sends no timestamps shows no time', async () => {
    mount(snap([
      { kind: 'user', text: 'q' },
      { kind: 'tool_call', id: 't1', name: 'knomit_query', state: 'done', arguments: {}, duration_ms: 1000 },
      { kind: 'assistant', text: 'done' },
    ]));
    await screen.findByTestId('chat-turn-0');
    expect(screen.getByTestId('chat-activity-steps').textContent).toBe('1 step');
  });

  it('a call that did not run has no result size or duration', async () => {
    mount(snap([
      { kind: 'user', text: 'q' },
      { kind: 'tool_call', id: 't1', name: 'knomit_learn', state: 'done', is_error: true, arguments: {} },
      { kind: 'assistant', text: 'ok' },
    ]));
    await screen.findByTestId('chat-turn-0');
    fireEvent.click(screen.getByTestId('chat-tool-group-toggle'));
    expect(screen.queryByTestId('chat-tool-meta-t1')).toBeNull();
    expect(screen.getByTestId('chat-activity-steps').textContent).toBe('1 step');
  });
});

describe('notices: one line, never a filled box', () => {
  const PRICE = 'This chat is nearing 100,000 tokens. Past that, each message costs more. Start a new chat to save.';

  it('the price-tier line is the INFO tone, with the server\'s numbers: 95K of 100K, then the rest, then New chat', async () => {
    const { onNewChat } = mount(snap([
      { kind: 'user', text: 'q', warning: PRICE, warning_info: { kind: 'price_tier', prompt_tokens: 95_400, threshold: 95_000, tier_tokens: 100_000, mult: 5, model: 'claude-test-x' } },
      { kind: 'assistant', text: 'a' },
    ], { prompt_tokens: 95_400 }));
    await screen.findByTestId('chat-turn-0');
    const line = screen.getByTestId('chat-notice-price');
    expect(line.getAttribute('data-tone')).toBe('info');
    expect(line.textContent).toBe("95.4K of 100K·Haiku 5.5's higher price tier is next (5×)·New chat");
    expect(line.getAttribute('title')).toBe(PRICE);
    // "95.4K of 100K" is normal text, not a tone colour; the rest is footer grey.
    expect((screen.getByTestId('chat-notice-price-lead') as HTMLElement).style.color).toBe('rgb(212, 212, 212)');
    // No amber anywhere for the price tier: not on the line, the composer meter or the pill.
    expect(line.innerHTML).not.toMatch(/b8995a|184, 153, 90/i);
    for (const m of screen.getAllByTestId('context-meter')) expect((m.firstElementChild as HTMLElement).style.background).not.toMatch(/8a7444|138, 116, 68/i);
    expect(screen.getByTestId('context-meter').getAttribute('data-pct')).toBe('10');

    // New chat asks for a chat like this one (the app starts it EMPTY, with the same model).
    fireEvent.click(screen.getByTestId('chat-notice-price-action'));
    expect(onNewChat).toHaveBeenCalledTimes(1);
    expect(onNewChat).toHaveBeenCalledWith(CHAT);
  });

  it('a failed "New chat" is shown in the same one-line style with the existing "New chat hit an error", not swallowed', async () => {
    const onNewChat = vi.fn().mockRejectedValue(new Error('boom'));
    mount(snap([
      { kind: 'user', text: 'q', warning: PRICE, warning_info: { kind: 'price_tier', prompt_tokens: 95_400, tier_tokens: 100_000, mult: 5 } },
      { kind: 'assistant', text: 'a' },
    ]), { onNewChat });
    await screen.findByTestId('chat-turn-0');
    fireEvent.click(screen.getByTestId('chat-notice-price-action'));
    const line = await screen.findByTestId('chat-action-error');
    expect(onNewChat).toHaveBeenCalledWith(CHAT);
    expect(line.textContent).toBe('New chat hit an error');
    expect(line.getAttribute('data-tone')).toBe('blocked');
  });

  it('a blocked send reads "Chat is full · 1.02M > 1M, nothing sent · New chat", with the server sentence as the title', async () => {
    const sentence = "This conversation no longer fits the model's context window, so the request was not sent. Start a new chat to continue.";
    mount(snap([{ kind: 'user', text: 'q', error: sentence, error_info: { kind: 'context_full', needed: 1_020_000, window: 1_000_000 } }]));
    await screen.findByTestId('chat-turn-0');
    const line = screen.getByTestId('chat-notice-full');
    expect(line.getAttribute('data-tone')).toBe('blocked');
    expect(line.textContent).toBe('Chat is full·1.02M > 1M, nothing sent·New chat');
    expect(line.getAttribute('title')).toBe(sentence);
    // The figures are mono, as in the mockup's .num.
    expect(screen.getByTestId('chat-notice-full-rest')).toHaveStyle({ fontFamily: 'var(--k-font-mono)', fontSize: '10.5px' });
  });

  it('with only the window known (the provider refused), it still says nothing was sent', async () => {
    mount(snap([{ kind: 'user', text: 'q', error: 'full', error_info: { kind: 'context_full', needed: 0, window: 1_000_000 } }]));
    await screen.findByTestId('chat-turn-0');
    expect(screen.getByTestId('chat-notice-full').textContent).toBe('Chat is full·nothing sent·New chat');
  });

  it('a turn that ended in any other error is one blocked line carrying the server sentence', async () => {
    mount(snap([{ kind: 'user', text: 'q', error: 'The model declined this request.' }]));
    await screen.findByTestId('chat-turn-0');
    const line = screen.getByTestId('chat-turn-error');
    expect(line.getAttribute('data-tone')).toBe('blocked');
    expect(line.textContent).toBe('The model declined this request.');
  });

  it('no filled warning or error box remains anywhere in the chat', async () => {
    const { answer } = mount(snap([
      { kind: 'user', text: 'one', warning: PRICE, warning_info: { kind: 'price_tier', prompt_tokens: 96_000, tier_tokens: 100_000, mult: 5 } },
      { kind: 'assistant', text: 'a' },
      { kind: 'user', text: 'two', error: 'The model declined this request.' },
      { kind: 'user', text: 'three', error: 'full', error_info: { kind: 'context_full', needed: 2, window: 1 } },
    ]));
    await screen.findByTestId('chat-turn-2');
    send('four');
    await answer(409, { detail: 'a turn is running' });
    await screen.findByTestId('chat-not-sent');
    const filled = ['chat-notice-price', 'chat-turn-error', 'chat-notice-full', 'chat-not-sent'].flatMap(id => screen.getAllByTestId(id));
    expect(filled.length).toBeGreaterThanOrEqual(4);
    for (const el of filled) {
      expect(el.style.background).toBe('');
      expect(el.style.backgroundColor).toBe('');
      expect(el.style.border).toBe('');
    }
    // None of the old boxes' ground colours is used in the chat at all (the
    // Stop button is not on screen: nothing runs).
    const html = screen.getByTestId('chat-panel').innerHTML;
    expect(html).not.toMatch(/#2b1c1c|43, 28, 28|#2a200e|42, 32, 14|chat-warning/i);
  });

  it('a refused send never loses its text: it is back in the composer, one grey line says why, and Retry sends it again', async () => {
    const { calls, answer } = mount(snap([{ kind: 'user', text: 'first' }, { kind: 'assistant', text: 'a' }]));
    await screen.findByTestId('chat-turn-0');
    send('Retract only the ones in knomit-kb');
    await answer(409, { detail: 'the stopped turn is still finishing' });
    await waitFor(() => expect(composer().value).toBe('Retract only the ones in knomit-kb'));
    const line = screen.getByTestId('chat-not-sent');
    expect(line.textContent).toBe('Not sent: the stopped turn is still finishing (409)Retry');
    // No red border on the composer box.
    expect((composer().parentElement as HTMLElement).style.border).toBe('1px solid rgb(46, 46, 46)');
    expect(screen.queryAllByTestId('chat-user').map(e => e.textContent)).toEqual(['first']);

    fireEvent.click(screen.getByTestId('chat-retry'));
    await waitFor(() => expect(calls('/turns')).toHaveLength(2));
    expect((calls('/turns')[1].body as { text: string }).text).toBe('Retract only the ones in knomit-kb');
    expect(screen.queryByTestId('chat-not-sent')).toBeNull();
  });
});

describe('the reducer and helpers', () => {
  const sent = (at: number): ChatEvent[] => [{ kind: 'user', text: 'q', sendId: 1 }, { kind: 'send_accepted', sendId: 1, turn: 0, at }];

  it('acceptedAt is the acceptance time carried by send_accepted, absent before it', () => {
    const s0 = chatReducer(initialChatState, { kind: 'user', text: 'q', sendId: 1 });
    expect(s0.turns[0].acceptedAt).toBeUndefined();
    const s1 = chatReducer(s0, { kind: 'send_accepted', sendId: 1, turn: 0, at: 424242 });
    expect(s1.turns[0].acceptedAt).toBe(424242);
  });

  it('a snapshot keeps the acceptance time and the thinking so far of a turn it lists', () => {
    let s = sent(777).reduce(chatReducer, initialChatState);
    s = chatReducer(s, { kind: 'thinking_delta', text: 'hmm', turn: 0 });
    const next = snapshotState({ ...CHAT, running: true, grants: [], pending_approvals: [], items: [{ kind: 'user', text: 'q' }] }, s, [1]);
    expect(next.turns[0].acceptedAt).toBe(777);
    expect(next.turns[0].thinking).toBe('hmm');
  });

  it('thinking is kept only until the first part arrives', () => {
    let s = sent(1).reduce(chatReducer, initialChatState);
    s = chatReducer(s, { kind: 'thinking_delta', text: 'a', turn: 0 });
    s = chatReducer(s, { kind: 'text_delta', text: 'x', turn: 0 });
    s = chatReducer(s, { kind: 'thinking_delta', text: 'late', turn: 0 });
    expect(s.turns[0].thinking).toBe('a');
  });

  it('the context used is the snapshot\'s prompt_tokens; the expected first-token time rides on the snapshot too', () => {
    const s = snapshotState({ ...CHAT, running: false, grants: [], pending_approvals: [], items: [], prompt_tokens: 12_345, expected_first_token_ms: 8000 });
    expect(s.ctxTokens).toBe(12_345);
    expect(s.expectedFirstTokenMs).toBe(8000);
  });

  it('wording helpers', () => {
    expect([formatDuration(840), formatDuration(1900), formatDuration(22_000), formatDuration(72_000)]).toEqual(['840ms', '1.9s', '22s', '1m 12s']);
    expect([noticeCount(95_000), noticeCount(100_000), noticeCount(1_020_000), noticeCount(1_000_000), noticeCount(512)]).toEqual(['95K', '100K', '1.02M', '1M', '512']);
    expect(refusalReason('/api/v1/chats/c/turns → 409 a turn is running')).toBe('a turn is running');
    expect(refusalReason('/api/v1/chats/c/turns → no answer')).toBe('no answer');
  });
});
