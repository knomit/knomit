import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, act, fireEvent, cleanup, within } from '@testing-library/react';
import { ChatFacet } from './ChatFacet';
import { ChatPill } from './ChatPill';
import { installFakeEventSource, uninstallFakeEventSource, chatStream } from './testEventSource';
import { stubFleetFetch } from './fleetTestFetch';
import type { ChatSnapshot, ChatViewItem } from './chatApi';
import { activitySummary, activitySteps, costSession } from './chatCopy';
import { cachedPercent, compactCount, sessionCost } from './chatState';
import type { ChatTurn } from './chatState';

// Chat UX v2, PR 1 (section 2: layout, activity line, turn footer, streaming
// caret, composer). Each test asserts WHICH value, not that something shows.

const CHAT = { id: 'chat-a', provider: 'anthropic', model: 'claude-test-x' };
const BASE = '/api/v1/chats/chat-a';

const call = (id: string, name: string, args: unknown, state: 'running' | 'done' = 'done'): ChatViewItem =>
  ({ kind: 'tool_call', id, name, state, arguments: args });

const snap = (items: ChatViewItem[], over: Partial<ChatSnapshot> = {}): ChatSnapshot =>
  ({ ...CHAT, running: false, grants: [], pending_approvals: [], items, ...over });

function mount(s: ChatSnapshot, extra: Record<string, Parameters<typeof stubFleetFetch>[0][string]> = {}) {
  const fx = stubFleetFetch({ [`GET ${BASE}`]: { body: s }, ...extra });
  const utils = render(<><ChatPill chatId="chat-a" /><ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} /></>);
  return { fx, ...utils };
}

beforeEach(() => { installFakeEventSource(); });
afterEach(() => { cleanup(); uninstallFakeEventSource(); vi.unstubAllGlobals(); });

const summaryText = () => screen.getByTestId('chat-activity-summary').textContent;

describe('the activity summary (chatCopy.activitySummary)', () => {
  const words = (calls: Parameters<typeof activitySummary>[0]) =>
    activitySummary(calls).map(p => p.map(s => s.text).join('')).join(' · ');

  it('names each call by its title, counted per title and target, joined with " · "', () => {
    expect(words([
      { title: 'Bind', target: 'all', bind: true },
      { title: 'Query', target: 'all' }, { title: 'Query', target: 'all' }, { title: 'Query', target: 'all' },
      { title: 'Materialize', target: 'all' }, { title: 'Materialize', target: 'all' },
    ])).toBe('Bind all · Query 3× in all · Materialize 2× in all');
  });
  it('one call has no count; a call with no target has no " in "; an untitled tool is its name', () => {
    expect(words([{ title: 'Query', target: 'all' }])).toBe('Query in all');
    expect(words([{ title: 'List repos', target: '' }, { title: 'List repos', target: '' }])).toBe('List repos 2×');
    expect(words([{ title: 'acme_frobnicate', target: '' }])).toBe('acme_frobnicate');
    // The same title in two targets is two pieces, in the order they first appear.
    expect(words([{ title: 'Query', target: 'a' }, { title: 'Query', target: 'b' }, { title: 'Query', target: 'a' }])).toBe('Query 2× in a · Query in b');
  });
  it('the singular and plural step count, and the session label', () => {
    expect(activitySteps(1)).toBe('1 step');
    expect(activitySteps(9)).toBe('9 steps');
    expect(costSession(1)).toBe('Session (1 turn)');
    expect(costSession(2)).toBe('Session (2 turns)');
    expect(costSession(0)).toBe('Session (0 turns)');
  });
  it('a bind with no readable target reads as its title alone, and only when no bind named one', () => {
    expect(words([{ title: 'Bind', target: '', bind: true }])).toBe('Bind');
    expect(words([{ title: 'Bind', target: 'all', bind: true }, { title: 'Query', target: 'all' }, { title: 'Bind', target: 'all', bind: true }])).toBe('Bind all 2× · Query in all');
    expect(words([{ title: 'Bind', target: '', bind: true }, { title: 'Bind', target: '', bind: true }])).toBe('Bind 2×');
    // ...never beside a bind that did name one: "Bind · Bind all" says it twice.
    expect(words([{ title: 'Bind', target: '', bind: true }, { title: 'Bind', target: 'all', bind: true }])).toBe('Bind all');
  });
  it('a failed bind is not a bind: it says nothing, and the line is what the other calls say', () => {
    expect(words([{ title: 'Bind', target: 'nosuch', bind: true, failed: true }, { title: 'Query', target: 'all' }])).toBe('Query in all');
    expect(words([{ title: 'Bind', target: 'all', bind: true }, { title: 'Bind', target: 'nosuch', bind: true, failed: true }])).toBe('Bind all');
    expect(words([{ title: 'Bind', target: '', bind: true, failed: true }, { title: 'Explain', target: '' }])).toBe('Explain');
    // A failed call of any other kind is counted like any other: no word says it failed.
    expect(words([{ title: 'Query', target: 'all', failed: true }])).toBe('Query in all');
  });
  it('marks a bind\'s target and the counts bold, nothing else', () => {
    const [bound, searched] = activitySummary([{ title: 'Bind', target: 'all', bind: true }, { title: 'Query', target: 'lens-x' }, { title: 'Query', target: 'lens-x' }]);
    expect(bound.filter(s => s.strong).map(s => s.text)).toEqual(['all']);
    expect(searched.filter(s => s.strong).map(s => s.text)).toEqual(['2×']);
  });
});

describe('the cache share and compact counts', () => {
  it('cached % is cache_read / (input + cache_read + cache_write), rounded', () => {
    expect(cachedPercent({ input: 612, cacheRead: 67572, cacheWrite: 2401, output: 132 })).toBe(96);
    expect(cachedPercent({ input: 11, cacheRead: 22, cacheWrite: 33, output: 44 })).toBe(33);
    expect(cachedPercent({ input: 100, cacheRead: 0, cacheWrite: 0, output: 5 })).toBe(0);
    expect(cachedPercent({ input: 0, cacheRead: 0, cacheWrite: 0, output: 5 })).toBe(0);
  });
  it('words counts compactly', () => {
    expect([950, 2400, 68000, 1100000, 1020000].map(compactCount)).toEqual(['950', '2.4K', '68K', '1.1M', '1M']);
  });
  it('the session cost is the sum of the priced turns, null when none was priced', () => {
    const t = (costUsd: number | null) => ({ costUsd } as ChatTurn);
    expect(sessionCost([t(0.25), t(null), t(0.5)])).toBe(0.75);
    expect(sessionCost([t(null)])).toBeNull();
  });
});

describe('the activity line on the page', () => {
  // Rows as the server sends them: each tool call carries its tool's title and main argument.
  const items = (): ChatViewItem[] => [
    { kind: 'user', text: 'bind to all' },
    { ...call('tu_1', 'knomit_bind', { lens: 'all' }), title: 'Bind', key_arg: 'all' },
    { ...call('tu_2', 'knomit_query', { binding: 'h', text: 'AI agent security incident' }), title: 'Query', key_arg: 'AI agent security incident', in_base: true },
    { ...call('tu_3', 'knomit_query', { binding: 'h', text: 'agentic practice' }), title: 'Query', key_arg: 'agentic practice', in_base: true },
    { ...call('tu_4', 'knomit_materialize', { binding: 'h', file: 'kb/a.md' }), title: 'Materialize', key_arg: 'kb/a.md', in_base: true },
    { kind: 'assistant', text: 'Done.' },
  ];

  it('is collapsed by default, and says what the calls were', async () => {
    mount(snap(items()));
    await screen.findByTestId('chat-turn-0');
    expect(summaryText()).toBe('Bind all · Query 2× in all · Materialize in all');
    expect(screen.getByTestId('chat-activity-steps').textContent).toBe('4 steps');
    expect(screen.getByTestId('chat-tool-group')).toHaveAttribute('data-open', 'false');
    expect(screen.queryByTestId('chat-timeline')).toBeNull();
  });

  it('expands into a timeline: one row per call, its title and its main argument in mono', async () => {
    mount(snap(items()));
    await screen.findByTestId('chat-turn-0');
    fireEvent.click(screen.getByTestId('chat-tool-group-toggle'));
    expect(screen.getByTestId('chat-tool-group')).toHaveAttribute('data-open', 'true');
    expect(['tu_1', 'tu_2', 'tu_3', 'tu_4'].map(id => screen.getByTestId(`chat-tool-arg-${id}`).textContent))
      .toEqual(['all', 'AI agent security incident', 'agentic practice', 'kb/a.md']);
    expect(['tu_1', 'tu_2', 'tu_3', 'tu_4'].map(id => screen.getByTestId(`chat-tool-select-${id}`).querySelector('[data-call-part="label"]')?.textContent))
      .toEqual(['Bind', 'Query', 'Query', 'Materialize']);
  });

  it('tools with no title are named by their raw names', async () => {
    mount(snap([{ kind: 'user', text: 'q' }, call('tu_1', 'acme_one', {}), call('tu_2', 'acme_two', {}), { kind: 'assistant', text: 'ok' }]));
    await screen.findByTestId('chat-turn-0');
    expect(summaryText()).toBe('acme_one · acme_two');
  });

  it('a call waiting for approval forces the group open', async () => {
    mount(snap([{ kind: 'user', text: 'learn' }, call('tu_1', 'knomit_learn', { facts: [] }, 'running')], {
      running: true,
      pending_approvals: [{ call_id: 'tu_1', tool: 'knomit_learn', action: 'learn', target: 'alpha', target_kind: 'repo', destructive: false, grantable: true, turn: 0 }],
    }));
    await screen.findByTestId('chat-approval-tu_1');
    expect(screen.getByTestId('chat-tool-group')).toHaveAttribute('data-open', 'true');
  });

  it('while running, the collapsed line is the same summary drawn amber, with a Stop pill that stops', async () => {
    const { fx } = mount(snap([
      { kind: 'user', text: 'q' },
      { ...call('tu_1', 'knomit_bind', { lens: 'all' }), title: 'Bind', key_arg: 'all' },
      { ...call('tu_2', 'knomit_query', { binding: 'h', text: 'OWASP Q3' }, 'running'), title: 'Query', key_arg: 'OWASP Q3', in_base: true },
    ], { running: true }));
    await screen.findByTestId('chat-activity-stop');
    expect(summaryText()).toBe('Bind all · Query in all');
    expect(screen.getByTestId('chat-activity-summary')).toHaveStyle({ color: '#e8c27a' });
    fx.calls.length = 0;
    fireEvent.click(screen.getByTestId('chat-activity-stop'));
    expect(fx.calls.map(c => c.route)).toEqual([`POST ${BASE}/cancel`]);
  });

  it('a live call without arguments shows no main argument and no target', async () => {
    mount(snap([{ kind: 'user', text: 'q' }], { running: true }));
    await screen.findByTestId('chat-turn-0');
    act(() => { chatStream('chat-a')!.emit('tool_call', { id: 'tu_9', name: 'knomit_query', title: 'Query', state: 'running', turn: 0 }); });
    expect(summaryText()).toBe('Query');
    fireEvent.click(screen.getByTestId('chat-tool-group-toggle'));
    expect(screen.queryByTestId('chat-tool-arg-tu_9')).toBeNull();
  });
});

describe('the turn footer and its hover card', () => {
  const turns = (): ChatViewItem[] => [
    { kind: 'user', text: 'one', usage: { input: 612, cache_read: 67572, cache_write: 2401, output: 132 }, cost_usd: 0.0012 },
    { kind: 'assistant', text: 'A' },
    { kind: 'user', text: 'two', usage: { input: 2400, cache_read: 0, cache_write: 0, output: 1100 }, cost_usd: 0.0042 },
    { kind: 'assistant', text: 'B' },
  ];

  it('shows the cost, the compact in/out, and the cache share, and no raw four counts', async () => {
    mount(snap(turns()));
    await screen.findByTestId('chat-turn-1');
    const f0 = within(screen.getByTestId('chat-turn-0'));
    expect(f0.getByTestId('chat-cost').textContent).toBe('$0.0012');
    expect(f0.getByTestId('chat-tokens').textContent).toBe('612 in · 132 out');
    expect(f0.getByTestId('chat-cached').textContent).toBe('96% cached');
    expect(screen.getByTestId('chat-turn-0').textContent).not.toMatch(/cache write \d/);
    const f1 = within(screen.getByTestId('chat-turn-1'));
    expect(f1.getByTestId('chat-tokens').textContent).toBe('2.4K in · 1.1K out');
    expect(f1.getByTestId('chat-cached').textContent).toBe('0% cached');
  });

  it('the hover card lists the four counts, and its totals add up: this turn is the footer cost, the session the sum', async () => {
    mount(snap(turns()));
    await screen.findByTestId('chat-turn-1');
    const t0 = within(screen.getByTestId('chat-turn-0'));
    expect(t0.queryByTestId('chat-cost-card')).toBeNull();
    fireEvent.mouseEnter(t0.getByTestId('chat-usage'));
    const card = within(t0.getByTestId('chat-cost-card'));
    expect(['input', 'cache-read', 'cache-write', 'output'].map(k => card.getByTestId(`chat-cost-tokens-${k}`).textContent))
      .toEqual(['612', '67,572', '2,401', '132']);
    expect(card.getByTestId('chat-cost-turn').textContent).toBe(t0.getByTestId('chat-cost').textContent);
    expect(card.getByTestId('chat-cost-turn').textContent).toBe('$0.0012');
    // 0.0012 + 0.0042, over two turns.
    expect(card.getByTestId('chat-cost-session').textContent).toBe('$0.0054');
    expect(t0.getByTestId('chat-cost-card')).toHaveTextContent('Session (2 turns)');
    fireEvent.mouseLeave(t0.getByTestId('chat-usage'));
    expect(t0.queryByTestId('chat-cost-card')).toBeNull();
  });

  it('copies the answer', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal('navigator', { clipboard: { writeText } });
    mount(snap(turns()));
    await screen.findByTestId('chat-turn-1');
    fireEvent.click(within(screen.getByTestId('chat-turn-0')).getByTestId('chat-copy-answer'));
    expect(writeText).toHaveBeenCalledWith('A');
  });
});

describe('the top-bar pill and the composer meter', () => {
  it('the pill shows the summed turn costs and the context used; the composer shows used / window', async () => {
    mount(snap([
      { kind: 'user', text: 'one', usage: { input: 612, cache_read: 67572, cache_write: 2401, output: 132 }, cost_usd: 0.0012 },
      { kind: 'assistant', text: 'A' },
      { kind: 'user', text: 'two', usage: { input: 2000, cache_read: 60000, cache_write: 6000, output: 9 }, cost_usd: 0.02 },
      { kind: 'assistant', text: 'B' },
    ], { usage_seq: 2, prompt_tokens: 68000 }), { [`GET ${BASE}/context`]: { body: { window: 1_000_000, how: 'config', spill: false } } });
    await screen.findByTestId('chat-turn-1');
    await screen.findByText('68K / 1M');
    expect(screen.getByTestId('chat-pill-session').textContent).toBe('session $0.0212');
    // The server's own figure for the last request's prompt (2000 + 60000 + 6000), not a sum or an estimate.
    expect(screen.getByTestId('chat-pill-context').textContent).toBe('context68K');
    expect(screen.getByTestId('chat-context-text').textContent).toBe('68K / 1M');
    expect(screen.getAllByTestId('context-meter').map(m => m.getAttribute('data-pct'))).toEqual(['7', '7']);
  });

  it('the context used is the server\'s last-request prompt, and a newer live request survives the snapshot read at the turn\'s end', async () => {
    // Joined mid-way: no live event yet. The snapshot's prompt_tokens is shown as it is (never averaged from the summed usage).
    mount(snap([
      { kind: 'user', text: 'q', usage: { input: 3000, cache_read: 15000, cache_write: 0, output: 9 } },
      call('tu_1', 'knomit_repos', {}), call('tu_2', 'knomit_repos', {}), { kind: 'assistant', text: 'A' },
    ], { usage_seq: 1, prompt_tokens: 7300 }), { [`GET ${BASE}/context`]: { body: { window: 32768, spill: false } } });
    await screen.findByTestId('chat-turn-0');
    expect(screen.getByTestId('chat-context-text').textContent).toBe('7.3K / 32.8K');
    // A live request lands (seq 2): its exact prompt is shown, and survives the read at the turn's end.
    const es = chatStream('chat-a')!;
    act(() => { es.emit('usage', { usage: { input: 700, cache_read: 8000, cache_write: 300, output: 5 }, seq: 2, turn: 0 }); });
    expect(screen.getByTestId('chat-context-text').textContent).toBe('9K / 32.8K');
    await act(async () => { es.emit('turn_done', { turn: 0 }); });
    expect(screen.getByTestId('chat-context-text').textContent).toBe('9K / 32.8K');
  });

  it('the placeholder is Reply… once there is a turn', async () => {
    mount(snap([{ kind: 'user', text: 'one' }, { kind: 'assistant', text: 'A' }]));
    await screen.findByTestId('chat-turn-0');
    expect(screen.getByTestId('chat-input')).toHaveAttribute('placeholder', 'Reply…');
  });

  it('the composer says Enter to send, and Send is the round arrow, active only with text', async () => {
    mount(snap([]));
    await screen.findByTestId('chat-input');
    expect(screen.getByTestId('chat-input')).toHaveAttribute('placeholder', 'Message');
    expect(screen.getByTestId('chat-composer-row')).toHaveTextContent('Enter to send');
    expect(screen.getByTestId('chat-send')).toBeDisabled();
    fireEvent.change(screen.getByTestId('chat-input'), { target: { value: 'hi' } });
    expect(screen.getByTestId('chat-send')).not.toBeDisabled();
  });
});

describe('the user message and the answer', () => {
  it('the user message is a strip, not a blockquote; a quote in an answer is a real blockquote inside the answer', async () => {
    mount(snap([
      { kind: 'user', text: 'what does the spec say?' },
      { kind: 'assistant', text: 'It says:\n\n> be careful\n\nDone.' },
    ]));
    await screen.findByTestId('chat-turn-0');
    const user = screen.getByTestId('chat-user');
    expect(user.tagName).toBe('DIV');
    expect(user.className).toContain('k-chat-user');
    expect(user.closest('blockquote')).toBeNull();
    expect(user.querySelector('blockquote')).toBeNull();
    expect(user.textContent).toBe('what does the spec say?\n'.trimEnd());
    const answer = screen.getByTestId('chat-text');
    expect(answer.className).toContain('k-chat-answer');
    const quote = answer.querySelector('blockquote');
    expect(quote?.textContent?.trim()).toBe('be careful');
    expect(quote?.closest('.k-chat-user')).toBeNull();
  });

  it('a table sits in its own horizontal scroll box, so a wide one is not cut off beside the side panel', async () => {
    mount(snap([
      { kind: 'user', text: 'x' },
      { kind: 'assistant', text: '| a | b |\n|---|---|\n| 1 | 2 |' },
    ]));
    await screen.findByTestId('chat-turn-0');
    const box = screen.getByTestId('chat-table-scroll');
    expect(box.className).toBe('k-chat-table-scroll');
    expect(box.children).toHaveLength(1);
    expect(box.firstElementChild?.tagName).toBe('TABLE');
    expect(box.closest('.k-chat-answer')).not.toBeNull();
  });

  it('a bare number in a table cell gets a bar scaled to its column maximum', async () => {
    mount(snap([
      { kind: 'user', text: 'rank' },
      { kind: 'assistant', text: '| # | name | score |\n|---|---|---|\n| 1 | a | ~5 |\n| 2 | b | 2.5 |\n| 3 | c | n/a |' },
    ]));
    await screen.findByTestId('chat-turn-0');
    const bars = screen.getAllByTestId('chat-cell-bar');
    // Row by row: # 1 of 3, score ~5 of 5; # 2 of 3, score 2.5 of 5; # 3 of 3, no bar for "n/a".
    expect(bars.map(b => Number(b.getAttribute('data-share')).toFixed(2))).toEqual(['0.33', '1.00', '0.67', '0.50', '1.00']);
  });

  it('inline code stays plain code', async () => {
    mount(snap([{ kind: 'user', text: 'q' }, { kind: 'assistant', text: 'See `3fc77970`.' }]));
    await screen.findByTestId('chat-turn-0');
    expect(screen.getByTestId('chat-text').querySelector('code')?.textContent).toBe('3fc77970');
  });
});

describe('the streaming caret', () => {
  it('ends the streamed text until the turn ends', async () => {
    mount(snap([{ kind: 'user', text: 'q' }], { running: true }));
    await screen.findByTestId('chat-turn-0');
    const es = chatStream('chat-a')!;
    act(() => { es.emit('text_delta', { text: 'Short answer', turn: 0 }); });
    const text = screen.getByTestId('chat-text');
    expect(within(text).getByTestId('chat-caret')).toBeInTheDocument();
    // Inline: the last child of the last paragraph, on the same line as the last word.
    expect(text.querySelector('p')!.lastElementChild).toBe(within(text).getByTestId('chat-caret'));
    act(() => { es.emit('turn_done', { turn: 0 }); });
    expect(screen.queryByTestId('chat-caret')).toBeNull();
  });
  it('is not drawn on a finished turn', async () => {
    mount(snap([{ kind: 'user', text: 'q' }, { kind: 'assistant', text: 'A' }]));
    await screen.findByTestId('chat-turn-0');
    expect(screen.queryByTestId('chat-caret')).toBeNull();
  });
});

describe('the activity line on the page after a failed bind', () => {
  const failedBind = (id: string, name: string): ChatViewItem => ({ ...call(id, 'knomit_bind', { name }), is_error: true });

  it('a group of only failed binds has no summary at all: no empty span, no stray separator', async () => {
    mount(snap([{ kind: 'user', text: 'q' }, failedBind('tu_1', 'nosuch-kb'), { kind: 'assistant', text: 'ok' }]));
    const toggle = await screen.findByTestId('chat-tool-group-toggle');
    expect(screen.queryByTestId('chat-activity-summary')).toBeNull();
    expect(toggle.textContent).toBe('1 step›');
    expect(toggle.textContent).not.toContain('·');
  });

  it('a failed bind in one group leaves the next group on the binding before it', async () => {
    mount(snap([
      { kind: 'user', text: 'q' },
      { ...call('tu_1', 'knomit_bind', { name: 'alpha' }), title: 'Bind' }, { ...failedBind('tu_2', 'nosuch-kb'), title: 'Bind' },
      { kind: 'assistant', text: 'then' },
      { ...call('tu_3', 'knomit_query', { binding: 'h', text: 'x' }), title: 'Query', in_base: true },
      { kind: 'assistant', text: 'done' },
    ]));
    await screen.findAllByTestId('chat-tool-group');
    const lines = screen.getAllByTestId('chat-activity-summary').map(e => e.textContent);
    expect(lines).toEqual(['Bind alpha', 'Query in alpha']);
  });
});
