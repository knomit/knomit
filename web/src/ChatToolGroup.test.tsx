import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, act, fireEvent, cleanup, within } from '@testing-library/react';
import { ChatFacet } from './ChatFacet';
import { ChatToolGroup } from './ChatToolGroup';
import { installFakeEventSource, uninstallFakeEventSource, chatStream } from './testEventSource';
import { stubFleetFetch } from './fleetTestFetch';
import type { ChatApproval, ChatSnapshot } from './chatApi';
import { APPROVAL_TARGET_UNRESOLVED } from './chatCopy';
import type { ToolCall } from './chatState';
import { computed } from './cascade.testutil';
import { readFileSync } from 'node:fs';

const CHAT = { id: 'chat-a', provider: 'anthropic', model: 'claude-test-x' };
const BASE = '/api/v1/chats/chat-a';

const learn: ChatApproval = {
  call_id: 'tu_2', tool: 'knomit_learn', action: 'learn', target: 'alpha', target_kind: 'repo',
  destructive: false, grantable: true, arguments: { facts: [{ title: 't' }] }, turn: 0,
};
const snap = (ap: ChatApproval[], name = 'knomit_learn'): ChatSnapshot => ({
  ...CHAT, running: true, grants: [], pending_approvals: ap,
  items: [
    { kind: 'user', text: 'remember this' },
    { kind: 'tool_call', id: 'tu_2', name, state: 'running' },
  ],
});

beforeEach(() => { installFakeEventSource(); });
afterEach(() => { cleanup(); uninstallFakeEventSource(); vi.unstubAllGlobals(); });

describe('approvals in a tool row', () => {
  it('buttons post the decision, and a waiting approval holds its group open', async () => {
    const fx = stubFleetFetch({
      [`GET ${BASE}`]: { body: snap([learn]) },
      [`POST ${BASE}/approvals/tu_2`]: { status: 204 },
    });
    render(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} />);
    // The group is collapsed by default, but this one has a card waiting.
    const card = await screen.findByTestId('chat-approval-tu_2');
    expect(screen.getByTestId('chat-tool-group')).toHaveAttribute('data-open', 'true');
    expect(within(screen.getByTestId('chat-tool-row-tu_2')).getByTestId('chat-approval-tu_2')).toBe(card);
    expect(screen.getByTestId('chat-approval-action')).toHaveTextContent('learn');
    expect(screen.getByTestId('chat-approval-target')).toHaveTextContent('alpha');
    expect(screen.getByTestId('chat-approval-allow')).toHaveTextContent(/^Allow$/);
    expect(screen.getByTestId('chat-approval-decline')).toHaveTextContent(/^Decline$/);
    expect(screen.getByTestId('chat-approval-allow-session')).toHaveTextContent(/^Allow for session$/);

    fireEvent.click(screen.getByTestId('chat-approval-allow-session'));
    await waitFor(() => expect(screen.queryByTestId('chat-approval-tu_2')).toBeNull());
    const posts = fx.calls.filter(c => c.route === `POST ${BASE}/approvals/tu_2`);
    expect(posts).toHaveLength(1);
    expect(posts[0].body).toEqual({ decision: 'allow_session' });
  });

  it('Allow and Decline post their own decisions', async () => {
    for (const [id, decision] of [['chat-approval-allow', 'allow'], ['chat-approval-decline', 'decline']] as const) {
      const fx = stubFleetFetch({
        [`GET ${BASE}`]: { body: snap([learn]) },
        [`POST ${BASE}/approvals/tu_2`]: { status: 204 },
      });
      const { unmount } = render(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} />);
      fireEvent.click(await screen.findByTestId(id));
      await waitFor(() => expect(fx.calls.filter(c => c.route.startsWith('POST'))).toHaveLength(1));
      expect(fx.calls.find(c => c.route.startsWith('POST'))?.body).toEqual({ decision });
      unmount();
      cleanup();
    }
  });

  it('an approval event arriving on the stream shows its card in the row', async () => {
    stubFleetFetch({ [`GET ${BASE}`]: { body: { ...snap([]), running: true } } });
    render(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} />);
    await screen.findByTestId('chat-tool-group');
    expect(screen.queryByTestId('chat-approval-tu_2')).toBeNull();
    act(() => { chatStream('chat-a')!.emit('approval', learn); });
    expect(await screen.findByTestId('chat-approval-tu_2')).toBeInTheDocument();
  });

  it('a takeover approval row shows the same three buttons', async () => {
    const takeover: ChatApproval = { ...learn, tool: 'knomit_review', action: 'takeover', destructive: true };
    stubFleetFetch({ [`GET ${BASE}`]: { body: snap([takeover], 'knomit_review') } });
    render(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} />);
    const card = await screen.findByTestId('chat-approval-tu_2');
    expect(screen.getByTestId('chat-tool-row-tu_2')).toHaveTextContent('knomit_review');
    expect(screen.getByTestId('chat-approval-action')).toHaveTextContent('takeover');
    expect(within(card).getAllByRole('button').map(b => b.textContent)).toEqual(['Allow', 'Decline', 'Allow for session']);
  });

  it('N14: an approval whose target the server could not resolve names it, never shows it empty', async () => {
    const unresolved: ChatApproval = { ...learn, target: '', target_kind: '', grantable: false };
    stubFleetFetch({ [`GET ${BASE}`]: { body: snap([unresolved]) } });
    render(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} />);
    await screen.findByTestId('chat-approval-tu_2');
    expect(screen.getByTestId('chat-approval-target').textContent).toBe(APPROVAL_TARGET_UNRESOLVED);
    expect(APPROVAL_TARGET_UNRESOLVED).not.toBe('');
    // The server will not grant it either, so the page does not offer to.
    expect(screen.queryByTestId('chat-approval-allow-session')).toBeNull();
    expect(screen.getByTestId('chat-approval-allow')).toBeInTheDocument();
  });

  it('a 404 on the answer (no longer pending) clears the stale card', async () => {
    stubFleetFetch({
      [`GET ${BASE}`]: { body: snap([learn]) },
      [`POST ${BASE}/approvals/tu_2`]: { status: 404, body: { detail: 'no approval is pending for that call' } },
    });
    render(<ChatFacet chat={CHAT} facet="chat" onGone={vi.fn()} />);
    fireEvent.click(await screen.findByTestId('chat-approval-decline'));
    await waitFor(() => expect(screen.queryByTestId('chat-approval-tu_2')).toBeNull());
  });
});

// Chat review, 2026-11-09, option B1: one grey card for every approval; a
// destructive one is marked by its action word alone, in amber; the answers
// are one line of text links split by a muted dot, Allow in green.
// SABOTAGE: give the card back its red ground for a destructive approval, draw
// the action word #ccc when destructive, or put the answers back in bordered
// buttons -> red.
describe('the approval card (B1)', () => {
  const cardFor = (ap: ChatApproval) => {
    render(<ChatToolGroup lead={[]} calls={[{ id: ap.call_id, name: ap.tool, state: 'running', isError: false } as ToolCall]}
      approvals={{ [ap.call_id]: ap }} selectedId={null} onSelect={vi.fn()} onAnswer={vi.fn()} />);
    return screen.getByTestId(`chat-approval-${ap.call_id}`);
  };

  it('a destructive approval is the same grey card; only its action word is amber', () => {
    const plain = cardFor(learn);
    const destructive = cardFor({ ...learn, call_id: 'tu_9', tool: 'knomit_review', action: 'takeover', destructive: true });
    // Reached: one card of each kind, and it says which it is.
    expect(plain).not.toHaveAttribute('data-destructive');
    expect(destructive).toHaveAttribute('data-destructive', 'true');
    for (const card of [plain, destructive]) {
      expect(card.style.background).toBe('rgb(26, 26, 26)'); // #1a1a1a
      expect(card.style.border).toBe('1px solid rgb(42, 42, 42)'); // #2a2a2a
    }
    expect(within(plain).getByTestId('chat-approval-action').style.color).toBe('rgb(204, 204, 204)'); // #ccc
    expect(within(destructive).getByTestId('chat-approval-action').style.color).toBe('rgb(232, 194, 122)'); // AMBER #e8c27a
    expect(within(destructive).getByTestId('chat-approval-action')).toHaveTextContent(/^takeover$/);
  });

  it('the answers are one line of text links: Allow green, the rest grey, split by aria-hidden dots', () => {
    const card = cardFor(learn);
    const line = within(card).getByTestId('chat-approval-answers');
    // In order, the three answers with a dot between each pair, and nothing else.
    expect([...line.children].map(c => c.textContent)).toEqual(['Allow', '·', 'Decline', '·', 'Allow for session']);
    const seps = [...line.children].filter(c => c.tagName === 'SPAN') as HTMLElement[];
    expect(seps).toHaveLength(2);
    for (const s of seps) {
      expect(s).toHaveAttribute('aria-hidden', 'true');
      expect(s.style.color).toBe('rgb(68, 68, 68)'); // #444
    }
    const [allow, decline, session] = ['chat-approval-allow', 'chat-approval-decline', 'chat-approval-allow-session'].map(id => within(card).getByTestId(id));
    for (const b of [allow, decline, session]) {
      expect(b.tagName).toBe('BUTTON');
      expect(b.getAttribute('style')).toBeNull(); // drawn by App.css's .k-approval-answer, not inline
      expect(computed(b, 'border-style', 1280)).toBe('none');
      expect(computed(b, 'background', 1280)).toBe('none');
      expect(computed(b, 'padding', 1280)).toBe('1px 5px');
      expect(computed(b, 'border-radius', 1280)).toBe('3px');
    }
    expect(computed(allow, 'color', 1280)).toBe('rgb(159, 212, 180)'); // #9fd4b4
    expect(computed(decline, 'color', 1280)).toBe('rgb(138, 138, 138)'); // #8a8a8a
    expect(computed(session, 'color', 1280)).toBe('rgb(138, 138, 138)');
  });

  it('a call the server cannot grant has two answers and one dot', () => {
    const card = cardFor({ ...learn, grantable: false });
    expect([...within(card).getByTestId('chat-approval-answers').children].map(c => c.textContent)).toEqual(['Allow', '·', 'Decline']);
  });

  it('hover lifts an answer; keyboard focus keeps a visible ring', () => {
    // jsdom matches no :hover or :focus-visible, so the rules themselves are read.
    const rules = [...cssRules()].filter(r => r.selectorText?.startsWith('.k-approval-answer:'));
    const hover = rules.find(r => r.selectorText === '.k-approval-answer:hover')!;
    expect(hover.style.getPropertyValue('color')).toBe('rgb(226, 226, 226)'); // #e2e2e2
    expect(hover.style.getPropertyValue('background')).toBe('rgb(35, 35, 35)'); // #232323
    const ring = rules.find(r => r.selectorText === '.k-approval-answer:focus-visible')!;
    expect(ring.style.getPropertyValue('outline')).toBe('2px solid #4a7ab5');
  });
});

function cssRules(): CSSStyleRule[] {
  const style = document.createElement('style');
  style.textContent = readFileSync('src/App.css', 'utf8');
  document.head.appendChild(style);
  try {
    return [...style.sheet!.cssRules].filter((r): r is CSSStyleRule => 'selectorText' in r);
  } finally {
    style.remove();
  }
}

describe('the loader row', () => {
  const group = (calls: ToolCall[]) => render(<ChatToolGroup lead={[]} approvals={{}} selectedId={null} onSelect={vi.fn()} onAnswer={vi.fn()} calls={calls} />);

  it('words a call to the loader like any other call: its title and the main argument the server sent', () => {
    group([
      { id: 'tu_1', name: 'load_tools', title: 'Load tools', keyArg: 'knomit_query, knomit_explain', state: 'done', isError: false, loaded: ['knomit_query', 'knomit_explain'] },
      { id: 'tu_2', name: 'knomit_query', title: 'Query', state: 'done', isError: false },
    ]);
    fireEvent.click(screen.getByTestId('chat-tool-group-toggle'));
    expect(screen.getByTestId('chat-tool-row-tu_1')).toHaveTextContent('Load tools·knomit_query, knomit_explain');
    expect(screen.getByTestId('chat-tool-row-tu_2')).toHaveTextContent(/^Query$/);
  });

  it('a loader call that loaded nothing is its title alone', () => {
    group([{ id: 'tu_1', name: 'load_tools', title: 'Load tools', state: 'done', isError: true }]);
    fireEvent.click(screen.getByTestId('chat-tool-group-toggle'));
    expect(screen.getByTestId('chat-tool-row-tu_1')).toHaveTextContent(/^Load tools$/);
  });
});

describe('ChatToolGroup', () => {
  it('a finished call offers its result; a failed one is marked', () => {
    const onSelect = vi.fn();
    render(<ChatToolGroup lead={[]} approvals={{}} selectedId={null} onSelect={onSelect} onAnswer={vi.fn()}
      calls={[{ id: 'tu_1', name: 'knomit_query', state: 'done', isError: true }]} />);
    fireEvent.click(screen.getByTestId('chat-tool-group-toggle'));
    const row = screen.getByTestId('chat-tool-row-tu_1');
    expect(row).toHaveAttribute('data-error', 'true');
    fireEvent.click(screen.getByTestId('chat-tool-select-tu_1'));
    expect(onSelect).toHaveBeenCalledWith({ id: 'tu_1', name: 'knomit_query', state: 'done', isError: true });
  });
});
