import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, act, fireEvent, cleanup, within } from '@testing-library/react';
import { ChatFacet } from './ChatFacet';
import { ChatPill } from './ChatPill';
import { installFakeEventSource, uninstallFakeEventSource, latestStream } from './testEventSource';
import { stubFleetFetch } from './fleetTestFetch';
import type { ChatSnapshot, ChatViewItem } from './chatApi';
import { activitySummary, liveStep, activitySteps, costSession, toolVerb } from './chatCopy';
import { cachedPercent, compactCount, sessionCost, keyArg } from './chatState';
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

  it('words bind, query and read from the calls, joined with " · "', () => {
    expect(words([
      { kind: 'bind', target: 'all' },
      { kind: 'query', target: 'all' }, { kind: 'query', target: 'all' }, { kind: 'query', target: 'all' },
      { kind: 'read', target: 'all' }, { kind: 'read', target: 'all' },
    ])).toBe('Bound to all · searched 3× in all · read 2 facts');
  });
  it('a tool with no wording is counted as "used N tools"', () => {
    expect(words([{ kind: null, target: '' }])).toBe('Used 1 tool');
    expect(words([{ kind: null, target: '' }, { kind: null, target: '' }])).toBe('Used 2 tools');
    expect(words([{ kind: 'bind', target: 'all' }, { kind: null, target: '' }, { kind: null, target: '' }])).toBe('Bound to all · used 2 tools');
  });
  it('uses the singular for one: "read 1 fact", "used 1 tool", "Session (1 turn)", and the plural from two', () => {
    expect(words([{ kind: 'read', target: '' }])).toBe('Read 1 fact');
    expect(words([{ kind: 'read', target: '' }, { kind: 'read', target: '' }])).toBe('Read 2 facts');
    expect(words([{ kind: 'bind', target: 'all' }, { kind: null, target: '' }])).toBe('Bound to all · used 1 tool');
    expect(words([{ kind: null, target: '' }, { kind: null, target: '' }, { kind: null, target: '' }])).toBe('Used 3 tools');
    expect(costSession(1)).toBe('Session (1 turn)');
    expect(costSession(2)).toBe('Session (2 turns)');
    expect(costSession(0)).toBe('Session (0 turns)');
  });
  it('words a timeline row: Bound, Searched (Searching while running), Read; other tools have none', () => {
    expect([toolVerb('bind', false), toolVerb('query', false), toolVerb('query', true), toolVerb('read', false), toolVerb(null, false)])
      .toEqual(['Bound', 'Searched', 'Searching', 'Read', null]);
  });
  it('a bind with no readable target reads "Bound"', () => {
    expect(words([{ kind: 'bind', target: '' }])).toBe('Bound');
  });
  it('marks the target and the counts bold, nothing else', () => {
    const [bound, searched] = activitySummary([{ kind: 'bind', target: 'all' }, { kind: 'query', target: 'lens-x' }]);
    expect(bound.filter(s => s.strong).map(s => s.text)).toEqual(['all']);
    expect(searched.filter(s => s.strong).map(s => s.text)).toEqual(['1×']);
  });
  it('the live step names the target and the query; the step count is singular for one', () => {
    expect(liveStep('all', 'OWASP Q3').map(s => s.text).join('')).toBe('Searching all for "OWASP Q3"…');
    expect(activitySteps(1)).toBe('1 step');
    expect(activitySteps(9)).toBe('9 steps');
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
  const items = (): ChatViewItem[] => [
    { kind: 'user', text: 'bind to all' },
    call('tu_1', 'knomit_bind', { lens: 'all' }),
    call('tu_2', 'knomit_query', { text: 'AI agent security incident' }),
    call('tu_3', 'knomit_query', { text: 'agentic practice' }),
    call('tu_4', 'knomit_materialize', { handle: 'a1b2c3d4' }),
    { kind: 'assistant', text: 'Done.' },
  ];

  it('is collapsed by default, and says what the calls were', async () => {
    mount(snap(items()));
    await screen.findByTestId('chat-turn-0');
    expect(summaryText()).toBe('Bound to all · searched 2× in all · read 1 fact');
    expect(screen.getByTestId('chat-activity-steps').textContent).toBe('4 steps');
    expect(screen.getByTestId('chat-tool-group')).toHaveAttribute('data-open', 'false');
    expect(screen.queryByTestId('chat-timeline')).toBeNull();
  });

  it('expands into a timeline: one row per call with its key argument in mono', async () => {
    mount(snap(items()));
    await screen.findByTestId('chat-turn-0');
    fireEvent.click(screen.getByTestId('chat-tool-group-toggle'));
    expect(screen.getByTestId('chat-tool-group')).toHaveAttribute('data-open', 'true');
    expect(['tu_1', 'tu_2', 'tu_3', 'tu_4'].map(id => screen.getByTestId(`chat-tool-arg-${id}`).textContent))
      .toEqual(['all', 'AI agent security incident', 'agentic practice', 'a1b2c3d4']);
    // The verbs: Bound, Searched, Searched, Read.
    expect(['tu_1', 'tu_2', 'tu_3', 'tu_4'].map(id => screen.getByTestId(`chat-tool-select-${id}`).firstElementChild?.textContent))
      .toEqual(['Bound', 'Searched', 'Searched', 'Read']);
  });

  it('a tool with no wording reads "used N tools"', async () => {
    mount(snap([{ kind: 'user', text: 'q' }, call('tu_1', 'knomit_repos', {}), call('tu_2', 'knomit_changes', {}), { kind: 'assistant', text: 'ok' }]));
    await screen.findByTestId('chat-turn-0');
    expect(summaryText()).toBe('Used 2 tools');
  });

  it('a call waiting for approval forces the group open', async () => {
    mount(snap([{ kind: 'user', text: 'learn' }, call('tu_1', 'knomit_learn', { facts: [] }, 'running')], {
      running: true,
      pending_approvals: [{ call_id: 'tu_1', tool: 'knomit_learn', action: 'learn', target: 'alpha', target_kind: 'repo', destructive: false, grantable: true, turn: 0 }],
    }));
    await screen.findByTestId('chat-approval-tu_1');
    expect(screen.getByTestId('chat-tool-group')).toHaveAttribute('data-open', 'true');
  });

  it('while running, the collapsed line shows the live step in amber with a Stop pill that stops', async () => {
    const { fx } = mount(snap([
      { kind: 'user', text: 'q' }, call('tu_1', 'knomit_bind', { lens: 'all' }), call('tu_2', 'knomit_query', { text: 'OWASP Q3' }, 'running'),
    ], { running: true }));
    const live = await screen.findByTestId('chat-activity-live');
    expect(live.textContent).toBe('Searching all for "OWASP Q3"…');
    expect(live).toHaveStyle({ color: '#e8c27a' });
    fx.calls.length = 0;
    fireEvent.click(screen.getByTestId('chat-activity-stop'));
    expect(fx.calls.map(c => c.route)).toEqual([`POST ${BASE}/cancel`]);
  });

  it('a live call without arguments shows no key argument, and falls back to the summary', async () => {
    mount(snap([{ kind: 'user', text: 'q' }], { running: true }));
    await screen.findByTestId('chat-turn-0');
    act(() => { latestStream('/chats/chat-a/events')!.emit('tool_call', { id: 'tu_9', name: 'knomit_query', state: 'running', turn: 0 }); });
    expect(screen.queryByTestId('chat-activity-live')).toBeNull();
    expect(summaryText()).toBe('Searched 1×');
    fireEvent.click(screen.getByTestId('chat-tool-group-toggle'));
    expect(screen.queryByTestId('chat-tool-arg-tu_9')).toBeNull();
  });

  it('keyArg is the query text, the bind target or the read handle', () => {
    expect(keyArg({ id: 'a', name: 'knomit_query', state: 'done', isError: false, args: { text: 'x' } })).toBe('x');
    expect(keyArg({ id: 'a', name: 'knomit_bind', state: 'done', isError: false, args: { repo: 'r' } })).toBe('r');
    expect(keyArg({ id: 'a', name: 'knomit_repos', state: 'done', isError: false, args: {} })).toBe('');
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
    ]), { [`GET ${BASE}/context`]: { body: { window: 1_000_000, how: 'config', spill: false } } });
    await screen.findByTestId('chat-turn-1');
    await screen.findByText('68K / 1M');
    expect(screen.getByTestId('chat-pill-session').textContent).toBe('session $0.0212');
    // The latest turn's prompt: 2000 + 60000 + 6000.
    expect(screen.getByTestId('chat-pill-context').textContent).toBe('context68K');
    expect(screen.getByTestId('chat-context-text').textContent).toBe('68K / 1M');
    expect(screen.getAllByTestId('context-meter').map(m => m.getAttribute('data-pct'))).toEqual(['7', '7']);
  });

  it('the context used is the latest request\'s prompt, kept across the snapshot read at the turn\'s end; a snapshot alone averages the summed turn over its requests', async () => {
    // Joined mid-way: no live event yet. The turn's summed prompt (3 requests: 2 tool calls + 1) is averaged.
    mount(snap([
      { kind: 'user', text: 'q', usage: { input: 3000, cache_read: 15000, cache_write: 0, output: 9 } },
      call('tu_1', 'knomit_repos', {}), call('tu_2', 'knomit_repos', {}), { kind: 'assistant', text: 'A' },
    ], { usage_seq: 1 }), { [`GET ${BASE}/context`]: { body: { window: 32768, spill: false } } });
    await screen.findByTestId('chat-turn-0');
    expect(screen.getByTestId('chat-context-text').textContent).toBe('6K / 32.8K');
    // A live request lands (seq 2): its exact prompt is shown, and survives the read at the turn's end.
    const es = latestStream('/chats/chat-a/events')!;
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
    const es = latestStream('/chats/chat-a/events')!;
    act(() => { es.emit('text_delta', { text: 'Short answer', turn: 0 }); });
    const text = screen.getByTestId('chat-text');
    expect(within(text).getByTestId('chat-caret')).toBeInTheDocument();
    expect(text.lastElementChild).toBe(within(text).getByTestId('chat-caret'));
    act(() => { es.emit('turn_done', { turn: 0 }); });
    expect(screen.queryByTestId('chat-caret')).toBeNull();
  });
  it('is not drawn on a finished turn', async () => {
    mount(snap([{ kind: 'user', text: 'q' }, { kind: 'assistant', text: 'A' }]));
    await screen.findByTestId('chat-turn-0');
    expect(screen.queryByTestId('chat-caret')).toBeNull();
  });
});
