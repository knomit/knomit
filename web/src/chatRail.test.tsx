import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
import { render, screen, fireEvent, within, cleanup, renderHook } from '@testing-library/react';
import { TargetRail } from './TargetRail';
import { TopBar } from './TopBar';
import { targetLabelFor } from './facets';
import { MARK_STYLE, railState, recentChats, useSeenTurns } from './chatRail';
import type { ChatInfo } from './chatApi';
import { __resetRepoCreatesForTest } from './useRepoCreates';
import { init } from './state';

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>();
  return { ...actual, api: { ...actual.api, listRepoCreates: vi.fn().mockResolvedValue([]) } };
});

beforeEach(() => { __resetRepoCreatesForTest(); });
afterEach(cleanup);

const providers = [{ name: 'prov-a', kind: 'anthropic', models: [] }, { name: 'prov-b', kind: 'ollama', models: [] }];
const chat = (id: string, over: Partial<ChatInfo> = {}): ChatInfo => ({
  id, provider: 'prov-a', model: 'm', name: 'Model M', title: `title ${id}`, state: 'idle', turns: 1,
  last_activity: `2026-11-06T10:00:0${id.length}Z`, started_at: '2026-11-06T09:00:00Z', ...over,
});

function rail(over: Partial<React.ComponentProps<typeof TargetRail>> = {}) {
  return render(
    <TargetRail repos={[]} lenses={[]} context={{ kind: 'repo', repo: 'a' }} currentRepo="a" remoteError="" serverReadOnly={false}
      onSelect={vi.fn()} chatProviders={providers} chatsAvailable {...over} />,
  );
}

describe('railState: which state a row shows', () => {
  it('maps the server state, and a new answer only when unseen and not on screen', () => {
    expect(railState(chat('a', { state: 'idle', turns: 2 }), 2, false)).toBe('idle');
    expect(railState(chat('a', { state: 'running', turns: 2 }), 1, false)).toBe('running');
    expect(railState(chat('a', { state: 'awaiting_approval' }), 1, true)).toBe('attention');
    expect(railState(chat('a', { state: 'error' }), 1, false)).toBe('failed');
    expect(railState(chat('a', { state: 'idle', turns: 2 }), 1, false)).toBe('new');
    expect(railState(chat('a', { state: 'idle', turns: 2 }), 1, true)).toBe('idle');
    expect(railState(chat('a', { state: 'idle', turns: 2 }), undefined, false)).toBe('idle');
  });
});

describe('the mark colours', () => {
  it('has the design colours, exactly', () => {
    expect(MARK_STYLE.idle).toEqual({ color: '#b59a5e' });
    expect(MARK_STYLE.running).toEqual({ color: '#f5d495', background: 'rgba(232,194,122,.14)', boxShadow: '0 0 8px rgba(232,194,122,.35)' });
    expect(MARK_STYLE.attention).toEqual({ color: '#e8c27a', boxShadow: 'inset 0 0 0 1.5px #e8c27a' });
    expect(MARK_STYLE.new).toEqual({ color: '#f0f0f0' });
    expect(MARK_STYLE.failed).toEqual({ color: '#f06a60' });
  });

  it('a row\'s mark wears the colour of its state', () => {
    const chats = [
      chat('i', { state: 'idle' }), chat('r', { state: 'running' }), chat('att', { state: 'awaiting_approval' }),
      chat('n', { state: 'idle', turns: 3 }), chat('f', { state: 'error' }),
    ];
    rail({ chats, chatSeen: { i: 1, r: 1, att: 1, n: 2, f: 1 } });
    const style = (id: string) => screen.getByTestId(`rail-chat-mark-${id}`).style;
    expect(style('i').color).toBe('rgb(181, 154, 94)');
    expect(style('r').color).toBe('rgb(245, 212, 149)');
    expect(style('r').background).toContain('rgba(232, 194, 122, 0.14)');
    expect(style('att').color).toBe('rgb(232, 194, 122)');
    expect(style('att').boxShadow).toBe('inset 0 0 0 1.5px #e8c27a');
    expect(style('n').color).toBe('rgb(240, 240, 240)');
    expect(style('f').color).toBe('rgb(240, 106, 96)');
    expect(screen.getByTestId('rail-chat-att')).toHaveAttribute('data-chat-state', 'attention');
  });
});

describe('the Chats group', () => {
  it('shows the five most recent by last activity, then "N more", which expands in place', () => {
    const chats = ['c1', 'c2', 'c3', 'c4', 'c5', 'c6', 'c7'].map((id, i) => chat(id, { last_activity: `2026-11-06T10:00:0${i}Z` }));
    rail({ chats });
    const ids = () => screen.getAllByTestId(/^rail-chat-title-/).map(e => e.textContent);
    expect(ids()).toEqual(['title c7', 'title c6', 'title c5', 'title c4', 'title c3']);
    const more = screen.getByTestId('rail-chats-more');
    expect(more).toHaveTextContent('2 more');
    fireEvent.click(more);
    expect(ids()).toEqual(['title c7', 'title c6', 'title c5', 'title c4', 'title c3', 'title c2', 'title c1']);
    expect(screen.queryByTestId('rail-chats-more')).toBeNull();
  });

  it('has no "N more" for five or fewer', () => {
    rail({ chats: [chat('c1'), chat('c2')] });
    expect(screen.queryByTestId('rail-chats-more')).toBeNull();
  });

  it('keeps a selected chat out of the hidden ones', () => {
    const chats = ['c1', 'c2', 'c3', 'c4', 'c5', 'c6'].map((id, i) => chat(id, { last_activity: `2026-11-06T10:00:0${i}Z` }));
    rail({ chats, pinned: { kind: 'chat', id: 'c1' } });
    expect(screen.getByTestId('rail-chat-c1')).toHaveAttribute('aria-current', 'true');
    expect(screen.queryByTestId('rail-chats-more')).toBeNull();
  });

  it('a row is one line: the mark, then the title with an ellipsis, and nothing else', () => {
    rail({ chats: [chat('c1', { title: 'A very long title '.repeat(10), turns: 4, cost_usd: 0.5 })] });
    const row = screen.getByTestId('rail-chat-c1');
    const kids = Array.from(row.children);
    expect(kids).toHaveLength(2);
    expect(kids[0]).toBe(screen.getByTestId('rail-chat-mark-c1'));
    const t = kids[1] as HTMLElement;
    expect(t.style.whiteSpace).toBe('nowrap');
    expect(t.style.textOverflow).toBe('ellipsis');
    expect(t.style.overflow).toBe('hidden');
    expect(t.textContent).toBe('A very long title '.repeat(10));
    // The mark's own <title> is the provider's name; nothing else is in the row.
    expect(row.textContent).toBe('prov-a' + 'A very long title '.repeat(10));
  });

  it('falls back to the model name for a chat with no title yet', () => {
    rail({ chats: [chat('c1', { title: '' })] });
    expect(screen.getByTestId('rail-chat-title-c1')).toHaveTextContent('Model M');
  });

  it('orders recentChats newest first', () => {
    const out = recentChats([chat('a', { last_activity: '2026-11-06T09:00:00Z' }), chat('b', { last_activity: '2026-11-06T11:00:00Z' })]);
    expect(out.map(c => c.id)).toEqual(['b', 'a']);
  });
});

describe('hover: tooltip and ×', () => {
  it('the tooltip has the full title, the state in words, the model display name, start and turns, and cost', () => {
    const c = chat('c1', { title: 'Retract stale decisions in knomit-kb', state: 'awaiting_approval', turns: 3, cost_usd: 0.0841, model: 'claude-sonnet-5-5', name: 'Sonnet 5.5' });
    rail({ chats: [c] });
    fireEvent.mouseEnter(screen.getByTestId('rail-chat-wrap-c1'));
    const tip = screen.getByTestId('rail-chat-tip-c1');
    expect(tip).toHaveTextContent('Retract stale decisions in knomit-kb');
    const state = within(tip).getByTestId('rail-chat-tip-state-c1');
    expect(state).toHaveTextContent('Waiting for your approval');
    expect(state.style.color).toBe('rgb(204, 204, 204)'); // normal text, not amber
    expect(within(tip).getByTestId('rail-chat-tip-model-c1')).toHaveTextContent(/^Sonnet 5\.5$/);
    expect(within(tip).getByTestId('rail-chat-tip-started-c1').textContent).toMatch(/^\d\d:\d\d · 3 turns$/);
    expect(within(tip).getByTestId('rail-chat-tip-cost-c1')).toHaveTextContent('$0.0841');
    expect(Array.from(tip.querySelectorAll('div > span:nth-child(odd)')).map(e => e.textContent)).toEqual(['Model', 'Started', 'Cost']);
  });

  it('says "1 turn" for one, shows no state words for idle, and a dash when unpriced', () => {
    rail({ chats: [chat('c1', { turns: 1 })] });
    fireEvent.mouseEnter(screen.getByTestId('rail-chat-wrap-c1'));
    expect(screen.getByTestId('rail-chat-tip-started-c1').textContent).toMatch(/ · 1 turn$/);
    expect(screen.queryByTestId('rail-chat-tip-state-c1')).toBeNull();
    expect(screen.getByTestId('rail-chat-tip-cost-c1')).toHaveTextContent('–');
  });

  it('× ends an idle chat at once', () => {
    const onEndChat = vi.fn();
    rail({ chats: [chat('c1'), chat('c2')], onEndChat });
    fireEvent.click(screen.getByTestId('rail-chat-end-c2'));
    expect(onEndChat).toHaveBeenCalledTimes(1);
    expect(onEndChat).toHaveBeenCalledWith('c2');
  });

  it('× asks first when a turn is running, and ends it only when confirmed', () => {
    const onEndChat = vi.fn();
    rail({ chats: [chat('c1', { state: 'running' })], onEndChat });
    fireEvent.click(screen.getByTestId('rail-chat-end-c1'));
    expect(onEndChat).not.toHaveBeenCalled();
    expect(screen.getByTestId('rail-chat-confirm-c1')).toBeInTheDocument();
    fireEvent.click(screen.getByTestId('rail-chat-confirm-keep-c1'));
    expect(onEndChat).not.toHaveBeenCalled();
    expect(screen.queryByTestId('rail-chat-confirm-c1')).toBeNull();
    fireEvent.click(screen.getByTestId('rail-chat-end-c1'));
    fireEvent.click(screen.getByTestId('rail-chat-confirm-end-c1'));
    expect(onEndChat).toHaveBeenCalledTimes(1);
    expect(onEndChat).toHaveBeenCalledWith('c1');
  });

  it('× also asks while a call waits for approval, and never opens the chat', () => {
    const onEndChat = vi.fn();
    const onPin = vi.fn();
    rail({ chats: [chat('c1', { state: 'awaiting_approval' })], onEndChat, onPin });
    fireEvent.click(screen.getByTestId('rail-chat-end-c1'));
    expect(screen.getByTestId('rail-chat-confirm-c1')).toBeInTheDocument();
    expect(onEndChat).not.toHaveBeenCalled();
    expect(onPin).not.toHaveBeenCalled();
  });
});

describe('new-answer tracking', () => {
  it('a chat first seen sets the baseline; a turn that completes elsewhere is new; opening it clears it', () => {
    const idle = (turns: number, state: ChatInfo['state'] = 'idle') => [chat('c1', { turns, state })];
    const { result, rerender } = renderHook(({ chats, shown }: { chats: ChatInfo[]; shown: string | null }) => useSeenTurns(chats, shown),
      { initialProps: { chats: idle(2), shown: null as string | null } });
    expect(result.current).toEqual({ c1: 2 });
    // A turn starts: the running turn is not an answer yet.
    rerender({ chats: idle(3, 'running'), shown: null });
    expect(result.current).toEqual({ c1: 2 });
    expect(railState(idle(3, 'running')[0], result.current.c1, false)).toBe('running');
    // It finishes while another page is up: a new answer.
    rerender({ chats: idle(3), shown: null });
    expect(railState(idle(3)[0], result.current.c1, false)).toBe('new');
    // The chat is opened: seen.
    rerender({ chats: idle(3), shown: 'c1' });
    expect(result.current).toEqual({ c1: 3 });
    rerender({ chats: idle(3), shown: null });
    expect(railState(idle(3)[0], result.current.c1, false)).toBe('idle');
  });

  it('a chat on screen never turns new when its answer lands', () => {
    const { result, rerender } = renderHook(({ chats, shown }: { chats: ChatInfo[]; shown: string | null }) => useSeenTurns(chats, shown),
      { initialProps: { chats: [chat('c1', { turns: 1, state: 'running' })], shown: 'c1' as string | null } });
    expect(result.current).toEqual({ c1: 0 });
    rerender({ chats: [chat('c1', { turns: 1, state: 'idle' })], shown: 'c1' });
    expect(result.current).toEqual({ c1: 1 });
  });
});

describe('the crumb', () => {
  const providersK = [{ name: 'prov-a', kind: 'anthropic', models: [] }];
  it('is the chat title, then the model display name', () => {
    const c = chat('c1', { title: 'What changed this week?', name: 'Sonnet 5.5' });
    const t = targetLabelFor({ kind: 'chat', id: 'c1' } as never, [], [], [], null, [c], providersK);
    expect(t.name).toBe('What changed this week?');
    expect(t.sub).toBe('Sonnet 5.5');
  });

  it('is the model name alone before the first turn', () => {
    const t = targetLabelFor({ kind: 'chat', id: 'c1' } as never, [], [], [], null, [chat('c1', { title: '', name: 'Sonnet 5.5' })], providersK);
    expect(t.name).toBe('Sonnet 5.5');
    expect(t.sub).toBeUndefined();
  });

  it('the top bar draws the model name in grey after the title', () => {
    render(<TopBar state={{ ...init, repo: 'alpha', context: { kind: 'repo', repo: 'alpha' } }} repos={[]} railOpen={false} onToggleRail={() => {}} onGear={() => {}} facets={['chat', 'settings']} facet="chat"
      label={{ kind: 'chat', name: 'What changed this week?', sub: 'Sonnet 5.5', id: 'c1' }} />);
    expect(screen.getByTestId('target-label')).toHaveTextContent('What changed this week?Sonnet 5.5');
    const sub = screen.getByTestId('target-label-sub');
    expect(sub).toHaveTextContent('Sonnet 5.5');
    expect(sub.style.color).toBe('rgb(122, 122, 122)');
  });
});
