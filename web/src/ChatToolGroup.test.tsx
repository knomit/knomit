import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, act, fireEvent, cleanup, within } from '@testing-library/react';
import { ChatFacet } from './ChatFacet';
import { ChatToolGroup } from './ChatToolGroup';
import { installFakeEventSource, uninstallFakeEventSource, chatStream } from './testEventSource';
import { stubFleetFetch } from './fleetTestFetch';
import type { ChatApproval, ChatSnapshot } from './chatApi';
import { APPROVAL_TARGET_UNRESOLVED, loadedToolsRow } from './chatCopy';
import type { ToolCall } from './chatState';

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
    expect(screen.getByTestId('chat-tool-row-tu_2')).toHaveTextContent('knomit: Knomit review');
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

describe('the loader row', () => {
  const group = (calls: ToolCall[]) => render(<ChatToolGroup lead={[]} approvals={{}} selectedId={null} onSelect={vi.fn()} onAnswer={vi.fn()} calls={calls} />);

  it('words a call to the loader as the tools it loaded, and every other call as before', () => {
    group([
      { id: 'tu_1', name: 'load_tools', state: 'done', isError: false, loaded: ['knomit_query', 'knomit_explain'] },
      { id: 'tu_2', name: 'knomit_query', state: 'done', isError: false },
    ]);
    fireEvent.click(screen.getByTestId('chat-tool-group-toggle'));
    expect(screen.getByTestId('chat-tool-row-tu_1')).toHaveTextContent('Loaded tools: knomit_query, knomit_explain');
    expect(screen.getByTestId('chat-tool-row-tu_1')).toHaveTextContent(loadedToolsRow(['knomit_query', 'knomit_explain']));
    expect(screen.getByTestId('chat-tool-row-tu_2')).toHaveTextContent('Searched');
    expect(screen.getByTestId('chat-tool-row-tu_2')).not.toHaveTextContent('Loaded tools');
  });

  it('a loader call that loaded nothing is the ordinary row', () => {
    group([{ id: 'tu_1', name: 'load_tools', state: 'done', isError: true }]);
    fireEvent.click(screen.getByTestId('chat-tool-group-toggle'));
    expect(screen.getByTestId('chat-tool-row-tu_1')).toHaveTextContent('load_tools');
    expect(screen.getByTestId('chat-tool-row-tu_1')).not.toHaveTextContent('Loaded tools');
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
