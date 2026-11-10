import { useEffect, useLayoutEffect, useRef } from 'react';
import { EditorState, StateEffect, StateField } from '@codemirror/state';
import type { Extension } from '@codemirror/state';
import { Decoration, EditorView, MatchDecorator, ViewPlugin, drawSelection, hoverTooltip, keymap } from '@codemirror/view';
import type { DecorationSet, ViewUpdate } from '@codemirror/view';
import { HighlightStyle, codeFolding, foldEffect, foldGutter, syntaxHighlighting } from '@codemirror/language';
import { json } from '@codemirror/lang-json';
import { openSearchPanel, search } from '@codemirror/search';
import { tags as t } from '@lezer/highlight';
import { CODE_GROUND, CODE_KEY, CODE_LITERAL, CODE_PUNCT, CODE_SELECTION, CODE_TEXT, CODE_VALUE } from './codePalette';
import { dateAt, foldLabel, initialFolds } from './jsonViewText';
import { findPanel } from './jsonFindBar';
import type { ResolvedFact } from './chatFactLinks';
import type { FactLinks } from './jsonViewLinks';

// A chat tool result (or a call's arguments), read only, in CodeMirror: JSON
// highlighted in the app's code palette, blocks deeper than two levels folded
// behind a "N keys" pill, kb:// fact paths as links, a `*_at` number's date on
// hover, long lines wrapped, and the app's own find bar. No line numbers and no
// active line: it is a document to read, not one to edit.
//
// The editor is editable in the DOM sense (contenteditable) and read only in
// state (EditorState.readOnly): that is what keeps keyboard selection, ⌘F and
// copy working. Its content carries data-k-readonly-text, which the side panel's
// Esc reads as "not typing" (chatPanelState.typingIn), so Esc in the result
// still closes the panel as it did before the view was an editor.


const setLinks = StateEffect.define<FactLinks>();
const linksField = StateField.define<FactLinks>({
  create: () => new Map(),
  update: (v, tr) => tr.effects.reduce((acc, e) => (e.is(setLinks) ? e.value : acc), v),
});

/** A kb:// path in a JSON string (stops at the closing quote or an escape). */
const linkMatcher = new MatchDecorator({
  regexp: /kb:\/\/[\w.-]+\/[\w./-]+/g,
  decorate: (add, from, _to, m, view) => {
    const ref = m[0].replace(/[./]+$/, '');
    if (!view.state.field(linksField).has(ref)) return; // unresolved: plain text
    add(from, from + ref.length, Decoration.mark({ class: 'k-json-fact', attributes: { 'data-ref': ref } }));
  },
});

/** How far the pointer may move between press and release for it to be a click. */
const CLICK_SLOP_PX = 3;

/**
 * The fact links, and their click: open the fact, unless the click ended a
 * drag. A drag is told by the pointer having moved since the press, not by the
 * selection: a press inside an existing selection leaves it in place until the
 * release, so a plain click there would read as "a selection" and open nothing.
 */
function factLinks(onOpenFact: (f: ResolvedFact) => void): Extension {
  let down: { x: number; y: number } | null = null;
  const plugin = ViewPlugin.fromClass(class {
    decorations: DecorationSet;
    constructor(view: EditorView) { this.decorations = linkMatcher.createDeco(view); }
    update(u: ViewUpdate) {
      const relinked = u.transactions.some(tr => tr.effects.some(e => e.is(setLinks)));
      this.decorations = relinked ? linkMatcher.createDeco(u.view) : linkMatcher.updateDeco(u, this.decorations);
    }
  }, {
    decorations: v => v.decorations,
    eventHandlers: {
      mousedown(e) {
        down = { x: e.clientX, y: e.clientY };
        return false;
      },
      click(e, view) {
        const link = (e.target as Element | null)?.closest?.('.k-json-fact');
        const from = down;
        down = null;
        if (!link || e.button !== 0) return false;
        // A drag that selected text ends in a click too: it is a selection, not a click on the link.
        if (from && Math.hypot(e.clientX - from.x, e.clientY - from.y) > CLICK_SLOP_PX) return false;
        const f = view.state.field(linksField).get(link.getAttribute('data-ref') ?? '');
        if (!f) return false;
        e.preventDefault();
        onOpenFact(f);
        return true;
      },
    },
  });
  return [linksField, plugin];
}

/** A `*_at` number's date, in a tooltip drawn as the app's tooltips are. */
const dates = hoverTooltip((view, pos) => {
  const line = view.state.doc.lineAt(pos);
  const hit = dateAt(line.text, pos - line.from);
  if (!hit) return null;
  return {
    pos: line.from + hit.from,
    end: line.from + hit.to,
    above: true,
    create: () => {
      const dom = document.createElement('div');
      dom.className = 'k-json-date';
      dom.setAttribute('data-testid', 'json-date');
      dom.textContent = hit.date.toLocaleString();
      return { dom };
    },
  };
});

/** The fold mark: the chat's own › (the tool group's), turned down when open. */
function foldMark(open: boolean): HTMLElement {
  const s = document.createElement('span');
  s.className = 'k-json-fold';
  s.setAttribute('data-open', String(open));
  s.textContent = '›';
  return s;
}

/** A folded block's pill ("3 keys"); a click unfolds it. */
function foldPill(_view: EditorView, onclick: (e: Event) => void, label: string | null): HTMLElement {
  const s = document.createElement('span');
  s.className = 'k-json-pill';
  s.setAttribute('data-testid', 'json-fold-pill');
  s.textContent = label ?? '…';
  s.onclick = onclick;
  return s;
}

const highlight = HighlightStyle.define([
  { tag: t.propertyName, color: CODE_KEY },
  { tag: t.string, color: CODE_VALUE },
  { tag: [t.number, t.bool, t.null], color: CODE_LITERAL },
  { tag: [t.punctuation, t.separator, t.bracket], color: CODE_PUNCT },
]);

/** A failed call's result keeps the error colour the panel always gave it. */
const ERROR_TEXT = '#e0a0a0';

// The well is the panel's (the <pre> it replaces): #0c0c0c, a #1e1e1e border,
// 6px corners; the mono font at 12px, line-height 1.6.
const theme = EditorView.theme({
  '&': {
    height: '100%', backgroundColor: CODE_GROUND, color: CODE_TEXT, fontSize: '12px',
    border: '1px solid #1e1e1e', borderRadius: '6px',
  },
  '&.cm-focused': { outline: '2px solid #6ea8fe', outlineOffset: '1px' },
  '&.k-json-error .cm-content': { color: ERROR_TEXT },
  '.cm-scroller': { fontFamily: 'var(--k-font-mono)', lineHeight: '1.6', overflow: 'auto' },
  // The app's scrollbar track (#111) inside the #0c0c0c well read as a second
  // line beside the frame's right edge: the track takes the well's ground, so
  // only the thumb shows.
  '.cm-scroller::-webkit-scrollbar-track': { background: CODE_GROUND },
  '.cm-content': { padding: '6px 0', caretColor: '#eee' },
  '.cm-line': { padding: '0 8px 0 6px' },
  '.cm-cursor': { borderLeftColor: '#eee' },
  '&.cm-focused .cm-selectionBackground, .cm-selectionBackground, .cm-content ::selection': { backgroundColor: `${CODE_SELECTION} !important` },
  '.cm-gutters': { backgroundColor: CODE_GROUND, border: 'none', borderTopLeftRadius: '6px', borderBottomLeftRadius: '6px' },
  '.cm-foldGutter .cm-gutterElement': { display: 'flex', alignItems: 'center', justifyContent: 'center', width: '14px', paddingLeft: '2px' },
  // The tool group's chevron: #555, 11px, turned down (90deg) when the block is open.
  '.k-json-fold': {
    display: 'inline-block', cursor: 'pointer', color: '#555', font: '11px/1 var(--k-font-body)', transition: 'transform .12s',
  },
  '.k-json-fold[data-open="true"]': { transform: 'rotate(90deg)' },
  '.k-json-fold:hover': { color: '#ccc' },
  '@media (prefers-reduced-motion: reduce)': { '.k-json-fold': { transition: 'none' } },
  // The pill is drawn as a fact chip (.k-fact-link): its ground, border and hover.
  '.k-json-pill': {
    display: 'inline-block', font: '11.5px/17px var(--k-font-body)', color: '#7a8aa0', background: '#161a21',
    border: '1px solid #263041', borderRadius: '5px', padding: '0 6px', margin: '0 3px', cursor: 'pointer', verticalAlign: '1px',
  },
  '.k-json-pill:hover': { borderColor: '#4f6a96', background: '#1b2332', color: '#eef3fb' },
  // A fact path: the fact link's text colour, underlined on hover as a commit link is.
  '.k-json-fact': { cursor: 'pointer' },
  '.k-json-fact, .k-json-fact *': { color: '#cbd6e6 !important' },
  '.k-json-fact:hover': { textDecoration: 'underline', textDecorationColor: '#34507a', textUnderlineOffset: '3px' },
  // The app's tooltip (the chat rail's): #1b1b1d, a #333 border, 8px corners, the long soft shadow.
  '.cm-tooltip': {
    backgroundColor: '#1b1b1d', border: '1px solid #333', borderRadius: '8px', color: CODE_TEXT,
    boxShadow: '0 14px 40px -10px rgba(0,0,0,.7)',
  },
  '.k-json-date': { font: '11.5px/1.4 var(--k-font-body)', color: '#ddd', padding: '5px 9px' },
  // The find bar.
  '.cm-panels': { backgroundColor: '#111', color: CODE_TEXT },
  '.cm-panels.cm-panels-bottom': { borderTop: '1px solid #1e1e1e', borderBottomLeftRadius: '6px', borderBottomRightRadius: '6px' },
  '.cm-searchMatch': { backgroundColor: '#3a3320', outline: '1px solid #6a5a30' },
  '.cm-searchMatch.cm-searchMatch-selected': { backgroundColor: '#5a4a20' },
  '.k-find': { display: 'flex', alignItems: 'center', gap: '4px', padding: '6px 8px', font: '12px var(--k-font-body)', color: CODE_TEXT },
  '.k-find-ico': { display: 'inline-flex', color: '#666', margin: '0 2px' },
  '.k-find-in': {
    flex: '1', minWidth: '60px', background: CODE_GROUND, color: CODE_TEXT, border: '1px solid #333', borderRadius: '4px',
    padding: '4px 7px', font: '12px var(--k-font-mono)', outline: 'none',
  },
  '.k-find-in:focus': { borderColor: '#4a7ab5', boxShadow: '0 0 0 1px #4a7ab5' },
  '.k-find--bad .k-find-in': { borderColor: '#7a3a3a' },
  '.k-find-n': { font: '11px var(--k-font-mono)', color: '#777', minWidth: '58px', textAlign: 'right', whiteSpace: 'nowrap' },
  '.k-find--bad .k-find-n': { color: ERROR_TEXT },
  '.k-find-t, .k-find-b': {
    display: 'inline-flex', alignItems: 'center', justifyContent: 'center', minWidth: '24px', height: '24px', background: 'none',
    border: '1px solid transparent', borderRadius: '4px', color: '#888', cursor: 'pointer', font: '11.5px var(--k-font-mono)', padding: '0 4px',
  },
  '.k-find-t:hover, .k-find-b:hover': { color: '#eee', background: '#1a1a1a' },
  '.k-find-t[aria-pressed="true"]': { color: '#cbd6e6', background: '#161a21', borderColor: '#263041' },
  '.k-find-sep': { width: '1px', height: '16px', background: '#2a2a2a', margin: '0 2px' },
  '.k-find button:focus-visible': { outline: '2px solid #4a7ab5', outlineOffset: '1px' },
}, { dark: true });

/** What the view hands its owner once it is up: the way to open its find bar. */
export interface JsonViewHandle { openFind: () => void }

/**
 * JsonView shows `text` read only. With `json` it is highlighted and foldable,
 * and opens with the blocks deeper than two levels folded; without, it is plain
 * text in the same view. `error` draws the text in the error colour, unhighlighted.
 * `links` are the kb:// paths that resolved; each opens its fact (onOpenFact).
 */
export function JsonView({ text, json: isJson, error = false, links, onOpenFact, onReady, testId }: {
  text: string;
  json: boolean;
  error?: boolean;
  links?: FactLinks;
  onOpenFact?: (f: ResolvedFact) => void;
  onReady?: (h: JsonViewHandle | null) => void;
  testId?: string;
}) {
  const host = useRef<HTMLDivElement>(null);
  const view = useRef<EditorView | null>(null);
  const openRef = useRef(onOpenFact);
  const readyRef = useRef(onReady);
  useLayoutEffect(() => { openRef.current = onOpenFact; readyRef.current = onReady; });

  // A layout effect: the editor is in the box before the page paints (or a test
  // finds the box), never an empty well for a frame.
  useLayoutEffect(() => {
    const parent = host.current;
    if (!parent) return;
    const highlighted = isJson && !error;
    const v = new EditorView({
      parent,
      state: EditorState.create({
        doc: text,
        extensions: [
          EditorState.readOnly.of(true),
          EditorView.contentAttributes.of({
            'data-k-readonly-text': 'true', spellcheck: 'false', autocorrect: 'off', autocapitalize: 'off',
          }),
          error ? EditorView.editorAttributes.of({ class: 'k-json-error' }) : [],
          EditorView.lineWrapping,
          drawSelection(),
          highlighted ? [
            json(),
            syntaxHighlighting(highlight),
            codeFolding({ preparePlaceholder: (state, r) => foldLabel(state.sliceDoc(r.from - 1, r.to), 1, r.to - r.from + 1), placeholderDOM: foldPill }),
            foldGutter({ markerDOM: foldMark }),
          ] : [],
          factLinks(f => openRef.current?.(f)),
          dates,
          search({ createPanel: findPanel }),
          keymap.of([{ key: 'Mod-f', run: openSearchPanel, scope: 'editor search-panel' }]),
          theme,
        ],
      }),
    });
    if (highlighted) {
      const folds = initialFolds(text);
      if (folds.length) v.dispatch({ effects: folds.map(r => foldEffect.of(r)) });
    }
    view.current = v;
    readyRef.current?.({ openFind: () => { openSearchPanel(v); } });
    return () => {
      readyRef.current?.(null);
      view.current = null;
      v.destroy();
    };
  }, [text, isJson, error]);

  // Links resolve after the view is up: hand them over without rebuilding it.
  useEffect(() => {
    view.current?.dispatch({ effects: setLinks.of(links ?? new Map()) });
  }, [links, text, isJson, error]);

  return <div ref={host} data-testid={testId} data-error={error || undefined} className="k-json-view" style={{ height: '100%', minHeight: 0 }} />;
}
