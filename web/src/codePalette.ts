// The colours this app draws code in, shared by every CodeMirror view: the
// ontology editor (ontologyTheme.ts) and a chat tool result's JSON view
// (JsonView.tsx). One list, so the two never drift into two greens or two
// ambers for the same thing.
//
// Colours are the app's own, so an editor reads as part of knomit rather than
// an embedded third-party widget:
//
//   keys        #8af   the hue this UI already spends on refs and structure
//   values      #c9a97a  warm, and never adjacent to a chrome element that
//                        would make it read as a warning — amber's reserved
//                        meaning (kb/conventions/ui/copy/warning-styling-
//                        reserved-for-failures) is about chrome, not syntax,
//                        and diagnostics still get the real red
//   numbers     #a8a4f0  the lens accent, reused for literals
//   comments    #5f6b5f  muted, recedes
//   punctuation #6a6a6a
//
// Diagnostics keep #f88 — inside an editor an invalid document IS a failure,
// and it is the one thing that must out-shout the syntax.

/** Property names (YAML keys, JSON keys). */
export const CODE_KEY = '#8af';
/** String values. */
export const CODE_VALUE = '#c9a97a';
/** Numbers, booleans, null. */
export const CODE_LITERAL = '#a8a4f0';
export const CODE_COMMENT = '#5f6b5f';
/** Braces, brackets, commas, colons. */
export const CODE_PUNCT = '#6a6a6a';
/** Text that carries no tag of its own. */
export const CODE_TEXT = '#ddd';
export const CODE_ERROR = '#f88';
/** The selection's ground. */
export const CODE_SELECTION = '#24405e';
/** The ground a code view sits on: the app's inputs and wells (manageStyles.ts). */
export const CODE_GROUND = '#0c0c0c';
