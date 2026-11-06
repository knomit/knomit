import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor, cleanup, within } from '@testing-library/react';
import { ChatStartForm } from './ChatStartForm';
import { costTier } from './chatState';
import type { ChatProvider } from './chatApi';
import {
  CHAT_NO_MODELS, START_HEADING, START_HINT, START_INTRO, SUGGEST_BIND, SUGGEST_CHANGED, startPlaceholder,
  suggestContradictions, suggestReview,
} from './chatCopy';

// Every number on a card is a value the server sent; the fixtures use odd
// figures so a price or window kept in code could not pass.
const providers: ChatProvider[] = [
  { name: 'anthropic', kind: 'anthropic', key_source: 'MY_KEY_VAR', reachable: true, models: [
    {
      id: 'a1', name: 'Alpha', available: true, tools: true, best_for: 'Quick things', context_window: 640_000, effort: 'low',
      price: { tiers: [{ up_to_prompt_tokens: 120_000, input_per_mtok: 0.37, output_per_mtok: 1.91 }, { up_to_prompt_tokens: 0, input_per_mtok: 0.8, output_per_mtok: 4 }], cache_read_mult: 0.25, cache_write_5m_mult: 1.25, warn_prompt_tokens: 100_000 },
    },
    { id: 'a2', available: false, tools: false, reason: 'not offered by the provider', price: { tiers: [{ up_to_prompt_tokens: 0, input_per_mtok: 5, output_per_mtok: 25 }], cache_read_mult: 0.1, cache_write_5m_mult: 1.25, warn_prompt_tokens: 0 } },
  ] },
  { name: 'local', kind: 'ollama', host: 'http://box.lan:11434', reachable: true, models: [
    { id: 'o1', name: 'Oh One', available: true, tools: true, num_ctx: 24576, parameter_size: '3.9B', quantization: 'Q5_K_S', keep_alive: '7m' },
    { id: 'o2', available: true, tools: false, reason: 'this model cannot call tools' },
  ] },
];

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

const created = { id: 'chat-new', provider: 'anthropic', model: 'a1', name: 'Alpha', turn: 0 };
const stubCreate = () => {
  const fetchMock = vi.fn<(url: string, init?: RequestInit) => Promise<Response>>(async () =>
    new Response(JSON.stringify(created), { status: 201 }));
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
};

describe('ChatStartForm', () => {
  it('shows the heading and intro, and a block per provider with where it runs and its reachability', () => {
    render(<ChatStartForm providers={providers} onCreated={vi.fn()} />);
    expect(screen.getByRole('heading')).toHaveTextContent(START_HEADING);
    expect(screen.getByText(START_INTRO)).toBeInTheDocument();
    expect(screen.getByTestId('chat-provider-where-anthropic')).toHaveTextContent('cloud · MY_KEY_VAR');
    expect(screen.getByTestId('chat-provider-where-local')).toHaveTextContent('box.lan:11434');
    expect(screen.getByTestId('chat-provider-reach-anthropic')).toHaveTextContent('connected');
    expect(screen.getByTestId('chat-provider-reach-local')).toHaveTextContent('running');
  });

  it('a card shows the server\'s values: name, id, best for, tags and stats', () => {
    render(<ChatStartForm providers={providers} onCreated={vi.fn()} />);
    const a1 = screen.getByTestId('chat-model-anthropic-a1');
    expect(a1).toHaveTextContent('Alpha');
    expect(screen.getByTestId('chat-model-id-anthropic-a1')).toHaveTextContent('a1');
    expect(screen.getByTestId('chat-model-bestfor-anthropic-a1')).toHaveTextContent('Quick things');
    expect(a1).toHaveTextContent('Default');
    expect(a1).toHaveTextContent('Tools');
    expect(a1).toHaveTextContent('Price tier at 120K');
    expect(a1).toHaveTextContent('Input$0.37 / MTok');
    expect(a1).toHaveTextContent('Output$1.91 / MTok');
    expect(a1).toHaveTextContent('Cache read$0.09 · 0.25×');
    expect(a1).toHaveTextContent('Context640K tokens');
    expect(a1).toHaveTextContent('Effortlow');

    const o1 = screen.getByTestId('chat-model-local-o1');
    expect(o1).toHaveTextContent('Oh One');
    expect(o1).toHaveTextContent('Ollama');
    expect(o1).toHaveTextContent('free');
    expect(o1).toHaveTextContent('Costnone');
    expect(o1).toHaveTextContent('Context24K (num_ctx)');
    expect(o1).toHaveTextContent('Size3.9B · Q5_K_S');
    expect(o1).toHaveTextContent('Keep alive7m');
    expect(o1).not.toHaveTextContent('Default');
  });

  it('the cost mark follows the input price: under 1, under 3, otherwise', () => {
    expect([0.99, 1, 2.99, 3, 20].map(costTier)).toEqual([1, 2, 2, 3, 3]);
    render(<ChatStartForm providers={[{ name: 'p', kind: 'anthropic', models: [0.37, 2.5, 4].map((input, i) => ({
      id: `m${i}`, available: true, tools: true,
      price: { tiers: [{ up_to_prompt_tokens: 0, input_per_mtok: input, output_per_mtok: 1 }], cache_read_mult: 0.1, cache_write_5m_mult: 1.25, warn_prompt_tokens: 0 },
    })) }]} onCreated={vi.fn()} />);
    expect(screen.getByTestId('chat-model-cost-p-m0')).toHaveAttribute('aria-label', '$');
    expect(screen.getByTestId('chat-model-cost-p-m1')).toHaveAttribute('aria-label', '$$');
    expect(screen.getByTestId('chat-model-cost-p-m2')).toHaveAttribute('aria-label', '$$$');
  });

  it('the Default model (the first usable one) is selected when the screen opens, and the composer names it', () => {
    render(<ChatStartForm providers={providers} onCreated={vi.fn()} />);
    expect(screen.getByTestId('chat-model-anthropic-a1')).toHaveAttribute('aria-checked', 'true');
    expect(screen.getByTestId('chat-model-local-o1')).toHaveAttribute('aria-checked', 'false');
    expect(screen.getByTestId('chat-start-input')).toHaveAttribute('placeholder', startPlaceholder('Alpha'));
    expect(screen.getByTestId('chat-start-chip')).toHaveTextContent('Alpha');
    expect(screen.getByText(START_HINT)).toBeInTheDocument();
  });

  it('a no-tools and an unavailable model are dimmed, show the server\'s reason, and cannot be picked', () => {
    const fetchMock = stubCreate();
    render(<ChatStartForm providers={providers} onCreated={vi.fn()} />);
    const a2 = screen.getByTestId('chat-model-anthropic-a2');
    const o2 = screen.getByTestId('chat-model-local-o2');
    for (const c of [a2, o2]) {
      expect(c).toBeDisabled();
      expect(c).toHaveStyle({ opacity: '0.5' });
    }
    expect(screen.getByTestId('chat-model-reason-anthropic-a2')).toHaveTextContent('not offered by the provider');
    expect(screen.getByTestId('chat-model-reason-local-o2')).toHaveTextContent('this model cannot call tools');
    expect(o2).toHaveTextContent('No tools');
    expect(a2).not.toHaveTextContent('No tools');
    fireEvent.click(a2);
    fireEvent.click(o2);
    expect(screen.getByTestId('chat-model-anthropic-a1')).toHaveAttribute('aria-checked', 'true');
    expect(a2).toHaveAttribute('aria-checked', 'false');
    fireEvent.change(screen.getByTestId('chat-start-input'), { target: { value: 'hi' } });
    fireEvent.click(screen.getByTestId('chat-start-go'));
    expect(JSON.parse(String(fetchMock.mock.calls[0][1]?.body))).toMatchObject({ provider: 'anthropic', model: 'a1' });
  });

  it('clicking a card selects it and creates nothing; Enter then creates the chat with the first message', async () => {
    const fetchMock = stubCreate();
    const onCreated = vi.fn();
    render(<ChatStartForm providers={providers} onCreated={onCreated} />);
    fireEvent.click(screen.getByTestId('chat-model-local-o1'));
    expect(screen.getByTestId('chat-model-local-o1')).toHaveAttribute('aria-checked', 'true');
    expect(screen.getByTestId('chat-model-anthropic-a1')).toHaveAttribute('aria-checked', 'false');
    expect(screen.getByTestId('chat-start-input')).toHaveAttribute('placeholder', startPlaceholder('Oh One'));
    expect(fetchMock).not.toHaveBeenCalled();

    const box = screen.getByTestId('chat-start-input');
    fireEvent.change(box, { target: { value: 'first words' } });
    expect(fetchMock).not.toHaveBeenCalled();
    fireEvent.keyDown(box, { key: 'Enter', shiftKey: true });
    expect(fetchMock).not.toHaveBeenCalled();
    fireEvent.keyDown(box, { key: 'Enter' });
    await waitFor(() => expect(onCreated).toHaveBeenCalledWith(created));
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe('/api/v1/chats');
    expect(init?.method).toBe('POST');
    expect(JSON.parse(String(init?.body))).toEqual({ provider: 'local', model: 'o1', message: 'first words' });
  });

  it('Start chat does the same, and needs a message', async () => {
    const fetchMock = stubCreate();
    const onCreated = vi.fn();
    render(<ChatStartForm providers={providers} onCreated={onCreated} />);
    expect(screen.getByTestId('chat-start-go')).toBeDisabled();
    fireEvent.keyDown(screen.getByTestId('chat-start-input'), { key: 'Enter' });
    expect(fetchMock).not.toHaveBeenCalled();
    fireEvent.change(screen.getByTestId('chat-start-input'), { target: { value: 'go' } });
    fireEvent.click(screen.getByTestId('chat-start-go'));
    await waitFor(() => expect(onCreated).toHaveBeenCalledTimes(1));
    expect(JSON.parse(String(fetchMock.mock.calls[0][1]?.body))).toEqual({ provider: 'anthropic', model: 'a1', message: 'go' });
  });

  it('suggestion chips fill the composer and do not send', () => {
    const fetchMock = stubCreate();
    render(<ChatStartForm providers={providers} repoNames={['r-one', 'r-two']} onCreated={vi.fn()} />);
    const chips = within(screen.getByTestId('chat-suggestions')).getAllByRole('button').map(b => b.textContent);
    expect(chips).toEqual([SUGGEST_CHANGED, SUGGEST_BIND, suggestReview('r-one'), suggestContradictions('r-two')]);
    fireEvent.click(screen.getByText(SUGGEST_BIND));
    expect(screen.getByTestId('chat-start-input')).toHaveValue(SUGGEST_BIND);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('shows the server\'s refusal, keeps the text and starts no chat', async () => {
    vi.stubGlobal('fetch', vi.fn(async () =>
      new Response(JSON.stringify({ title: 'Cannot start the chat', detail: 'no key' }), { status: 422 })));
    const onCreated = vi.fn();
    render(<ChatStartForm providers={providers} onCreated={onCreated} />);
    fireEvent.change(screen.getByTestId('chat-start-input'), { target: { value: 'keep me' } });
    fireEvent.click(screen.getByTestId('chat-start-go'));
    expect(await screen.findByTestId('chat-start-error')).toHaveTextContent('422');
    expect(onCreated).not.toHaveBeenCalled();
    expect(screen.getByTestId('chat-start-input')).toHaveValue('keep me');
  });

  it('says so when no provider lists a model', () => {
    render(<ChatStartForm providers={[{ name: 'anthropic', kind: 'anthropic', models: [] }]} onCreated={vi.fn()} />);
    expect(screen.getByTestId('chat-no-models')).toHaveTextContent(CHAT_NO_MODELS);
  });

  it('when every model is unavailable they are all shown with the server\'s reason and nothing can start', () => {
    const down: ChatProvider[] = [{ name: 'anthropic', kind: 'anthropic', key_source: 'UNSET_VAR', reachable: false, models: [
      { id: 'a1', available: false, tools: false, reason: 'no API key configured' },
      { id: 'a2', available: false, tools: false, reason: 'no API key configured' },
    ] }];
    render(<ChatStartForm providers={down} onCreated={vi.fn()} />);
    expect(screen.queryByTestId('chat-no-models')).toBeNull();
    expect(screen.getByTestId('chat-model-reason-anthropic-a1')).toHaveTextContent('no API key configured');
    expect(screen.getByTestId('chat-model-reason-anthropic-a2')).toHaveTextContent('no API key configured');
    expect(screen.getByTestId('chat-provider-where-anthropic')).toHaveTextContent('cloud · UNSET_VAR');
    expect(screen.getByTestId('chat-provider-reach-anthropic')).toHaveAttribute('data-reachable', 'false');
    expect(screen.getByTestId('chat-start-input')).toBeDisabled();
    expect(screen.getByTestId('chat-start-go')).toBeDisabled();
    expect(screen.queryByTestId('chat-start-chip')).toBeNull();
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
