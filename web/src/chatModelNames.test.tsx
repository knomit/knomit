import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, cleanup } from '@testing-library/react';
import { ChatStartForm } from './ChatStartForm';
import { TargetRail } from './TargetRail';
import { targetLabelFor } from './facets';
import type { ChatInfo, ChatProvider } from './chatApi';

// A model shows its configured name everywhere a person sees it; the id is the
// identity (data-testid, the chat's model) and the title tooltip. A model with
// no name shows its id.
const providers: ChatProvider[] = [
  { name: 'anthropic', kind: 'anthropic', models: [
    { id: 'claude-haiku-5-5', name: 'Haiku 5.5', available: true, tools: true },
    { id: 'claude-sonnet-5-5', name: 'claude-sonnet-5-5', available: true, tools: true },
  ] },
];

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

describe('model names in the New chat picker', () => {
  it('shows the name, keeps the id as the test id and the title', () => {
    render(<ChatStartForm providers={providers} onCreated={() => {}} />);
    const named = screen.getByTestId('chat-model-anthropic-claude-haiku-5-5');
    expect(named).toHaveTextContent('Haiku 5.5');
    expect(named).not.toHaveTextContent('claude-haiku-5-5');
    expect(named).toHaveAttribute('title', 'claude-haiku-5-5');
    expect(screen.getByTestId('chat-model-anthropic-claude-sonnet-5-5')).toHaveTextContent('claude-sonnet-5-5');
  });
});

describe('model names in the breadcrumb', () => {
  const chat: ChatInfo = { id: 'chat-1', provider: 'anthropic', model: 'claude-haiku-5-5', name: 'Haiku 5.5' };
  it('names a chat target by its model name', () => {
    const t = targetLabelFor({ kind: 'chat', id: 'chat-1' } as never, [], [], [], null, [chat], providers);
    expect(t.name).toBe('Haiku 5.5');
  });
  it('falls back to the model id when the chat carries no name', () => {
    const t = targetLabelFor({ kind: 'chat', id: 'chat-1' } as never, [], [], [], null, [{ ...chat, name: '' }], providers);
    expect(t.name).toBe('claude-haiku-5-5');
  });
});

describe('model names in the left rail', () => {
  it('draws the chat row with its name, and the id when there is no name', () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response('[]', { status: 200 })));
    const chats: ChatInfo[] = [
      { id: 'chat-a', provider: 'anthropic', model: 'claude-haiku-5-5', name: 'Haiku 5.5' },
      { id: 'chat-b', provider: 'anthropic', model: 'lfm3:1.7b', name: 'lfm3:1.7b' },
    ];
    render(
      <TargetRail repos={[]} lenses={[]} context={{ kind: 'repo', repo: '' } as never} currentRepo=""
        remoteError="" serverReadOnly={false} onSelect={() => {}}
        chats={chats} chatProviders={providers} chatsAvailable />,
    );
    expect(screen.getByTestId('rail-chat-chat-a')).toHaveTextContent('Haiku 5.5');
    expect(screen.getByTestId('rail-chat-chat-a')).not.toHaveTextContent('claude-haiku-5-5');
    expect(screen.getByTestId('rail-chat-chat-b')).toHaveTextContent('lfm3:1.7b');
  });
});
