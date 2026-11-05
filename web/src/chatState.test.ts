import { describe, it, expect } from 'vitest';
import { chatReducer, initialChatState, toolRowLabel, usageLine, formatCost, snapshotState } from './chatState';
import type { ChatEvent, ChatState } from './chatState';
import type { ChatSnapshot, ChatViewItem } from './chatApi';

const run = (events: ChatEvent[], from: ChatState = initialChatState): ChatState => events.reduce(chatReducer, from);
const call = (id: string, name: string, state: 'running' | 'done' = 'running'): ChatEvent => ({ kind: 'tool_call', id, name, state });
/** A message sent from here whose POST the server has accepted. */
const sent = (text: string, sendId = 1): ChatEvent[] => [{ kind: 'user', text, sendId }, { kind: 'send_accepted', sendId }];
const snap = (items: ChatViewItem[], over: Partial<ChatSnapshot> = {}): ChatSnapshot => ({
  id: 'c', provider: 'p', model: 'm', running: false, grants: [], pending_approvals: [], items, ...over,
});
const ready = (items: ChatViewItem[], over: Partial<ChatSnapshot> = {}, confirmed: number[] = []): ChatEvent =>
  ({ kind: 'ready', snapshot: snap(items, over), confirmed });
const u = (text: string): ChatViewItem => ({ kind: 'user', text });

describe('chatReducer', () => {
  it('groups consecutive tool calls into one group', () => {
    const s = run([
      { kind: 'text_delta', text: 'looking' },
      call('tu_1', 'knomit_repos'), call('tu_2', 'knomit_query'),
      { kind: 'text_delta', text: 'found it' },
    ]);
    expect(s.turns).toHaveLength(1);
    const parts = s.turns[0].parts;
    expect(parts.map(p => p.kind)).toEqual(['text', 'group', 'text']);
    const group = parts[1];
    expect(group.kind === 'group' && group.calls.map(c => c.id)).toEqual(['tu_1', 'tu_2']);
    expect(parts[0].kind === 'text' && parts[0].text).toBe('looking');
    expect(parts[2].kind === 'text' && parts[2].text).toBe('found it');
  });

  it('streams text deltas into one text part', () => {
    const s = run([{ kind: 'text_delta', text: 'he' }, { kind: 'text_delta', text: 'llo' }]);
    expect(s.turns[0].parts).toEqual([{ kind: 'text', text: 'hello' }]);
  });

  it('a done event updates the call it names, not a new row', () => {
    const s = run([call('tu_1', 'knomit_repos'), { kind: 'tool_call', id: 'tu_1', name: 'knomit_repos', state: 'done', is_error: true }]);
    const g = s.turns[0].parts[0];
    expect(g.kind === 'group' && g.calls).toEqual([{ id: 'tu_1', name: 'knomit_repos', state: 'done', isError: true }]);
  });

  it('usage attaches to its turn', () => {
    const usage = { input: 11, cache_read: 22, cache_write: 33, output: 44 };
    const s = run([
      ...sent('one', 1), { kind: 'text_delta', text: 'a' }, { kind: 'turn_done' },
      ...sent('two', 2), { kind: 'text_delta', text: 'b' }, { kind: 'usage', usage, cost_usd: 0.5 },
    ]);
    expect(s.turns).toHaveLength(2);
    expect(s.turns[0].usage).toBeNull();
    expect(s.turns[1].usage).toEqual({ input: 11, cacheRead: 22, cacheWrite: 33, output: 44 });
    expect(s.turns[1].costUsd).toBe(0.5);
  });

  it('sums the usage of a turn\'s requests; cost stays unknown until a request prices', () => {
    const u1 = { input: 1, cache_read: 2, cache_write: 3, output: 4 };
    const s = run([...sent('q'), { kind: 'usage', usage: u1 }, { kind: 'usage', usage: u1 }]);
    expect(s.turns[0].usage).toEqual({ input: 2, cacheRead: 4, cacheWrite: 6, output: 8 });
    expect(s.turns[0].costUsd).toBeNull();
  });

  it('a snapshot replaces the transcript verbatim: parts, usage, cost, warning and how each turn ended', () => {
    const dirty = run([...sent('stale'), { kind: 'text_delta', text: 'stale reply' }, { kind: 'warning', message: 'w' }]);
    const s = chatReducer(dirty, {
      kind: 'ready',
      snapshot: snap([
        { kind: 'user', text: 'hi', usage: { input: 1, cache_read: 2, cache_write: 3, output: 4 }, cost_usd: 0.25, warning: 'server warning' },
        { kind: 'assistant', text: 'let me look' },
        { kind: 'tool_call', id: 'tu_1', name: 'knomit_repos', state: 'running' },
        { kind: 'tool_call', id: 'tu_2', name: 'knomit_query', state: 'done' },
        { kind: 'user', text: 'again', error: 'The model declined to answer.' },
        { kind: 'user', text: 'stopped', error: 'llm: the turn was cancelled', cancelled: true },
      ], {
        running: true, usage_seq: 4,
        pending_approvals: [{ call_id: 'tu_1', tool: 'knomit_repos', action: 'x', destructive: false, grantable: true }],
      }),
    });
    expect(s.turns.map(t => t.user)).toEqual(['hi', 'again', 'stopped']);
    expect(s.turns[0].parts.map(p => p.kind)).toEqual(['text', 'group']);
    expect(s.turns[0].usage).toEqual({ input: 1, cacheRead: 2, cacheWrite: 3, output: 4 });
    expect(s.turns[0].costUsd).toBe(0.25);
    expect(s.turns[0].warning).toBe('server warning');
    expect(s.turns[1].error).toBe('The model declined to answer.');
    expect(s.turns[2].error).toBeNull(); // the person's own Stop is not an error
    expect(s.turns[1].usage).toBeNull();
    expect(s.running).toBe(true);
    expect(Object.keys(s.approvals)).toEqual(['tu_1']);
    expect(s.lastUsageSeq).toBe(4);
    expect(s.loaded).toBe(true);
    // Nothing of the old state survives: no stale turn, no stale warning.
    expect(s.turns.some(t => t.user === 'stale')).toBe(false);
  });

  it('a reconnect keeps every earlier turn\'s usage line (the snapshot carries them)', () => {
    const items: ChatViewItem[] = [
      { kind: 'user', text: 'one', usage: { input: 1, cache_read: 0, cache_write: 0, output: 2 }, cost_usd: 0.1 },
      { kind: 'assistant', text: 'A' },
      { kind: 'user', text: 'two', usage: { input: 3, cache_read: 0, cache_write: 0, output: 4 } },
      { kind: 'assistant', text: 'B' },
    ];
    // A fresh page (a second tab, a reload) has no earlier state at all.
    const s = chatReducer(initialChatState, ready(items));
    expect(s.turns.map(t => t.usage?.input)).toEqual([1, 3]);
    expect(s.turns.map(t => t.costUsd)).toEqual([0.1, null]);
    expect(usageLine(s.turns[0].usage!, s.turns[0].costUsd)).toBe('input 1 · cache read 0 · cache write 0 · output 2 · $0.1000');
  });

  it('the second-tab scenario: a turn joined mid-way with usage lands on the server\'s turn it is, not on turn 0', () => {
    const joined = run([{ kind: 'text_delta', text: 'late text' }, { kind: 'usage', usage: { input: 9, cache_read: 9, cache_write: 9, output: 9 }, seq: 5 }]);
    expect(joined.turns[0].user).toBe('');
    // The server's view: an old turn with its own usage, then the in-flight turn.
    const s = chatReducer(joined, ready([
      { kind: 'user', text: 'old', usage: { input: 1, cache_read: 1, cache_write: 1, output: 1 } }, { kind: 'assistant', text: 'old answer' },
      { kind: 'user', text: 'in flight', usage: { input: 9, cache_read: 9, cache_write: 9, output: 9 } },
    ], { running: true, usage_seq: 5 }));
    expect(s.turns.map(t => [t.user, t.usage?.input])).toEqual([['old', 1], ['in flight', 9]]);
    expect(s.turns[0].error).toBeNull();
  });

  it('an approval is keyed by its call, and answering it removes it', () => {
    const ap = { call_id: 'tu_9', tool: 'knomit_learn', action: 'learn', target: 'alpha', target_kind: 'repo', destructive: false, grantable: true };
    const s1 = run([call('tu_9', 'knomit_learn'), { kind: 'approval', approval: ap }]);
    expect(s1.approvals['tu_9'].target).toBe('alpha');
    expect(run([{ kind: 'approval_answered', callId: 'tu_9' }], s1).approvals).toEqual({});
  });

  it('turn_done stops the turn; turn_error keeps the partial text and records the message', () => {
    const s = run([...sent('q'), { kind: 'text_delta', text: 'partial' }, { kind: 'turn_error', message: 'The model refused.' }]);
    expect(s.running).toBe(false);
    expect(s.turns[0].parts).toEqual([{ kind: 'text', text: 'partial' }]);
    expect(s.turns[0].error).toBe('The model refused.');
  });

  it('a cancelled turn ends quietly: no error, the partial text kept, not running', () => {
    const s = run([...sent('q'), { kind: 'text_delta', text: 'partial' }, { kind: 'turn_error', message: 'llm: the turn was cancelled', cancelled: true }]);
    expect(s.running).toBe(false);
    expect(s.turns[0].error).toBeNull();
    expect(s.turns[0].parts).toEqual([{ kind: 'text', text: 'partial' }]);
    expect(run([...sent('q'), { kind: 'turn_error', message: 'real failure', cancelled: false }]).turns[0].error).toBe('real failure');
  });

  it('stopped ends the turn and drops its approvals, whatever the stream says', () => {
    const ap = { call_id: 'tu_1', tool: 'knomit_learn', action: 'learn', destructive: false, grantable: true };
    const s = run([...sent('q'), { kind: 'approval', approval: ap }, { kind: 'stopped' }]);
    expect(s.running).toBe(false);
    expect(s.approvals).toEqual({});
  });

  it('a repeated running event does not move a done call back; the error flag stays', () => {
    const s = run([
      call('tu_1', 'knomit_repos'),
      { kind: 'tool_call', id: 'tu_1', name: 'knomit_repos', state: 'done', is_error: true },
      call('tu_1', 'knomit_repos'),
    ]);
    const g = s.turns[0].parts[0];
    expect(g.kind === 'group' && g.calls).toEqual([{ id: 'tu_1', name: 'knomit_repos', state: 'done', isError: true }]);
    expect(s.turns[0].parts).toHaveLength(1);
  });

  it('a usage event seen twice (same seq) is counted once; a different seq is another request', () => {
    const us = { input: 10, cache_read: 0, cache_write: 0, output: 5 };
    const once = run([...sent('q'), { kind: 'usage', usage: us, cost_usd: 1, seq: 7 }, { kind: 'usage', usage: us, cost_usd: 1, seq: 7 }]);
    expect(once.turns[0].usage?.input).toBe(10);
    expect(once.turns[0].costUsd).toBe(1);
    const two = chatReducer(once, { kind: 'usage', usage: us, cost_usd: 1, seq: 8 });
    expect(two.turns[0].usage?.input).toBe(20);
    expect(two.turns[0].costUsd).toBe(2);
  });

  it('a usage event the snapshot already includes (seq at or below its usage_seq) is not added again', () => {
    const us = { input: 10, cache_read: 0, cache_write: 0, output: 5 };
    const s = chatReducer(initialChatState, ready([{ kind: 'user', text: 'q', usage: { input: 10, cache_read: 0, cache_write: 0, output: 5 } }], { usage_seq: 3 }));
    expect(chatReducer(s, { kind: 'usage', usage: us, seq: 3 }).turns[0].usage?.input).toBe(10);
    expect(chatReducer(s, { kind: 'usage', usage: us, seq: 4 }).turns[0].usage?.input).toBe(20);
  });

  it('the warning is the server\'s own message, verbatim, on its turn', () => {
    const s = run([...sent('q'), { kind: 'warning', message: 'This chat is nearing 100,000 tokens.' }]);
    expect(s.turns[0].warning).toBe('This chat is nearing 100,000 tokens.');
  });

  it('a send starts a running turn; chat_end marks the chat ended', () => {
    const s = run([{ kind: 'user', text: 'q', sendId: 1 }]);
    expect(s.running).toBe(true);
    expect(run([{ kind: 'chat_end' }], s).ended).toBe(true);
  });

  it('snapshotState puts every user item on its own turn', () => {
    const s = snapshotState(snap([u('a'), { kind: 'assistant', text: 'A' }, u('b')]));
    expect(s.turns.map(t => t.user)).toEqual(['a', 'b']);
  });
});

describe('chatReducer: the turn just sent', () => {
  it('is kept after a snapshot that cannot list it yet, and a snapshot never forces running for it', () => {
    const s = run([...sent('one', 1), { kind: 'turn_done' }, { kind: 'user', text: 'two', sendId: 2 }]);
    // The read started before the second POST was answered.
    const after = chatReducer(s, ready([u('one'), { kind: 'assistant', text: 'A' }]));
    expect(after.turns.map(t => t.user)).toEqual(['one', 'two']);
    expect(after.turns[1].local).toEqual({ sendId: 2, accepted: false });
    expect(after.running).toBe(false);
  });

  it('is dropped by a snapshot started after its POST was accepted: that snapshot lists it, or it is not there', () => {
    const s = run([...sent('one', 1), { kind: 'turn_done' }, ...sent('two', 2)]);
    const listed = chatReducer(s, ready([u('one'), u('two')], { running: true }, [1, 2]));
    expect(listed.turns.map(t => t.user)).toEqual(['one', 'two']);
    expect(listed.turns[1].local).toBeUndefined();
    // Accepted before the read started, yet absent: the server's view wins.
    const absent = chatReducer(s, ready([u('one')], {}, [1, 2]));
    expect(absent.turns.map(t => t.user)).toEqual(['one']);
  });

  it('a send accepted after the read started is kept (that read cannot list it) and marked accepted', () => {
    const s = run([...sent('one', 1), { kind: 'turn_done' }, ...sent('two', 2)]);
    const after = chatReducer(s, ready([u('one')], {}, [1])); // read began before send 2 was accepted
    expect(after.turns.map(t => t.user)).toEqual(['one', 'two']);
    expect(after.turns[1].local).toEqual({ sendId: 2, accepted: true });
  });

  it('B1: a refused send after late events from the stopped turn leaves no ghost and no stuck running', () => {
    // user a, text_delta, stopped, user b, text_delta (late, from a), send_failed b, cancelled turn_error, snapshot running:false
    const s = run([
      ...sent('a', 1), { kind: 'text_delta', text: 'partial' }, { kind: 'stopped' },
      { kind: 'user', text: 'b', sendId: 2 },
      { kind: 'text_delta', text: 'late' },
      { kind: 'send_failed', sendId: 2 },
      { kind: 'turn_error', message: 'llm: the turn was cancelled', cancelled: true },
      ready([u('a'), { kind: 'assistant', text: 'partial late' }], { running: false }, [1]),
    ]);
    expect(s.running).toBe(false);
    expect(s.turns.map(t => t.user)).toEqual(['a']);
  });

  it('a late event while the send is unconfirmed belongs to the turn before it, never to the new one', () => {
    const s = run([...sent('a', 1), { kind: 'user', text: 'b', sendId: 2 }, { kind: 'text_delta', text: 'late from a' }]);
    expect(s.turns[0].parts).toEqual([{ kind: 'text', text: 'late from a' }]);
    expect(s.turns[1].parts).toEqual([]);
    // Once accepted, events are the new turn's.
    const accepted = run([{ kind: 'send_accepted', sendId: 2 }, { kind: 'text_delta', text: 'for b' }], s);
    expect(accepted.turns[1].parts).toEqual([{ kind: 'text', text: 'for b' }]);
  });

  it('send_failed removes the turn outright and an unknown send changes nothing', () => {
    const s = run([...sent('one', 1), { kind: 'turn_done' }, { kind: 'user', text: 'two', sendId: 2 }]);
    const failed = chatReducer(s, { kind: 'send_failed', sendId: 2 });
    expect(failed.turns.map(t => t.user)).toEqual(['one']);
    expect(failed.running).toBe(false);
    expect(chatReducer(failed, { kind: 'send_failed', sendId: 9 })).toBe(failed);
  });
});

describe('toolRowLabel', () => {
  it('derives "knomit: Knomit <rest>" from the tool name', () => {
    expect(toolRowLabel('knomit_repos')).toBe('knomit: Knomit repos');
    expect(toolRowLabel('knomit_materialize')).toBe('knomit: Knomit materialize');
    expect(toolRowLabel('knomit_review_answer')).toBe('knomit: Knomit review answer');
  });
  it('leaves a tool that is not a knomit tool as its own name', () => {
    expect(toolRowLabel('other_tool')).toBe('other_tool');
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
