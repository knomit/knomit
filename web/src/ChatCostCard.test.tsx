import { describe, it, expect, afterEach } from 'vitest';
import { render, screen, fireEvent, cleanup } from '@testing-library/react';
import { ChatTurnFooter } from './ChatTurnFooter';
import { costCardHeader } from './chatCopy';
import { chatReducer, initialChatState, snapshotState } from './chatState';
import type { ChatEvent } from './chatState';
import type { ChatViewItem } from './chatApi';

afterEach(cleanup);

const usage = { input: 612, output: 132, cacheRead: 67572, cacheWrite: 2401 };
const rows = { input: 0.051, cacheRead: 0.0027, cacheWrite: 0.01, output: 0.007 };

function card(over: Partial<Parameters<typeof ChatTurnFooter>[0]> = {}) {
  render(
    <ChatTurnFooter usage={usage} costUsd={0.0707} costRows={rows} turnNumber={3} modelName="Haiku 5.5"
      answer="a" sessionCostUsd={0.0213} sessionTurns={3} {...over} />,
  );
  fireEvent.mouseEnter(screen.getByTestId('chat-usage'));
  return screen.getByTestId('chat-cost-card');
}

describe('the cost card header', () => {
  it('is "Turn N · <model>" and nothing else, whatever the model or turn', () => {
    expect(costCardHeader(3, 'Haiku 5.5')).toBe('Turn 3 · Haiku 5.5');
    expect(costCardHeader(1, 'claude-x')).toBe('Turn 1 · claude-x');
  });
  it('is the first line of the card, with the model name it was given', () => {
    const el = card({ modelName: 'claude-x' });
    expect(screen.getByTestId('chat-cost-header').textContent).toBe('Turn 3 · claude-x');
    expect(el.firstElementChild).toBe(screen.getByTestId('chat-cost-header'));
  });
  it('a model with no price card has the same header', () => {
    card({ costUsd: null, costRows: null });
    expect(screen.getByTestId('chat-cost-header').textContent).toBe('Turn 3 · Haiku 5.5');
  });
});

describe('the per-row dollars', () => {
  it('each row shows its own cost in the existing format, and they add up to the total shown', () => {
    card();
    const text = (k: string) => screen.getByTestId(`chat-cost-usd-${k}`).textContent;
    expect(['input', 'cache-read', 'cache-write', 'output'].map(text)).toEqual(['$0.0510', '$0.0027', '$0.0100', '$0.0070']);
    expect(screen.getByTestId('chat-cost-turn').textContent).toBe('$0.0707');
    expect(rows.input + rows.cacheRead + rows.cacheWrite + rows.output).toBeCloseTo(0.0707, 12);
  });
  it('a row under a hundredth of a cent reads <$0.0001', () => {
    card({ costRows: { ...rows, input: 0.00001 } });
    expect(screen.getByTestId('chat-cost-usd-input').textContent).toBe('<$0.0001');
  });
  it('a model with no price card: no dollar on any row, and the total stays "cost unknown"', () => {
    card({ costUsd: null, costRows: null });
    for (const k of ['input', 'cache-read', 'cache-write', 'output']) expect(screen.queryByTestId(`chat-cost-usd-${k}`)).toBeNull();
    expect(screen.getByTestId('chat-cost-turn').textContent).toBe('–');
  });
});

describe('rows and tier reach the page from the server', () => {
  const u = { input: 1, cache_read: 2, cache_write: 3, output: 4 };
  const r1 = { input: 0.001, cache_read: 0.0002, cache_write: 0.00375, output: 0.002 };
  const r2 = { input: 0.05, cache_read: 0.0025, cache_write: 0.00625, output: 0.005 };
  const sum = (r: { input: number; cacheRead: number; cacheWrite: number; output: number }) => r.input + r.cacheRead + r.cacheWrite + r.output;

  it('live usage events add up per row, and the rows sum to the turn cost', () => {
    const evs: ChatEvent[] = [
      { kind: 'user', text: 'q', sendId: 1 }, { kind: 'send_accepted', sendId: 1, turn: 0, at: 1 },
      { kind: 'usage', usage: u, cost_usd: 0.00695, cost_rows: r1, turn: 0 },
      { kind: 'usage', usage: u, cost_usd: 0.06375, cost_rows: r2, turn: 0 },
    ];
    const t = evs.reduce(chatReducer, initialChatState).turns[0];
    const r = t.costRows!;
    expect([r.input, r.cacheRead, r.cacheWrite, r.output].map(v => +v.toFixed(10))).toEqual([0.051, 0.0027, 0.01, 0.007]);
    expect(sum(t.costRows!)).toBeCloseTo(t.costUsd!, 12);
  });
  it('a snapshot turn carries them too; one without a price card has none', () => {
    const items: ChatViewItem[] = [
      { kind: 'user', text: 'a', usage: u, cost_usd: 0.0707, cost_rows: { input: 0.051, cache_read: 0.0027, cache_write: 0.01, output: 0.007 } },
      { kind: 'user', text: 'b', usage: u },
    ];
    const s = snapshotState({ id: 'c', provider: 'p', model: 'm', running: false, grants: [], pending_approvals: [], items }, initialChatState, []);
    expect(s.turns[0].costRows).toEqual({ input: 0.051, cacheRead: 0.0027, cacheWrite: 0.01, output: 0.007 });
    expect(s.turns[1].costRows).toBeNull();
  });
});
