import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor, cleanup } from '@testing-library/react';
import { ChatStartForm } from './ChatStartForm';
import type { ChatProvider } from './chatApi';
import { CHAT_NO_MODELS } from './chatCopy';

const providers: ChatProvider[] = [
  { name: 'anthropic', kind: 'anthropic', models: [
    { id: 'a1', available: true, tools: true },
    { id: 'a2', available: false, tools: true, reason: 'not offered to this key' },
  ] },
  { name: 'local', kind: 'ollama', models: [
    { id: 'o1', available: true, tools: false, reason: 'this model cannot call tools' },
  ] },
];

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

describe('ChatStartForm', () => {
  it('picker shows only allowed available models and marks toolless ones unusable', async () => {
    const fetchMock = vi.fn<(url: string, init?: RequestInit) => Promise<Response>>(async () =>
      new Response(JSON.stringify({ id: 'chat-new', provider: 'anthropic', model: 'a1' }), { status: 201 }));
    vi.stubGlobal('fetch', fetchMock);
    const onCreated = vi.fn();
    render(<ChatStartForm providers={providers} onCreated={onCreated} />);

    expect(screen.getAllByTestId(/^chat-model-(anthropic|local)-/).map(b => b.getAttribute('data-testid')))
      .toEqual(['chat-model-anthropic-a1', 'chat-model-local-o1']);
    expect(screen.queryByTestId('chat-model-anthropic-a2')).toBeNull();
    expect(screen.getByTestId('chat-model-anthropic-a1')).toBeEnabled();
    expect(screen.getByTestId('chat-model-local-o1')).toBeDisabled();
    expect(screen.getByTestId('chat-model-reason-local-o1')).toHaveTextContent('this model cannot call tools');
    expect(screen.queryByTestId('chat-model-reason-anthropic-a2')).toBeNull();

    fireEvent.click(screen.getByTestId('chat-model-anthropic-a1'));
    await waitFor(() => expect(onCreated).toHaveBeenCalledWith({ id: 'chat-new', provider: 'anthropic', model: 'a1' }));
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe('/api/v1/chats');
    expect(init?.method).toBe('POST');
    expect(JSON.parse(String(init?.body))).toEqual({ provider: 'anthropic', model: 'a1' });
  });

  it('clicking the unusable model starts nothing', () => {
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    render(<ChatStartForm providers={providers} onCreated={vi.fn()} />);
    fireEvent.click(screen.getByTestId('chat-model-local-o1'));
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('shows the server\'s refusal and starts no chat', async () => {
    vi.stubGlobal('fetch', vi.fn(async () =>
      new Response(JSON.stringify({ title: 'Cannot start the chat', detail: 'no key' }), { status: 422 })));
    const onCreated = vi.fn();
    render(<ChatStartForm providers={providers} onCreated={onCreated} />);
    fireEvent.click(screen.getByTestId('chat-model-anthropic-a1'));
    expect(await screen.findByTestId('chat-start-error')).toHaveTextContent('422');
    expect(onCreated).not.toHaveBeenCalled();
  });

  it('says so when no provider offers a model', () => {
    render(<ChatStartForm providers={[{ name: 'anthropic', kind: 'anthropic', models: [{ id: 'a2', available: false, tools: true }] }]} onCreated={vi.fn()} />);
    expect(screen.getByTestId('chat-no-models')).toHaveTextContent(CHAT_NO_MODELS);
  });

  it('Cancel is offered only when the caller gives one', () => {
    const onCancel = vi.fn();
    const { rerender } = render(<ChatStartForm providers={providers} onCreated={vi.fn()} />);
    expect(screen.queryByTestId('chat-cancel')).toBeNull();
    rerender(<ChatStartForm providers={providers} onCreated={vi.fn()} onCancel={onCancel} />);
    fireEvent.click(screen.getByTestId('chat-cancel'));
    expect(onCancel).toHaveBeenCalledTimes(1);
  });
});
