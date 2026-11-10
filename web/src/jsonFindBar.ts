import type { EditorState } from '@codemirror/state';
import type { EditorView, Panel } from '@codemirror/view';
import { SearchQuery, closeSearchPanel, findNext, findPrevious, getSearchQuery, setSearchQuery } from '@codemirror/search';
import {
  FIND_CLOSE, FIND_INVALID, FIND_LABEL, FIND_MATCH_CASE, FIND_NEXT, FIND_NONE, FIND_PREV, FIND_REGEXP, FIND_WHOLE_WORD,
  findAt, findFound,
} from './chatCopy';

// The JSON view's find bar: CodeMirror's search, behind the app's own bar
// instead of CodeMirror's stock panel (search({ createPanel })). The search
// itself (matching, highlighting, moving the selection) stays CodeMirror's.

/** The most matches the count walks; past it the count stops growing. */
const COUNT_CAP = 9999;

/** What the count says, and whether it is drawn red (nothing found, a bad pattern). */
export interface FindCount { text: string; bad: boolean }

/**
 * findCount is the bar's count for a state: "1 of 7" when the selection is on a
 * match, "7 found" when it is not, "no results", or "invalid" for a regular
 * expression that does not compile. Empty while nothing is typed.
 */
export function findCount(state: EditorState): FindCount {
  const q = getSearchQuery(state);
  if (!q.search) return { text: '', bad: false };
  if (!q.valid) return { text: FIND_INVALID, bad: true };
  const sel = state.selection.main;
  const cursor = q.getCursor(state.doc);
  let n = 0;
  let at = 0;
  for (let r = cursor.next(); !r.done && n < COUNT_CAP; r = cursor.next()) {
    n++;
    if (r.value.from === sel.from && r.value.to === sel.to) at = n;
  }
  if (n === 0) return { text: FIND_NONE, bad: true };
  return { text: at ? findAt(at, n) : findFound(n), bad: false };
}

const ns = 'http://www.w3.org/2000/svg';
/** A 24-unit stroke icon, as the app's icons are drawn. */
function icon(size: number, draw: (svg: SVGSVGElement) => void): SVGSVGElement {
  const svg = document.createElementNS(ns, 'svg');
  for (const [k, v] of Object.entries({
    width: String(size), height: String(size), viewBox: '0 0 24 24', fill: 'none', stroke: 'currentColor',
    'stroke-width': '2', 'stroke-linecap': 'round', 'stroke-linejoin': 'round', 'aria-hidden': 'true',
  })) svg.setAttribute(k, v);
  draw(svg);
  return svg;
}
function part(svg: SVGSVGElement, tag: string, attrs: Record<string, string>): void {
  const el = document.createElementNS(ns, tag);
  for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, v);
  svg.appendChild(el);
}
/** The magnifier, also drawn on the footer's Find button. */
export const magnifier = (size: number) => icon(size, s => { part(s, 'circle', { cx: '11', cy: '11', r: '7' }); part(s, 'path', { d: 'm20 20-3.5-3.5' }); });
const chevron = (points: string) => icon(12, s => part(s, 'polyline', { points }));

function el<K extends keyof HTMLElementTagNameMap>(tag: K, cls: string, attrs: Record<string, string> = {}): HTMLElementTagNameMap[K] {
  const e = document.createElement(tag);
  e.className = cls;
  for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v);
  return e;
}

type Toggle = 'caseSensitive' | 'wholeWord' | 'regexp';

/**
 * findPanel builds the bar: a magnifier, the input ("Find"), the count, the
 * three toggles (Aa match case, ab underlined whole word, .* regular
 * expression), previous and next, and ✕. Enter goes to the next match,
 * Shift+Enter to the previous; Esc closes the bar and gives the focus back to
 * the text. Esc is taken (preventDefault), so the side panel's own Esc, which
 * closes the panel, does not also fire.
 */
export function findPanel(view: EditorView): Panel {
  const dom = el('div', 'k-find', { 'data-testid': 'json-find' });
  const ico = el('span', 'k-find-ico');
  ico.appendChild(magnifier(13));
  const input = el('input', 'k-find-in', {
    type: 'text', placeholder: FIND_LABEL, 'aria-label': FIND_LABEL, spellcheck: 'false', autocomplete: 'off',
    'main-field': 'true', 'data-testid': 'json-find-input',
  });
  const count = el('span', 'k-find-n', { 'aria-live': 'polite', 'data-testid': 'json-find-count' });
  const q0 = getSearchQuery(view.state);
  input.value = q0.search;
  const opts: Record<Toggle, boolean> = { caseSensitive: q0.caseSensitive, wholeWord: q0.wholeWord, regexp: q0.regexp };

  const toggle = (k: Toggle, label: string, face: Node) => {
    const b = el('button', 'k-find-t', { type: 'button', 'aria-label': label, title: label, 'aria-pressed': String(opts[k]), 'data-testid': `json-find-${k}` });
    b.appendChild(face);
    b.addEventListener('click', () => {
      opts[k] = !opts[k];
      b.setAttribute('aria-pressed', String(opts[k]));
      commit();
      input.focus();
    });
    return b;
  };
  const underlined = el('u', '');
  underlined.textContent = 'ab';
  const button = (label: string, testId: string, face: Node, run: () => void) => {
    const b = el('button', 'k-find-b', { type: 'button', 'aria-label': label, title: label, 'data-testid': testId });
    b.appendChild(face);
    b.addEventListener('click', run);
    return b;
  };
  const close = () => { closeSearchPanel(view); view.focus(); };

  dom.append(
    ico, input, count,
    toggle('caseSensitive', FIND_MATCH_CASE, document.createTextNode('Aa')),
    toggle('wholeWord', FIND_WHOLE_WORD, underlined),
    toggle('regexp', FIND_REGEXP, document.createTextNode('.*')),
    el('span', 'k-find-sep', { 'aria-hidden': 'true' }),
    button(FIND_PREV, 'json-find-prev', chevron('18 15 12 9 6 15'), () => findPrevious(view)),
    button(FIND_NEXT, 'json-find-next', chevron('6 9 12 15 18 9'), () => findNext(view)),
    button(FIND_CLOSE, 'json-find-close', document.createTextNode('✕'), close),
  );

  function commit() {
    view.dispatch({ effects: setSearchQuery.of(new SearchQuery({ search: input.value, ...opts })) });
  }
  const refresh = (state: EditorState) => {
    const c = findCount(state);
    count.textContent = c.text;
    dom.classList.toggle('k-find--bad', c.bad);
  };
  input.addEventListener('input', commit);
  input.addEventListener('keydown', e => {
    if (e.key === 'Enter') {
      e.preventDefault();
      if (e.shiftKey) findPrevious(view); else findNext(view);
    } else if (e.key === 'Escape') {
      e.preventDefault();
      close();
    }
  });

  return {
    dom,
    top: false,
    mount() { input.focus(); input.select(); refresh(view.state); },
    update(u) { if (u.docChanged || u.selectionSet || u.transactions.some(t => t.effects.some(e => e.is(setSearchQuery)))) refresh(u.state); },
  };
}
