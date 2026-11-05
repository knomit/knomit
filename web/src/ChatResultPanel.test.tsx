import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor, cleanup } from '@testing-library/react';
import { ChatResultPanel } from './ChatResultPanel';
import { materializedMarkdown } from './chatState';
import { stubFleetFetch } from './fleetTestFetch';

const BASE = '/api/v1/chats/chat-a';
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

describe('ChatResultPanel', () => {
  it('a materialize result renders as a fact in the result panel', async () => {
    const doc = '# A decision\n\nWe chose **SSE** over a socket.\n';
    stubFleetFetch({
      [`GET ${BASE}/calls/tu_3`]: { body: { name: 'knomit_materialize', arguments: { path: 'kb/decisions/x.md', audience: 'agent' }, result: JSON.stringify({ text: doc, has_more: false }), is_error: false, elided: false } },
    });
    render(<ChatResultPanel chatId="chat-a" callId="tu_3" />);
    const fact = await screen.findByTestId('chat-result-fact');
    expect(fact.className).toContain('k-prose');
    expect(fact.querySelector('h1')).toHaveTextContent('A decision');
    expect(fact.querySelector('strong')).toHaveTextContent('SSE');
    expect(screen.getByTestId('chat-result-title')).toHaveTextContent('knomit: Knomit materialize');
    // Not shown as the raw JSON it travelled in.
    expect(fact.textContent).not.toContain('has_more');
    expect(screen.queryByTestId('chat-result-text')).toBeNull();
  });

  it('any other result is the text it is, and a failed one is marked', async () => {
    stubFleetFetch({
      [`GET ${BASE}/calls/tu_1`]: { body: { name: 'knomit_query', arguments: { text: 'x' }, result: 'no such repo', is_error: true, elided: false } },
    });
    render(<ChatResultPanel chatId="chat-a" callId="tu_1" />);
    const text = await screen.findByTestId('chat-result-text');
    expect(text).toHaveTextContent('no such repo');
    expect(text).toHaveAttribute('data-error', 'true');
    expect(screen.queryByTestId('chat-result-fact')).toBeNull();
    expect(screen.getByTestId('chat-result-args')).toHaveTextContent('"text": "x"');
  });

  it('shows the error when the call cannot be read', async () => {
    stubFleetFetch({ [`GET ${BASE}/calls/tu_9`]: { status: 404, body: { detail: 'no such call' } } });
    render(<ChatResultPanel chatId="chat-a" callId="tu_9" />);
    await waitFor(() => expect(screen.getByTestId('chat-result-error')).toHaveTextContent('no such call'));
  });

  it('materializedMarkdown reads the agent document, the human facts, or the text as it is', () => {
    expect(materializedMarkdown(JSON.stringify({ text: 'T' }))).toBe('T');
    expect(materializedMarkdown(JSON.stringify({ facts: [{ title: 'A', body: 'a' }, { title: 'B', body: 'b' }] }))).toBe('# A\n\na\n\n---\n\n# B\n\nb');
    expect(materializedMarkdown('plain words')).toBe('plain words');
    expect(materializedMarkdown('{"other":1}')).toBe('{"other":1}');
  });
});
