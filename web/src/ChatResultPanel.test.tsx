import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor, cleanup, fireEvent } from '@testing-library/react';
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
    expect(screen.getByTestId('chat-result-title')).toHaveTextContent('Read');
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
    // The arguments are on their own tab, not stacked under the result.
    expect(screen.queryByTestId('chat-result-args')).toBeNull();
    fireEvent.click(screen.getByTestId('chat-result-tab-arguments'));
    expect(screen.getByTestId('chat-result-args')).toHaveTextContent('"text": "x"');
    expect(screen.queryByTestId('chat-result-text')).toBeNull();
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

// A knomit_materialize result as the live server returned it (audience agent,
// recorded from a real chat log; the kb path is repo-relative).
const REAL_MATERIALIZE_RESULT = '{"root":"kb/technology/coffee/moka/ec80f23a.md","anchor":"355a1022a2dec4daf739b4fcc9a9706260f34ef3","order":"refs","audience":"agent","unpinned":false,"cross_kb":false,"depth":4,"max_nodes":200,"total":1,"truncated":{"depth":0,"nodes":0},"text":"# Moka pot: remove from heat when the stream turns pale and sputters\\n\\nMaterialized from `kb/technology/coffee/moka/ec80f23a.md` at commit 355a102.\\n1 knowledge-base fact, each reproduced in full and unedited, in reading order.\\n\\n\\u003ca id=\\"f-dcf7b84d36e2a394\\"\\u003e\\u003c/a\\u003e\\n### Moka pot: remove from heat when the stream turns pale and sputters\\n`ec80f23a` · 2026-11-05 · well-established · observation\\n\\nA moka pot should come off the heat the moment the stream turns pale and starts to sputter.\\n","n_from":1,"n_to":1,"cursor":null,"has_more":false}';

describe('ChatResultPanel real materialize result', () => {
  it('shows the fact, not the anchor tag or the result metadata', async () => {
    stubFleetFetch({
      [`GET ${BASE}/calls/tu_6`]: { body: { name: 'knomit_materialize', arguments: { file: 'kb/technology/coffee/moka/ec80f23a.md', audience: 'agent' }, result: REAL_MATERIALIZE_RESULT, is_error: false, elided: false } },
    });
    render(<ChatResultPanel chatId="chat-a" callId="tu_6" />);
    const fact = await screen.findByTestId('chat-result-fact');
    expect(fact.textContent).not.toContain('<a id');
    expect(fact.textContent).not.toContain('f-dcf7b84d36e2a394');
    expect(fact.textContent).not.toContain('"root"');
    expect(fact.textContent).toContain('A moka pot should come off the heat');
    expect(fact.querySelectorAll('h3')).toHaveLength(1);
  });

  it('materializedMarkdown drops anchor lines from a document and from human facts', () => {
    const md = materializedMarkdown(REAL_MATERIALIZE_RESULT);
    expect(md).not.toMatch(/<a\s/);
    expect(md).toContain('### Moka pot: remove from heat');
    expect(materializedMarkdown(JSON.stringify({ facts: [{ title: 'T', body: '<a id="f-1"></a>\nbody' }] }))).toBe('# T\n\nbody');
  });

  it('keeps an anchor tag inside a fenced code block, where it is the fact\'s own content', () => {
    const doc = 'before\n<a id="f-1"></a>\n### T\n\n```html\n<a id="example"></a>\n```\n\n~~~\n<a name="x"></a>\n~~~\nafter\n';
    expect(materializedMarkdown(JSON.stringify({ text: doc }))).toBe(
      'before\n### T\n\n```html\n<a id="example"></a>\n```\n\n~~~\n<a name="x"></a>\n~~~\nafter\n');
  });
});
