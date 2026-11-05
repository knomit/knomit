import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/react';
import { ChatSettings } from './ChatSettings';
import { stubFleetFetch } from './fleetTestFetch';
import { CHAT_NO_GRANTS, CHAT_REVOKE_GRANT_DRAFT } from './chatCopy';

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
    expect(await screen.findByTestId('chat-grants-empty')).toHaveTextContent(CHAT_NO_GRANTS);
    expect(screen.queryByTestId('chat-grant-row')).toBeNull();
  });

  it('says why when the grants cannot be read', async () => {
    stubFleetFetch({ [`GET ${BASE}/grants`]: { status: 404, body: { detail: 'no such chat' } } });
    render(<ChatSettings chatId="chat-a" />);
    expect(await screen.findByRole('alert')).toHaveTextContent('no such chat');
    expect(screen.queryByTestId('chat-grants-empty')).toBeNull();
  });

  const ALPHA = { target: 'alpha', kind: 'repo', tool: 'knomit_learn', action: 'learn' };
  const BETA = { target: 'beta', kind: 'repo', tool: 'knomit_learn', action: 'learn' };

  it('a Revoke button on each row sends that grant and removes only that row', async () => {
    const fx = stubFleetFetch({
      [`GET ${BASE}/grants`]: { body: { _embedded: { grants: [ALPHA, BETA] } } },
      [`DELETE ${BASE}/grants`]: { status: 204 },
    });
    render(<ChatSettings chatId="chat-a" />);
    await screen.findAllByTestId('chat-grant-row');
    const buttons = screen.getAllByTestId('chat-grant-revoke');
    expect(buttons).toHaveLength(2);
    expect(buttons[0]).toHaveTextContent(CHAT_REVOKE_GRANT_DRAFT);

    fireEvent.click(buttons[0]);
    await waitFor(() => expect(screen.getAllByTestId('chat-grant-row')).toHaveLength(1));
    expect(screen.getByTestId('chat-grant-target')).toHaveTextContent('beta');
    const dels = fx.calls.filter(c => c.route === `DELETE ${BASE}/grants`);
    expect(dels).toHaveLength(1);
    expect(dels[0].body).toEqual(ALPHA);
  });

  it('a revoke the server answers 404 (no longer held) drops the stale row; another failure keeps it and says why', async () => {
    const fx = stubFleetFetch({
      [`GET ${BASE}/grants`]: { body: { _embedded: { grants: [ALPHA, BETA] } } },
      [`DELETE ${BASE}/grants`]: { status: 404, body: { detail: 'no such grant' } },
    });
    render(<ChatSettings chatId="chat-a" />);
    await screen.findAllByTestId('chat-grant-row');
    fireEvent.click(screen.getAllByTestId('chat-grant-revoke')[0]);
    await waitFor(() => expect(screen.getAllByTestId('chat-grant-row')).toHaveLength(1));

    fx.set(`DELETE ${BASE}/grants`, { status: 500, body: { detail: 'store unavailable' } });
    fireEvent.click(screen.getByTestId('chat-grant-revoke'));
    expect(await screen.findByRole('alert')).toHaveTextContent('store unavailable');
    expect(screen.getAllByTestId('chat-grant-row')).toHaveLength(1);
  });
});
