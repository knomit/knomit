import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent, act, waitFor, within } from '@testing-library/react';
import { EditorView } from '@codemirror/view';
import { EditorSelection } from '@codemirror/state';
import { JsonView } from './JsonView';
import type { JsonViewHandle } from './JsonView';
import { resultText } from './jsonViewText';
import type { ResolvedFact } from './chatFactLinks';

// jsdom lays nothing out, so these assert what CodeMirror puts in the DOM and
// state (folds, pills, decorations, the find bar, the clipboard text). How it
// LOOKS (wrapping, colours, the chevron's turn, the tooltip) is checked in a
// real browser.

// jsdom's Range has no geometry; test-setup.ts gives it empty rects, which
// CodeMirror needs when a mouse press or a selection is measured.
const FACT_REF = 'kb://3ec012f5b4d2/kb/architecture/web/api-chat/ca7a68fa.md';
const fact: ResolvedFact = { handle: FACT_REF, id: 'ca7a68fa', title: 'A chat title', type: 'observation', repo: 'knomit-kb', branch: 'agent/x', path: 'kb/architecture/web/api-chat/ca7a68fa.md', excerpt: '' };

const RESULT = JSON.stringify({
  facts: [
    { file: FACT_REF, title: 'keep this', frontmatter: { refs: ['src://7b4887ce51d9/web/a.ts@abc', 'kb://3ec012f5b4d2/kb/unknown/zz.md'], entities: ['keep', 'Keep', 'keeper'] }, committed_at: 1794274206 },
  ],
});

function mount(props: Partial<Parameters<typeof JsonView>[0]> & { raw?: string } = {}) {
  const { raw = RESULT, ...rest } = props;
  const { text, json } = resultText(raw);
  let handle: JsonViewHandle | null = null;
  const utils = render(<div style={{ height: 400 }}><JsonView testId="jv" text={text} json={json} onReady={h => { handle = h; }} {...rest} /></div>);
  const host = screen.getByTestId('jv');
  const view = EditorView.findFromDOM(host.querySelector('.cm-editor') as HTMLElement)!;
  return { ...utils, host, view, text, handle: () => handle! };
}

describe('JsonView', () => {
  it('shows the pretty-printed JSON, read only, with no line numbers and no active line', () => {
    const { host, view, text } = mount();
    expect(view.state.doc.toString()).toBe(text);
    expect(text.split('\n')[1]).toBe('  "facts": [');
    expect(view.state.readOnly).toBe(true);
    expect(host.querySelector('.cm-lineNumbers')).toBeNull();
    expect(host.querySelector('.cm-activeLine')).toBeNull();
    expect(host.querySelector('.cm-foldGutter')).not.toBeNull();
    expect(host.querySelector('.cm-lineWrapping')).not.toBeNull();
  });

  it('starts with blocks deeper than two levels folded behind a count pill', () => {
    const { host } = mount();
    // facts (1) > the fact (2) > frontmatter (3): frontmatter is folded; its two keys are counted.
    const pills = [...host.querySelectorAll('[data-testid="json-fold-pill"]')].map(p => p.textContent);
    expect(pills).toEqual(['2 keys']);
    expect(host.querySelector('.cm-content')?.textContent).not.toContain('entities');
    // The fact itself (depth 2) is open.
    expect(host.querySelector('.cm-content')?.textContent).toContain('"title": "keep this"');
  });

  it('a pill unfolds on click; the fold mark is the chat\'s › and says whether it is open', () => {
    const { host } = mount();
    const marks = [...host.querySelectorAll('.k-json-fold')];
    expect(marks.length).toBeGreaterThan(0);
    expect(marks.every(m => m.textContent === '›')).toBe(true);
    expect(marks.some(m => m.getAttribute('data-open') === 'false')).toBe(true);
    fireEvent.click(host.querySelector('[data-testid="json-fold-pill"]')!);
    expect(host.querySelector('[data-testid="json-fold-pill"]')).toBeNull();
    expect(host.querySelector('.cm-content')?.textContent).toContain('"entities"');
  });

  it('plain text: not JSON, so no folding and no JSON highlighting', () => {
    const { host, view } = mount({ raw: 'no lens named "demo-kb" {' });
    expect(view.state.doc.toString()).toBe('no lens named "demo-kb" {');
    expect(host.querySelector('.cm-foldGutter')).toBeNull();
    // No highlighting: the line holds the text and no styled token spans.
    expect(host.querySelector('.cm-line')?.querySelector('span')).toBeNull();
  });

  it('JSON is highlighted (tokens are styled spans)', () => {
    const { host } = mount({ raw: '{"a": 1}' });
    expect(host.querySelectorAll('.cm-line span').length).toBeGreaterThan(0);
  });

  it('an error keeps the error colouring: the editor is marked and nothing is highlighted', () => {
    const { host } = mount({ raw: '{"error": "boom"}', error: true });
    expect(host.querySelector('.cm-editor')).toHaveClass('k-json-error');
    expect(host.querySelector('.cm-line span')).toBeNull();
  });

  it('a resolved kb:// path is a link that opens its fact; an unresolved one, and src://, stay text', async () => {
    const onOpenFact = vi.fn();
    const { host } = mount({ links: new Map([[FACT_REF, fact]]), onOpenFact });
    // Unfold everything so the unresolved ref in frontmatter is on screen too.
    fireEvent.click(host.querySelector('[data-testid="json-fold-pill"]')!);
    const links = await waitFor(() => {
      const l = host.querySelectorAll('.k-json-fact');
      expect(l.length).toBe(1);
      return l;
    });
    expect(links[0].getAttribute('data-ref')).toBe(FACT_REF);
    expect(host.querySelector('[data-ref^="src://"]')).toBeNull();
    expect(host.querySelector('[data-ref*="unknown"]')).toBeNull();
    fireEvent.click(links[0]);
    expect(onOpenFact).toHaveBeenCalledWith(fact);
  });

  it('a click that ends a drag selection opens nothing', () => {
    const onOpenFact = vi.fn();
    const { host } = mount({ links: new Map([[FACT_REF, fact]]), onOpenFact });
    const link = host.querySelector('.k-json-fact')!;
    // Pressed 40px away, released on the link: a drag.
    fireEvent.mouseDown(link, { clientX: 10, clientY: 10 });
    fireEvent.click(link, { clientX: 50, clientY: 10 });
    expect(onOpenFact).not.toHaveBeenCalled();
  });

  it('a plain click on a link opens it even inside an existing selection (seen in a browser: the press keeps the selection)', () => {
    const onOpenFact = vi.fn();
    const { host, view } = mount({ links: new Map([[FACT_REF, fact]]), onOpenFact });
    act(() => { view.dispatch({ selection: EditorSelection.range(0, view.state.doc.length) }); });
    const link = host.querySelector('.k-json-fact')!;
    fireEvent.mouseDown(link, { clientX: 20, clientY: 10 });
    fireEvent.click(link, { clientX: 21, clientY: 10 });
    expect(onOpenFact).toHaveBeenCalledWith(fact);
  });

  it('a copy across a folded block gives the folded text, not the pill', () => {
    const { view, text } = mount();
    act(() => { view.focus(); view.dispatch({ selection: EditorSelection.range(0, text.length) }); });
    const data = new Map<string, string>();
    const ev = new Event('copy', { bubbles: true, cancelable: true }) as ClipboardEvent;
    Object.defineProperty(ev, 'clipboardData', { value: { clearData: () => data.clear(), setData: (k: string, v: string) => data.set(k, v), getData: (k: string) => data.get(k) ?? '' } });
    act(() => { view.contentDOM.dispatchEvent(ev); });
    expect(data.get('text/plain')).toBe(text);
    expect(data.get('text/plain')).toContain('"entities"');
    expect(data.get('text/plain')).not.toContain('2 keys');
  });

  it('marks its content as read-only text, so the side panel\'s Esc does not take it for a field', () => {
    const { view } = mount();
    expect(view.contentDOM).toHaveAttribute('data-k-readonly-text', 'true');
    expect(view.contentDOM).toHaveAttribute('spellcheck', 'false');
  });
});

describe('the find bar', () => {
  const open = () => {
    const m = mount();
    act(() => m.handle().openFind());
    const bar = within(m.host).getByTestId('json-find');
    return { ...m, bar, input: within(bar).getByTestId('json-find-input') as HTMLInputElement, count: () => within(bar).getByTestId('json-find-count') };
  };
  const type = (input: HTMLInputElement, v: string) => fireEvent.input(input, { target: { value: v } });

  it('replaces CodeMirror\'s panel: magnifier, "Find", count, three toggles, previous, next, close', () => {
    const { host, bar, input } = open();
    expect(host.querySelector('.cm-search')).toBeNull();
    expect(input).toHaveAttribute('placeholder', 'Find');
    expect(document.activeElement).toBe(input);
    expect(bar.querySelector('.k-find-ico svg')).not.toBeNull();
    const labels = [...bar.querySelectorAll('button')].map(b => [b.getAttribute('aria-label'), b.textContent]);
    expect(labels).toEqual([
      ['Match case', 'Aa'], ['Whole word', 'ab'], ['Regular expression', '.*'],
      ['Previous match', ''], ['Next match', ''], ['Close find', '✕'],
    ]);
    expect(within(bar).getByTestId('json-find-wholeWord').querySelector('u')).toHaveTextContent('ab');
  });

  it('counts: "N found", then "1 of N" on Enter, back with Shift+Enter; "no results" in red', () => {
    const { bar, input, count } = open();
    type(input, 'keep');
    expect(count()).toHaveTextContent('4 found'); // keep this, keep, Keep, keeper
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(count()).toHaveTextContent('1 of 4');
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(count()).toHaveTextContent('2 of 4');
    fireEvent.keyDown(input, { key: 'Enter', shiftKey: true });
    expect(count()).toHaveTextContent('1 of 4');
    expect(bar).not.toHaveClass('k-find--bad');
    type(input, 'zzzz');
    expect(count()).toHaveTextContent('no results');
    expect(bar).toHaveClass('k-find--bad');
  });

  it('the toggles: match case, whole word, and a regular expression that does not compile is "invalid"', () => {
    const { bar, input, count } = open();
    type(input, 'keep');
    const toggle = (k: string) => within(bar).getByTestId(`json-find-${k}`);
    fireEvent.click(toggle('caseSensitive'));
    expect(toggle('caseSensitive')).toHaveAttribute('aria-pressed', 'true');
    expect(count()).toHaveTextContent('3 found'); // not "Keep"
    fireEvent.click(toggle('wholeWord'));
    expect(count()).toHaveTextContent('2 found'); // not "keeper"
    fireEvent.click(toggle('caseSensitive'));
    fireEvent.click(toggle('wholeWord'));
    fireEvent.click(toggle('regexp'));
    type(input, 'kee(');
    expect(count()).toHaveTextContent('invalid');
    expect(bar).toHaveClass('k-find--bad');
    type(input, 'kee[pr]');
    expect(count()).toHaveTextContent('4 found');
  });

  it('Esc closes the bar, takes the key (the panel stays) and gives the focus back to the text', () => {
    const { host, view, input } = open();
    const ev = new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true });
    act(() => { input.dispatchEvent(ev); });
    expect(ev.defaultPrevented).toBe(true);
    expect(within(host).queryByTestId('json-find')).toBeNull();
    expect(document.activeElement).toBe(view.contentDOM);
  });

  it('✕ closes it too; Mod-F (Ctrl here, ⌘ on a Mac) in the text opens it again', () => {
    const { host, view, bar } = open();
    fireEvent.click(within(bar).getByTestId('json-find-close'));
    expect(within(host).queryByTestId('json-find')).toBeNull();
    const key = new KeyboardEvent('keydown', { key: 'f', code: 'KeyF', keyCode: 70, ctrlKey: true, bubbles: true, cancelable: true });
    act(() => { view.contentDOM.dispatchEvent(key); });
    expect(within(host).getByTestId('json-find')).toBeInTheDocument();
  });
});
