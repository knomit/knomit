import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, cleanup, fireEvent, waitFor } from '@testing-library/react';
import { ChatSettings } from './ChatSettings';
import { stubFleetFetch } from './fleetTestFetch';
import { CHAT_NO_GRANTS, CHAT_REVOKE_GRANT, chatContextLine, chatSpillWarning } from './chatCopy';

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
    expect(fx.calls.map(c => c.route).sort()).toEqual([`GET ${BASE}/context`, `GET ${BASE}/grants`]);
  });

  it('shows the context window and how it was resolved, with the values the server sent', async () => {
    stubFleetFetch({
      [`GET ${BASE}/grants`]: { body: { count: 0, _embedded: { grants: [] } } },
      [`GET ${BASE}/context`]: { body: { window: 45056, how: 'memory', spill: false } },
    });
    render(<ChatSettings chatId="chat-a" />);
    expect(await screen.findByTestId('chat-context')).toHaveTextContent(chatContextLine(45056, 'memory'));
    expect(screen.getByTestId('chat-context')).toHaveTextContent("Context window 45056 tokens, limited to fit this machine's memory");
    expect(screen.queryByTestId('chat-context-spill')).toBeNull();
  });

  it('says "set in config" for a configured window and warns when the model is partly on the CPU', async () => {
    stubFleetFetch({
      [`GET ${BASE}/grants`]: { body: { count: 0, _embedded: { grants: [] } } },
      [`GET ${BASE}/context`]: { body: { window: 12288, how: 'config', spill: true } },
    });
    render(<ChatSettings chatId="chat-a" />);
    expect(await screen.findByTestId('chat-context')).toHaveTextContent('Context window 12288 tokens, set in config');
    expect(screen.getByTestId('chat-context-spill')).toHaveTextContent(chatSpillWarning(12288));
    expect(screen.getByTestId('chat-context-spill')).toHaveTextContent('Set num_ctx for this provider in knomit.toml to a smaller value.');
  });

  it('shows no context line when the server cannot say', async () => {
    stubFleetFetch({ [`GET ${BASE}/grants`]: { body: { count: 0, _embedded: { grants: [] } } } });
    render(<ChatSettings chatId="chat-a" />);
    await screen.findByTestId('chat-grants-empty');
    expect(screen.queryByTestId('chat-context')).toBeNull();
    expect(screen.queryByRole('alert')).toBeNull();
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
    expect(buttons[0]).toHaveTextContent(CHAT_REVOKE_GRANT);

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
