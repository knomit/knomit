import { describe, it, expect } from 'vitest';
import { chatReducer, initialChatState, toolCallLabel, usageLine, formatCost, snapshotState, boundAfter, boundBeforeCall, sessionCost } from './chatState';
import type { ChatEvent, ChatState, ChatTurn, ToolCall } from './chatState';
import type { ChatApproval, ChatSnapshot, ChatViewItem } from './chatApi';

const run = (events: ChatEvent[], from: ChatState = initialChatState): ChatState => events.reduce(chatReducer, from);
const call = (id: string, name: string, turn: number, state: 'running' | 'done' = 'running'): ChatEvent => ({ kind: 'tool_call', id, name, state, turn });
const text = (t: string, turn: number): ChatEvent => ({ kind: 'text_delta', text: t, turn });
/** A message sent from here whose POST the server accepted: turn `turn` in its view. */
const sent = (msg: string, sendId: number, turn: number): ChatEvent[] => [{ kind: 'user', text: msg, sendId }, { kind: 'send_accepted', sendId, turn, at: 1000 }];
const snap = (items: ChatViewItem[], over: Partial<ChatSnapshot> = {}): ChatSnapshot => ({
  id: 'c', provider: 'p', model: 'm', running: false, grants: [], pending_approvals: [], items, ...over,
});
const ready = (items: ChatViewItem[], over: Partial<ChatSnapshot> = {}, confirmed: number[] = []): ChatEvent =>
  ({ kind: 'ready', snapshot: snap(items, over), confirmed });
const u = (msg: string): ChatViewItem => ({ kind: 'user', text: msg });
const approval = (callId: string, turn: number): ChatApproval =>
  ({ call_id: callId, tool: 'knomit_learn', action: 'learn', target: 'alpha', target_kind: 'repo', destructive: false, grantable: true, turn });
const usage1 = { input: 1, cache_read: 2, cache_write: 3, output: 4 };

describe('adaptive chats: the loader call', () => {
  it('the loader call keeps the tools it loaded, from the event and from the snapshot', () => {
    const live = run([...sent('q', 1, 0), call('tu_1', 'load_tools', 0),
      { kind: 'tool_call', id: 'tu_1', name: 'load_tools', state: 'done', loaded: ['knomit_query', 'knomit_explain'], turn: 0 }]);
    const g = live.turns[0].parts[0];
    expect(g.kind === 'group' && g.calls[0].loaded).toEqual(['knomit_query', 'knomit_explain']);
    const fromSnap = run([ready([u('q'), { kind: 'tool_call', id: 'tu_1', name: 'load_tools', state: 'done', loaded: ['knomit_query'] }])]);
    const g2 = fromSnap.turns[0].parts[0];
    expect(g2.kind === 'group' && g2.calls[0].loaded).toEqual(['knomit_query']);
  });
});

describe('chatReducer: live events are routed by the turn index they name', () => {
  it('groups consecutive tool calls into one group', () => {
    const s = run([...sent('q', 1, 0), text('looking', 0), call('tu_1', 'knomit_repos', 0), call('tu_2', 'knomit_query', 0), text('found it', 0)]);
    expect(s.turns).toHaveLength(1);
    const parts = s.turns[0].parts;
    expect(parts.map(p => p.kind)).toEqual(['text', 'group', 'text']);
    const group = parts[1];
    expect(group.kind === 'group' && group.calls.map(c => c.id)).toEqual(['tu_1', 'tu_2']);
  });

  it('streams text deltas into one text part', () => {
    const s = run([...sent('q', 1, 0), text('he', 0), text('llo', 0)]);
    expect(s.turns[0].parts).toEqual([{ kind: 'text', text: 'hello' }]);
  });

  it('a done event updates the call it names, not a new row', () => {
    const s = run([...sent('q', 1, 0), call('tu_1', 'knomit_repos', 0), { kind: 'tool_call', id: 'tu_1', name: 'knomit_repos', state: 'done', is_error: true, turn: 0 }]);
    const g = s.turns[0].parts[0];
    expect(g.kind === 'group' && g.calls).toEqual([{ id: 'tu_1', name: 'knomit_repos', state: 'done', isError: true }]);
  });

  it('usage attaches to the turn it names, not the last one', () => {
    const s = run([
      ...sent('one', 1, 0), text('a', 0), { kind: 'turn_done', turn: 0 },
      ...sent('two', 2, 2), text('b', 2),
      { kind: 'usage', usage: usage1, cost_usd: 0.5, turn: 2 },
      // A late usage frame of turn one lands on turn one.
      { kind: 'usage', usage: { input: 10, cache_read: 0, cache_write: 0, output: 0 }, turn: 0 },
    ]);
    expect(s.turns.map(t => t.usage?.input)).toEqual([10, 1]);
    expect(s.turns[1].costUsd).toBe(0.5);
    expect(s.turns[0].costUsd).toBeNull();
  });

  // The auto-title request's usage (side): its cost and tokens count toward the
  // turn it followed, so toward the session cost, but its prompt is not the
  // conversation's, so the context meter keeps the turn's last prompt.
  // SABOTAGE: take side usage for ctxTokens -> red.
  it('a side usage adds to the turn and the session cost but leaves the context meter alone', () => {
    const s = run([...sent('q', 1, 0),
      { kind: 'usage', usage: { input: 1000, cache_read: 200, cache_write: 0, output: 50 }, cost_usd: 0.01, seq: 1, turn: 0 },
      { kind: 'usage', usage: { input: 300, cache_read: 0, cache_write: 0, output: 7 }, cost_usd: 0.002, seq: 2, side: true, turn: 0 }]);
    expect(s.ctxTokens).toBe(1200);
    expect(s.lastUsageSeq).toBe(2);
    expect(s.turns[0].usage).toEqual({ input: 1300, cacheRead: 200, cacheWrite: 0, output: 57 });
    expect(s.turns[0].costUsd).toBeCloseTo(0.012, 12);
    expect(sessionCost(s.turns)).toBeCloseTo(0.012, 12);
  });

  it('sums the usage of a turn\'s requests; cost stays unknown until a request prices', () => {
    const s = run([...sent('q', 1, 0), { kind: 'usage', usage: usage1, turn: 0 }, { kind: 'usage', usage: usage1, turn: 0 }]);
    expect(s.turns[0].usage).toEqual({ input: 2, cacheRead: 4, cacheWrite: 6, output: 8 });
    expect(s.turns[0].costUsd).toBeNull();
  });

  it('a late event from the stopped turn lands on that turn even after a new local turn exists', () => {
    const s = run([
      ...sent('a', 1, 0), text('partial', 0), { kind: 'stopped' },
      { kind: 'user', text: 'b', sendId: 2 },
      text(' late', 0), call('tu_l', 'knomit_repos', 0), { kind: 'usage', usage: usage1, turn: 0 },
    ]);
    expect(s.turns[0].parts.map(p => p.kind)).toEqual(['text', 'group']);
    expect(s.turns[0].parts[0]).toEqual({ kind: 'text', text: 'partial late' });
    expect(s.turns[0].usage?.input).toBe(1);
    expect(s.turns[1].parts).toEqual([]);
    expect(s.turns[1].usage).toBeNull();
    expect(s.turns[1].local).toEqual({ sendId: 2, accepted: false });
    expect(s.staleSeq).toBe(0);
  });

  it('the new turn\'s events before its 202 attach to the local turn, which learns its index from them', () => {
    // Turn one is at 0; the second send is not answered yet; the server's events for it name 2.
    const s = run([
      ...sent('one', 1, 0), { kind: 'turn_done', turn: 0 },
      { kind: 'user', text: 'two', sendId: 2 },
      text('first words', 2), call('tu_1', 'knomit_learn', 2), { kind: 'approval', approval: approval('tu_1', 2) },
      { kind: 'usage', usage: usage1, turn: 2 },
    ]);
    expect(s.turns).toHaveLength(2);
    expect(s.turns[0].parts).toEqual([]);
    expect(s.turns[0].usage).toBeNull();
    expect(s.turns[1].at).toBe(2);
    expect(s.turns[1].local).toEqual({ sendId: 2, accepted: true });
    expect(s.turns[1].parts.map(p => p.kind)).toEqual(['text', 'group']);
    expect(s.turns[1].usage?.input).toBe(1);
    expect(Object.keys(s.approvals)).toEqual(['tu_1']);
    expect(s.staleSeq).toBe(0);
    // The 202 arrives afterwards, naming the same index: nothing changes.
    expect(chatReducer(s, { kind: 'send_accepted', sendId: 2, turn: 2, at: 1000 }).turns[1].at).toBe(2);
  });

  it('the first message of a chat: its events before the 202 land on the local turn, the approval included', () => {
    const s = run([
      { kind: 'user', text: 'first ever', sendId: 1 },
      call('tu_1', 'knomit_learn', 0), { kind: 'approval', approval: approval('tu_1', 0) },
    ]);
    expect(s.turns).toHaveLength(1);
    expect(s.turns[0].parts.map(p => p.kind)).toEqual(['group']);
    expect(s.turns[0].at).toBe(0);
    expect(s.approvals['tu_1'].turn).toBe(0);
    expect(s.staleSeq).toBe(0);
  });

  it('an event for a turn the page has no turn for, with no local turn, asks for a snapshot and applies nothing', () => {
    const s = run([text('from another tab', 4), { kind: 'approval', approval: approval('tu_9', 4) }]);
    expect(s.turns).toEqual([]);
    expect(s.staleSeq).toBe(2);
  });

  it('a late turn end from an older turn does not end the run of the newer one', () => {
    const s = run([
      ...sent('a', 1, 0), { kind: 'stopped' }, ...sent('b', 2, 2),
      { kind: 'turn_error', message: 'llm: the turn was cancelled', cancelled: true, turn: 0 },
    ]);
    expect(s.running).toBe(true);
    expect(run([{ kind: 'turn_done', turn: 2 }], s).running).toBe(false);
  });

  it('a snapshot replaces the transcript verbatim: parts, usage, cost, warning and how each turn ended', () => {
    const dirty = run([...sent('stale', 1, 0), text('stale reply', 0), { kind: 'warning', message: 'w', turn: 0 }]);
    const s = chatReducer(dirty, {
      kind: 'ready',
      snapshot: snap([
        { kind: 'user', text: 'hi', usage: { input: 1, cache_read: 2, cache_write: 3, output: 4 }, cost_usd: 0.25, warning: 'server warning' },
        { kind: 'assistant', text: 'let me look' },
        { kind: 'tool_call', id: 'tu_1', name: 'knomit_repos', state: 'running' },
        { kind: 'tool_call', id: 'tu_2', name: 'knomit_query', state: 'done' },
        { kind: 'user', text: 'again', error: 'The model declined to answer.' },
        { kind: 'user', text: 'stopped', error: 'llm: the turn was cancelled', cancelled: true },
      ], { running: true, usage_seq: 4, pending_approvals: [approval('tu_1', 0)] }),
    });
    expect(s.turns.map(t => t.user)).toEqual(['hi', 'again', 'stopped']);
    expect(s.turns.map(t => t.at)).toEqual([0, 4, 5]);
    expect(s.turns[0].parts.map(p => p.kind)).toEqual(['text', 'group']);
    expect(s.turns[0].usage).toEqual({ input: 1, cacheRead: 2, cacheWrite: 3, output: 4 });
    expect(s.turns[0].costUsd).toBe(0.25);
    expect(s.turns[0].warning).toBe('server warning');
    expect(s.turns[1].error).toBe('The model declined to answer.');
    expect(s.turns[2].error).toBeNull(); // the person's own Stop is not an error
    expect(s.running).toBe(true);
    expect(Object.keys(s.approvals)).toEqual(['tu_1']);
    expect(s.lastUsageSeq).toBe(4);
    expect(s.loaded).toBe(true);
    expect(s.turns.some(t => t.user === 'stale')).toBe(false);
  });

  it('a reconnect keeps every earlier turn\'s usage line (the snapshot carries them)', () => {
    const items: ChatViewItem[] = [
      { kind: 'user', text: 'one', usage: { input: 1, cache_read: 0, cache_write: 0, output: 2 }, cost_usd: 0.1 },
      { kind: 'assistant', text: 'A' },
      { kind: 'user', text: 'two', usage: { input: 3, cache_read: 0, cache_write: 0, output: 4 } },
      { kind: 'assistant', text: 'B' },
    ];
    const s = chatReducer(initialChatState, ready(items));
    expect(s.turns.map(t => t.usage?.input)).toEqual([1, 3]);
    expect(s.turns.map(t => t.costUsd)).toEqual([0.1, null]);
    expect(usageLine(s.turns[0].usage!, s.turns[0].costUsd)).toBe('input 1 · cache read 0 · cache write 0 · output 2 · $0.1000');
  });

  it('an approval is keyed by its call, and answering it removes it', () => {
    const s1 = run([...sent('q', 1, 0), call('tu_9', 'knomit_learn', 0), { kind: 'approval', approval: approval('tu_9', 0) }]);
    expect(s1.approvals['tu_9'].target).toBe('alpha');
    expect(run([{ kind: 'approval_answered', callId: 'tu_9' }], s1).approvals).toEqual({});
  });

  it('turn_error keeps the partial text and records the message; a cancelled one is quiet', () => {
    const s = run([...sent('q', 1, 0), text('partial', 0), { kind: 'turn_error', message: 'The model refused.', turn: 0 }]);
    expect(s.running).toBe(false);
    expect(s.turns[0].parts).toEqual([{ kind: 'text', text: 'partial' }]);
    expect(s.turns[0].error).toBe('The model refused.');
    const c = run([...sent('q', 1, 0), text('partial', 0), { kind: 'turn_error', message: 'llm: the turn was cancelled', cancelled: true, turn: 0 }]);
    expect(c.running).toBe(false);
    expect(c.turns[0].error).toBeNull();
    expect(c.turns[0].parts).toEqual([{ kind: 'text', text: 'partial' }]);
  });

  it('stopped ends the turn and drops its approvals, whatever the stream says', () => {
    const s = run([...sent('q', 1, 0), { kind: 'approval', approval: approval('tu_1', 0) }, { kind: 'stopped' }]);
    expect(s.running).toBe(false);
    expect(s.approvals).toEqual({});
  });

  it('a repeated running event does not move a done call back; the error flag stays', () => {
    const s = run([
      ...sent('q', 1, 0), call('tu_1', 'knomit_repos', 0),
      { kind: 'tool_call', id: 'tu_1', name: 'knomit_repos', state: 'done', is_error: true, turn: 0 },
      call('tu_1', 'knomit_repos', 0),
    ]);
    const g = s.turns[0].parts[0];
    expect(g.kind === 'group' && g.calls).toEqual([{ id: 'tu_1', name: 'knomit_repos', state: 'done', isError: true }]);
    expect(s.turns[0].parts).toHaveLength(1);
  });

  it('a usage event seen twice (same seq) is counted once; a different seq is another request', () => {
    const us = { input: 10, cache_read: 0, cache_write: 0, output: 5 };
    const once = run([...sent('q', 1, 0), { kind: 'usage', usage: us, cost_usd: 1, seq: 7, turn: 0 }, { kind: 'usage', usage: us, cost_usd: 1, seq: 7, turn: 0 }]);
    expect(once.turns[0].usage?.input).toBe(10);
    expect(once.turns[0].costUsd).toBe(1);
    const two = chatReducer(once, { kind: 'usage', usage: us, cost_usd: 1, seq: 8, turn: 0 });
    expect(two.turns[0].usage?.input).toBe(20);
    expect(two.turns[0].costUsd).toBe(2);
  });

  it('a usage event the snapshot already includes (seq at or below its usage_seq) is not added again', () => {
    const us = { input: 10, cache_read: 0, cache_write: 0, output: 5 };
    const s = chatReducer(initialChatState, ready([{ kind: 'user', text: 'q', usage: { input: 10, cache_read: 0, cache_write: 0, output: 5 } }], { usage_seq: 3 }));
    expect(chatReducer(s, { kind: 'usage', usage: us, seq: 3, turn: 0 }).turns[0].usage?.input).toBe(10);
    expect(chatReducer(s, { kind: 'usage', usage: us, seq: 4, turn: 0 }).turns[0].usage?.input).toBe(20);
  });

  it('the warning is the server\'s own message, verbatim, on its turn', () => {
    const s = run([...sent('q', 1, 0), { kind: 'warning', message: 'This chat is nearing 100,000 tokens.', turn: 0 }]);
    expect(s.turns[0].warning).toBe('This chat is nearing 100,000 tokens.');
  });

  it('a send starts a running turn; chat_end marks the chat ended', () => {
    const s = run([{ kind: 'user', text: 'q', sendId: 1 }]);
    expect(s.running).toBe(true);
    expect(run([{ kind: 'chat_end' }], s).ended).toBe(true);
  });

  it('snapshotState puts every user item on its own turn, at its view index', () => {
    const s = snapshotState(snap([u('a'), { kind: 'assistant', text: 'A' }, u('b')]));
    expect(s.turns.map(t => [t.user, t.at])).toEqual([['a', 0], ['b', 2]]);
  });
});

describe('chatReducer: the turn just sent', () => {
  it('is kept after a snapshot that cannot list it yet, and a snapshot never forces running for it', () => {
    const s = run([...sent('one', 1, 0), { kind: 'turn_done', turn: 0 }, { kind: 'user', text: 'two', sendId: 2 }]);
    const after = chatReducer(s, ready([u('one'), { kind: 'assistant', text: 'A' }]));
    expect(after.turns.map(t => t.user)).toEqual(['one', 'two']);
    expect(after.turns[1].local).toEqual({ sendId: 2, accepted: false });
    expect(after.running).toBe(false);
  });

  it('is dropped by a snapshot started after its POST was accepted: that snapshot lists it, or it is not there', () => {
    const s = run([...sent('one', 1, 0), { kind: 'turn_done', turn: 0 }, ...sent('two', 2, 2)]);
    const listed = chatReducer(s, ready([u('one'), { kind: 'assistant', text: 'A' }, u('two')], { running: true }, [1, 2]));
    expect(listed.turns.map(t => t.user)).toEqual(['one', 'two']);
    expect(listed.turns[1].local).toBeUndefined();
    const absent = chatReducer(s, ready([u('one')], {}, [1, 2]));
    expect(absent.turns.map(t => t.user)).toEqual(['one']);
  });

  it('a send accepted after the read started is kept (that read cannot list it) and marked accepted', () => {
    const s = run([...sent('one', 1, 0), { kind: 'turn_done', turn: 0 }, ...sent('two', 2, 2)]);
    const after = chatReducer(s, ready([u('one')], {}, [1]));
    expect(after.turns.map(t => t.user)).toEqual(['one', 'two']);
    expect(after.turns[1].local).toEqual({ sendId: 2, accepted: true });
  });

  it('B1: a refused send after late events from the stopped turn leaves no ghost and no stuck running', () => {
    const s = run([
      ...sent('a', 1, 0), text('partial', 0), { kind: 'stopped' },
      { kind: 'user', text: 'b', sendId: 2 },
      text(' late', 0),
      { kind: 'send_failed', sendId: 2 },
      { kind: 'turn_error', message: 'llm: the turn was cancelled', cancelled: true, turn: 0 },
      ready([u('a'), { kind: 'assistant', text: 'partial late' }], { running: false }, [1]),
    ]);
    expect(s.running).toBe(false);
    expect(s.turns.map(t => t.user)).toEqual(['a']);
  });

  it('send_failed removes the turn outright and an unknown send changes nothing', () => {
    const s = run([...sent('one', 1, 0), { kind: 'turn_done', turn: 0 }, { kind: 'user', text: 'two', sendId: 2 }]);
    const failed = chatReducer(s, { kind: 'send_failed', sendId: 2 });
    expect(failed.turns.map(t => t.user)).toEqual(['one']);
    expect(failed.running).toBe(false);
    expect(chatReducer(failed, { kind: 'send_failed', sendId: 9 })).toBe(failed);
  });
});

describe('toolCallLabel', () => {
  const c = (name: string, title?: string): ToolCall => ({ id: 'a', name, state: 'done', isError: false, ...(title ? { title } : {}) });
  it('is the title the server sent, whatever the tool', () => {
    expect(toolCallLabel(c('knomit_repos', 'List repos'))).toBe('List repos');
    expect(toolCallLabel(c('other_tool', 'Other'))).toBe('Other');
  });
  it('is the raw tool name when the tool has no title', () => {
    expect(toolCallLabel(c('knomit_repos'))).toBe('knomit_repos');
    expect(toolCallLabel(c('other_tool'))).toBe('other_tool');
  });
});

describe('usage line', () => {
  it('words the four counts and the cost', () => {
    const us = { input: 11, cacheRead: 22, cacheWrite: 33, output: 44 };
    expect(usageLine(us, 0.0123)).toBe('input 11 · cache read 22 · cache write 33 · output 44 · $0.0123');
    expect(usageLine(us, null)).toBe('input 11 · cache read 22 · cache write 33 · output 44 · cost unknown');
  });
  it('formats small and large costs', () => {
    expect(formatCost(0.00004)).toBe('<$0.0001');
    expect(formatCost(1.5)).toBe('$1.50');
  });
});

describe('the binding in force: a failed bind binds nothing', () => {
  const bind = (id: string, name: string, isError: boolean): ToolCall => ({ id, name: 'knomit_bind', state: 'done', isError, args: { name } });
  const query: ToolCall = { id: 'q', name: 'knomit_query', state: 'done', isError: false, args: { text: 'x' } };
  it('boundAfter: a bind that succeeded moves it, one that failed leaves it where it was', () => {
    expect(boundAfter([bind('a', 'alpha', false)], 'all')).toBe('alpha');
    expect(boundAfter([bind('a', 'nosuch', true)], 'all')).toBe('all');
    expect(boundAfter([bind('a', 'alpha', false), bind('b', 'nosuch', true)], '')).toBe('alpha');
  });
  it('boundBeforeCall: a call after a failed bind belongs to the binding before it', () => {
    const turns = [{ user: 'u', parts: [{ kind: 'group', calls: [bind('a', 'alpha', false), bind('b', 'nosuch', true), query] }] }] as unknown as ChatTurn[];
    expect(boundBeforeCall(turns, 'q')).toBe('alpha');
  });
});
