import { describe, it, expect } from 'vitest';
import { chatReducer, initialChatState, toolRowLabel, usageLine, formatCost, snapshotState } from './chatState';
import type { ChatEvent, ChatState } from './chatState';

const run = (events: ChatEvent[], from: ChatState = initialChatState): ChatState => events.reduce(chatReducer, from);
const call = (id: string, name: string, state: 'running' | 'done' = 'running'): ChatEvent => ({ kind: 'tool_call', id, name, state });

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
      { kind: 'user', text: 'one' }, { kind: 'text_delta', text: 'a' }, { kind: 'turn_done' },
      { kind: 'user', text: 'two' }, { kind: 'text_delta', text: 'b' }, { kind: 'usage', usage, cost_usd: 0.5 },
    ]);
    expect(s.turns).toHaveLength(2);
    expect(s.turns[0].usage).toBeNull();
    expect(s.turns[1].usage).toEqual({ input: 11, cacheRead: 22, cacheWrite: 33, output: 44 });
    expect(s.turns[1].costUsd).toBe(0.5);
  });

  it('sums the usage of a turn\'s requests; cost stays unknown until a request prices', () => {
    const u = { input: 1, cache_read: 2, cache_write: 3, output: 4 };
    const s = run([{ kind: 'user', text: 'q' }, { kind: 'usage', usage: u }, { kind: 'usage', usage: u }]);
    expect(s.turns[0].usage).toEqual({ input: 2, cacheRead: 4, cacheWrite: 6, output: 8 });
    expect(s.turns[0].costUsd).toBeNull();
  });

  it('ready resets to the refetched transcript', () => {
    const dirty = run([{ kind: 'user', text: 'stale' }, { kind: 'text_delta', text: 'stale reply' }, { kind: 'warning', message: 'w' }]);
    const s = chatReducer(dirty, {
      kind: 'ready',
      snapshot: {
        id: 'chat-a', provider: 'anthropic', model: 'm', running: true, grants: [],
        items: [
          { kind: 'user', text: 'hi' },
          { kind: 'assistant', text: 'let me look' },
          { kind: 'tool_call', id: 'tu_1', name: 'knomit_repos', state: 'running' },
          { kind: 'tool_call', id: 'tu_2', name: 'knomit_query', state: 'done' },
        ],
        pending_approvals: [{ call_id: 'tu_1', tool: 'knomit_repos', action: 'x', destructive: false, grantable: true }],
      },
    });
    expect(s.turns).toHaveLength(1);
    expect(s.turns[0].user).toBe('hi');
    expect(s.turns[0].parts.map(p => p.kind)).toEqual(['text', 'group']);
    expect(s.running).toBe(true);
    expect(Object.keys(s.approvals)).toEqual(['tu_1']);
    expect(s.warning).toBeNull();
    expect(s.loaded).toBe(true);
  });

  it('an approval is keyed by its call, and answering it removes it', () => {
    const ap = { call_id: 'tu_9', tool: 'knomit_learn', action: 'learn', target: 'alpha', target_kind: 'repo', destructive: false, grantable: true };
    const s1 = run([call('tu_9', 'knomit_learn'), { kind: 'approval', approval: ap }]);
    expect(s1.approvals['tu_9'].target).toBe('alpha');
    expect(run([{ kind: 'approval_answered', callId: 'tu_9' }], s1).approvals).toEqual({});
  });

  it('turn_done stops the turn; turn_error keeps the partial text and records the message', () => {
    const s = run([{ kind: 'user', text: 'q' }, { kind: 'text_delta', text: 'partial' }, { kind: 'turn_error', message: 'The model refused.' }]);
    expect(s.running).toBe(false);
    expect(s.turns[0].parts).toEqual([{ kind: 'text', text: 'partial' }]);
    expect(s.turns[0].error).toBe('The model refused.');
  });

  it('the warning is the server\'s own message, verbatim', () => {
    expect(run([{ kind: 'warning', message: 'This chat is nearing 100,000 tokens.' }]).warning).toBe('This chat is nearing 100,000 tokens.');
  });

  it('user starts a running turn; chat_end marks the chat ended', () => {
    const s = run([{ kind: 'user', text: 'q' }]);
    expect(s.running).toBe(true);
    expect(run([{ kind: 'chat_end' }], s).ended).toBe(true);
  });

  it('snapshotState puts every user item on its own turn', () => {
    const s = snapshotState({
      id: 'c', provider: 'p', model: 'm', running: false, grants: [], pending_approvals: [],
      items: [{ kind: 'user', text: 'a' }, { kind: 'assistant', text: 'A' }, { kind: 'user', text: 'b' }],
    });
    expect(s.turns.map(t => t.user)).toEqual(['a', 'b']);
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
    const u = { input: 11, cacheRead: 22, cacheWrite: 33, output: 44 };
    expect(usageLine(u, 0.0123)).toBe('input 11 · cache read 22 · cache write 33 · output 44 · $0.0123');
    expect(usageLine(u, null)).toBe('input 11 · cache read 22 · cache write 33 · output 44 · cost unknown');
  });
  it('formats small and large costs', () => {
    expect(formatCost(0.00004)).toBe('<$0.0001');
    expect(formatCost(1.5)).toBe('$1.50');
  });
});
