import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, cleanup } from '@testing-library/react';
import { ChatSettings } from './ChatSettings';
import { stubFleetFetch } from './fleetTestFetch';
import { CHAT_NO_GRANTS_DRAFT } from './chatCopy';

const BASE = '/api/v1/chats/chat-a';
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

describe('ChatSettings', () => {
  it('lists the approval grants', async () => {
    const fx = stubFleetFetch({
      [`GET ${BASE}/grants`]: { body: { count: 1, _embedded: { grants: [{ target: 'alpha', kind: 'repo', tool: 'knomit_learn', action: 'learn' }] } } },
    });
    render(<ChatSettings chatId="chat-a" />);
    const row = await screen.findByTestId('chat-grant-row');
    expect(screen.getAllByTestId('chat-grant-row')).toHaveLength(1);
    expect(screen.getByTestId('chat-grant-target')).toHaveTextContent('alpha');
    expect(screen.getByTestId('chat-grant-tool')).toHaveTextContent('knomit_learn');
    expect(screen.getByTestId('chat-grant-action')).toHaveTextContent('learn');
    expect(row).toBeInTheDocument();
    expect(screen.queryByTestId('chat-grants-empty')).toBeNull();
    expect(fx.calls.map(c => c.route)).toEqual([`GET ${BASE}/grants`]);
  });

  it('shows an empty state when nothing has been allowed', async () => {
    stubFleetFetch({ [`GET ${BASE}/grants`]: { body: { count: 0, _embedded: { grants: [] } } } });
    render(<ChatSettings chatId="chat-a" />);
    expect(await screen.findByTestId('chat-grants-empty')).toHaveTextContent(CHAT_NO_GRANTS_DRAFT);
    expect(screen.queryByTestId('chat-grant-row')).toBeNull();
  });

  it('says why when the grants cannot be read', async () => {
    stubFleetFetch({ [`GET ${BASE}/grants`]: { status: 404, body: { detail: 'no such chat' } } });
    render(<ChatSettings chatId="chat-a" />);
    expect(await screen.findByRole('alert')).toHaveTextContent('no such chat');
    expect(screen.queryByTestId('chat-grants-empty')).toBeNull();
  });
});
