import { useEffect, useRef } from 'react';
import { EditorView, basicSetup } from 'codemirror';
import { yaml } from '@codemirror/lang-yaml';

// SPIKE (Task 6): throwaway probe verifying CodeMirror 6 works inside the
// desktop WKWebView shell. Task 9 replaces this with the real editor
// (completions from api.ontologySchema(), lint diagnostics from
// api.validateOntology). Do not build those features here.
export function OntologyEditor({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  const host = useRef<HTMLDivElement>(null);
  const view = useRef<EditorView | null>(null);

  useEffect(() => {
    if (!host.current) return;
    const v = new EditorView({
      doc: value,
      extensions: [
        basicSetup,
        yaml(),
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
