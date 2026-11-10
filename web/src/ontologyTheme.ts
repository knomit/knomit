import { EditorView } from '@codemirror/view';
import { HighlightStyle, syntaxHighlighting } from '@codemirror/language';
import { tags as t } from '@lezer/highlight';

import {
  CODE_COMMENT, CODE_ERROR, CODE_GROUND, CODE_KEY, CODE_LITERAL, CODE_PUNCT, CODE_SELECTION, CODE_TEXT, CODE_VALUE,
} from './codePalette';

// The ontology editor's theme.
//
// Without one, CodeMirror ships its DEFAULT highlight style — which is built
// for a light background. Dropped onto this app's dark panels it rendered YAML
// keys as navy on near-black: the structure of the document, in the lowest
// contrast on the screen.
//
// The colours are the app's code palette (codePalette.ts, which says why each
// is what it is), shared with the chat's JSON view.
const KEY = CODE_KEY;
const VALUE = CODE_VALUE;
const LITERAL = CODE_LITERAL;
const COMMENT = CODE_COMMENT;
const PUNCT = CODE_PUNCT;
const TEXT = CODE_TEXT;
const ERROR = CODE_ERROR;

const highlight = HighlightStyle.define([
  // YAML maps its keys to propertyName; the definition() form covers the
  // lezer-yaml variant, so both spellings land on the same colour rather than
  // one of them silently falling through to the default.
  { tag: [t.propertyName, t.definition(t.propertyName), t.attributeName], color: KEY },
  { tag: [t.string, t.special(t.string), t.attributeValue], color: VALUE },
  { tag: [t.number, t.bool, t.null, t.atom], color: LITERAL },
  { tag: [t.comment, t.lineComment, t.blockComment], color: COMMENT, fontStyle: 'italic' },
  { tag: [t.punctuation, t.separator, t.bracket], color: PUNCT },
  { tag: [t.keyword, t.operator], color: '#7c9' },
  { tag: t.meta, color: COMMENT },
  { tag: t.invalid, color: ERROR },
  // Plain scalars are the bulk of an ontology's descriptions and carry no tag
  // of their own; the base .cm-content colour below covers them.
]);

// Values lifted from the app's own inputs and cards (manageStyles.ts): the
// editor sits in a `panel`, so its ground is the same #0c0c0c the confirm
// inputs use, not a colour invented here.
const theme = EditorView.theme({
  '&': {
    color: TEXT,
    backgroundColor: CODE_GROUND,
    border: '1px solid #333',
    borderRadius: '4px',
    fontSize: '12px',
    // An ontology can run to hundreds of lines. Unbounded, the editor pushed
    // the step's own footer off the bottom of the page and the reader lost the
    // controls that let them leave.
    maxHeight: '340px',
  },
  '&.cm-focused': { outline: '2px solid #6ea8fe', outlineOffset: '1px' },
  '.cm-scroller': {
    fontFamily: 'var(--k-font-mono)',
    lineHeight: '1.6',
    overflow: 'auto',
  },
  '.cm-content': { caretColor: '#eee', padding: '8px 0' },
  '.cm-cursor, .cm-dropCursor': { borderLeftColor: '#eee' },
  '&.cm-focused .cm-selectionBackground, .cm-selectionBackground, .cm-content ::selection': {
    backgroundColor: CODE_SELECTION,
  },
  '.cm-activeLine': { backgroundColor: '#161616' },
  '.cm-gutters': {
    backgroundColor: CODE_GROUND,
    color: '#555',
    border: 'none',
    borderRight: '1px solid #1e1e1e',
  },
  '.cm-activeLineGutter': { backgroundColor: '#161616', color: '#999' },
  '.cm-foldGutter .cm-gutterElement': { color: '#4a4a4a' },
  '.cm-matchingBracket, &.cm-focused .cm-matchingBracket': {
    backgroundColor: '#1a2a1a',
    color: '#7c9',
    outline: 'none',
  },
  '.cm-nonmatchingBracket': { color: ERROR },
  // Completions: the same card grammar as the rest of the Manage surface, so
  // the popup does not arrive as a stock widget in a themed screen.
  '.cm-tooltip': {
    backgroundColor: '#111',
    border: '1px solid #333',
    borderRadius: '4px',
    color: TEXT,
  },
  '.cm-tooltip.cm-tooltip-autocomplete > ul': {
    fontFamily: 'var(--k-font-mono)',
    fontSize: '11.5px',
    maxHeight: '180px',
  },
  '.cm-tooltip.cm-tooltip-autocomplete > ul > li': { padding: '3px 8px' },
  '.cm-tooltip.cm-tooltip-autocomplete > ul > li[aria-selected]': {
    backgroundColor: '#1a2a1a',
    color: '#eee',
  },
  '.cm-completionLabel': { color: TEXT },
  '.cm-completionDetail': { color: '#777', fontStyle: 'normal', marginLeft: '8px' },
  '.cm-completionInfo': {
    backgroundColor: '#111',
    border: '1px solid #333',
    borderRadius: '4px',
    color: '#aaa',
    padding: '6px 8px',
    maxWidth: '280px',
  },
  // Diagnostics. The squiggle is the editor's own; the panel below the step
  // carries the readable version.
  '.cm-diagnostic-error': { borderLeft: `3px solid ${ERROR}` },
  '.cm-tooltip-lint .cm-diagnostic': { color: '#ddd', fontFamily: 'var(--k-font-mono)', fontSize: '11px' },
  '.cm-lintRange-error': {
    backgroundImage: 'none',
    textDecoration: `underline wavy ${ERROR}`,
    textUnderlineOffset: '3px',
  },
  '.cm-lint-marker-error': { content: 'none' },
  '.cm-gutter-lint .cm-gutterElement': { padding: '0 3px' },
  '.cm-placeholder': { color: '#5a5a5a' },
}, { dark: true });

/** ontologyEditorTheme is the whole visual contract — theme plus highlighting —
 *  as one extension, so a caller cannot install one without the other. */
export const ontologyEditorTheme = [theme, syntaxHighlighting(highlight)];
