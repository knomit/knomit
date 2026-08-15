import { useEffect, useRef } from 'react';
import { EditorView, basicSetup } from 'codemirror';
import { EditorState } from '@codemirror/state';
import { keymap } from '@codemirror/view';
import { indentWithTab } from '@codemirror/commands';
import { yaml } from '@codemirror/lang-yaml';
import type { CompletionContext, CompletionResult } from '@codemirror/autocomplete';
import { linter, lintGutter, type Diagnostic } from '@codemirror/lint';
import { api, type OntologyDiagnostic, type OntologyField } from './api';

// mapDiagnostic turns a server OntologyDiagnostic (1-based line/column into
// the YAML that was SUBMITTED) into a CodeMirror Diagnostic (0-based offsets
// into whatever document is CURRENT when this runs).
//
// Those can differ: the linter below is async, so the user may keep typing
// while a validate request is in flight. By the time the response lands,
// `state` may describe a shorter (or differently shaped) document than the
// one the diagnostic was computed against. Every offset is therefore clamped
// into the current document's bounds — a stale diagnostic must never throw a
// range error, only render in a slightly wrong place until the next debounce
// tick corrects it.
export function mapDiagnostic(state: EditorState, d: OntologyDiagnostic): Diagnostic {
  const line = Math.min(Math.max(d.line, 1), state.doc.lines);
  const lineInfo = state.doc.line(line);
  const from = Math.min(lineInfo.from + Math.max(d.column - 1, 0), state.doc.length);
  const to = Math.min(from + 1, state.doc.length);
  return { from, to: Math.max(from, to), severity: 'error', message: d.message };
}

// DEBOUNCE_MS matches the brief: validate 400ms after the user stops typing,
// not on every keystroke — an ontology is a small document but the server
// round trip is still real network cost.
const DEBOUNCE_MS = 400;

// ontologyLinter is a module-scope extension (stateless beyond the api
// module), reused by every editor instance. `view.state` is read AFTER the
// round trip resolves — see mapDiagnostic's comment — so positions always
// reflect the live document, not the one that was validated.
const ontologyLinter = linter(async (view) => {
  const text = view.state.doc.toString();
  try {
    const result = await api.validateOntology(text);
    if (result.ok) return [];
    return result.diagnostics.map(d => mapDiagnostic(view.state, d));
  } catch {
    // A network hiccup here shouldn't paint the whole document red; the
    // StepOntology-level validate (which drives the Next gate) surfaces the
    // real error.
    return [];
  }
}, { delay: DEBOUNCE_MS });

// OntologyEditor is a CodeMirror 6 YAML editor with two ontology-specific
// extensions: field-name completions sourced from api.ontologySchema() (Go
// is the one description of the ontology shape — a hand-maintained TS copy
// of this list is the version of this feature that was rejected, because a
// drifted list teaches confidently wrong field names) and inline diagnostics
// from api.validateOntology(). It knows nothing about presets or uploads —
// that's StepOntology's job; this component is a plain value/onChange box.
export function OntologyEditor({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  const host = useRef<HTMLDivElement>(null);
  const view = useRef<EditorView | null>(null);
  // Populated asynchronously; the completion source below reads this by
  // reference on every keystroke, so it doesn't matter whether the schema
  // fetch resolves before or after the view is constructed.
  const fieldsRef = useRef<OntologyField[]>([]);

  useEffect(() => {
    let cancelled = false;
    api.ontologySchema().then(f => { if (!cancelled) fieldsRef.current = f; }).catch(() => {});
    return () => { cancelled = true; };
  }, []);

  useEffect(() => {
    if (!host.current) return;
    const yamlLang = yaml();
    const completionSource = (context: CompletionContext): CompletionResult | null => {
      const word = context.matchBefore(/[\w-]*/);
      if (!word || (word.from === word.to && !context.explicit)) return null;
      const fields = fieldsRef.current;
      if (!fields.length) return null;
      return {
        from: word.from,
        options: fields.map(f => ({ label: f.field, type: 'property', detail: f.struct, info: f.doc })),
      };
    };
    const v = new EditorView({
      doc: value,
      extensions: [
        // Ahead of basicSetup so it wins the Tab binding: basicSetup's own
        // keymap does NOT bind Tab to indent (CM6's deliberate
        // accessibility-first default — Tab moves focus to the next
        // focusable element instead). For a bare textarea that was exactly
        // the old ontology editor's worst complaint (Tab moves focus rather
        // than indenting an indentation-sensitive format) — the whole reason
        // this component exists. basicSetup's defaultKeymap already binds
        // Ctrl-M / Alt-Shift-M (mac) to toggleTabFocusMode, CodeMirror's own
        // escape hatch for keyboard users who need Tab to leave the editor,
        // so this doesn't strand anyone.
        keymap.of([indentWithTab]),
        basicSetup,
        yamlLang,
        // The documented CM6 pattern for adding a completion source scoped to
        // one language, rather than a second autocompletion() call — basicSetup
        // already installs one, and language.data is where it looks for more.
        yamlLang.language.data.of({ autocomplete: completionSource }),
        ontologyLinter,
        lintGutter(),
        EditorView.updateListener.of(u => { if (u.docChanged) onChange(u.state.doc.toString()); }),
      ],
      parent: host.current,
    });
    // WKWebView otherwise autocapitalizes and autocorrects typed text; ontology
    // keys are lowercase-kebab-only, so a capitalized key is silent corruption.
    // Mirrors the same guard on the repo-name input in the old CreateRepoForm.
    v.contentDOM.setAttribute('autocapitalize', 'off');
    v.contentDOM.setAttribute('autocorrect', 'off');
    v.contentDOM.setAttribute('spellcheck', 'false');
    view.current = v;
    return () => { v.destroy(); view.current = null; };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return <div ref={host} data-testid="ontology-editor" />;
}
