import { describe, it, expect, afterEach } from 'vitest';
import { render, cleanup } from '@testing-library/react';
import ReactMarkdown from 'react-markdown';
import { chatLivePlugins, chatPlugins, chatComponents } from './chatTableBars';

afterEach(cleanup);

const draw = (md: string, live = true) => render(
  <div data-testid="root">
    <ReactMarkdown remarkPlugins={live ? chatLivePlugins : chatPlugins} components={chatComponents}>{md}</ReactMarkdown>
  </div>,
).container.querySelector('[data-testid=root]')!;

/** The one caret, and the element it sits in. */
function caretIn(root: Element): Element {
  const carets = root.querySelectorAll('[data-testid=chat-caret]');
  expect(carets.length, 'exactly one caret').toBe(1);
  return carets[0].parentElement!;
}

// Where the caret goes: at the END of the last line of text (paragraph, heading,
// list item), so it follows the last word on the same line. A block with no text
// to end (code, table, rule) or no content at all gets a paragraph of its own,
// after it. SABOTAGE: append the caret to the root instead of the last block -> red.
describe('the streaming caret (chatLivePlugins)', () => {
  it('a paragraph: inside it, as its last child', () => {
    const root = draw('one\n\ntwo words');
    const p = caretIn(root);
    expect(p.tagName).toBe('P');
    expect(p.textContent).toBe('two words');
    expect(p.lastElementChild!.getAttribute('data-testid')).toBe('chat-caret');
  });

  it('a heading: inside it', () => {
    expect(caretIn(draw('intro\n\n#### Ranking')).tagName).toBe('H4');
  });

  it('a list: in the LAST item, not after the list', () => {
    const li = caretIn(draw('- a\n- b\n- c'));
    expect(li.tagName).toBe('LI');
    expect(li.textContent).toBe('c');
  });

  it('a nested list: in the last item of the innermost last list', () => {
    const li = caretIn(draw('- a\n  - inner one\n  - inner two'));
    expect(li.tagName).toBe('LI');
    expect(li.textContent).toBe('inner two');
  });

  it('a blockquote: in its last paragraph', () => {
    const p = caretIn(draw('> quoted\n> last line'));
    expect(p.tagName).toBe('P');
    expect(p.closest('blockquote')).not.toBeNull();
  });

  it('a code block: after it, in a paragraph of its own (code has no line to end)', () => {
    const root = draw('text\n\n```\nconst x = 1\n```');
    const p = caretIn(root);
    expect(p.tagName).toBe('P');
    expect(p.closest('pre')).toBeNull();
    expect(root.querySelector('pre')!.compareDocumentPosition(p) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('a table: after it, in a paragraph of its own', () => {
    const root = draw('| a | b |\n|---|---|\n| 1 | 2 |');
    const p = caretIn(root);
    expect(p.tagName).toBe('P');
    expect(p.closest('table')).toBeNull();
    expect(root.querySelector('table')!.compareDocumentPosition(p) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('a horizontal rule: after it, in a paragraph of its own', () => {
    const root = draw('above\n\n---');
    const p = caretIn(root);
    expect(p.tagName).toBe('P');
    expect(root.querySelector('hr')!.compareDocumentPosition(p) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('empty text: a lone paragraph holding just the caret', () => {
    const root = draw('');
    const p = caretIn(root);
    expect(p.tagName).toBe('P');
    expect(p.textContent).toBe('');
  });

  it('inline HTML: still exactly one caret, not inside the markup', () => {
    const root = draw('hello <b>bold</b> and <i>it');
    const parent = caretIn(root);
    expect(parent.tagName).toBe('P');
    expect(parent.closest('b, i')).toBeNull();
  });

  it('a finished block gets no caret', () => {
    expect(draw('done', false).querySelector('[data-testid=chat-caret]')).toBeNull();
  });
});
