import { describe, it, expect, vi } from 'vitest';
import { render, screen, act } from '@testing-library/react';
import { EditorState } from '@codemirror/state';
import { OntologyEditor, mapDiagnostic } from './OntologyEditor';

vi.mock('./api', () => ({
  api: { ontologySchema: vi.fn(async () => []), validateOntology: vi.fn(async () => ({ ok: true })) },
}));

describe('OntologyEditor', () => {
  // WKWebView autocapitalize corrupted the repo-name input; ontology keys are
  // lowercase-kebab-only, so the same bug here is silent data corruption.
  it('disables autocapitalize, autocorrect and spellcheck on the content DOM', () => {
    render(<OntologyEditor value="id: x" onChange={() => {}} />);
    const content = screen.getByTestId('ontology-editor').querySelector('.cm-content');
    expect(content).toHaveAttribute('autocapitalize', 'off');
    expect(content).toHaveAttribute('autocorrect', 'off');
    expect(content).toHaveAttribute('spellcheck', 'false');
  });

  const shownDoc = () =>
    (screen.getByTestId('ontology-editor').querySelector('.cm-content') as HTMLElement).textContent;

  // The editor is built ONCE, so a document handed to it afterwards — an
  // uploaded file, a re-seed from another preset — only appears if the view is
  // told. Without this it was write-only after mount: edits went out through
  // onChange, nothing came back in, and uploading a file silently changed the
  // wizard's state while the visible document stayed put.
  //
  // Asserted against the REAL component on purpose. The suites that exercise
  // upload stub it with a controlled <textarea>, which honours `value` for
  // free — so the stub passed while the shipped editor did not.
  it('adopts a document supplied after mount', () => {
    const { rerender } = render(<OntologyEditor value="id: first" onChange={() => {}} />);
    expect(shownDoc()).toContain('id: first');

    act(() => { rerender(<OntologyEditor value="id: uploaded" onChange={() => {}} />); });
    expect(shownDoc()).toContain('id: uploaded');
    expect(shownDoc()).not.toContain('id: first');
  });

  // The guard that makes the sync safe to run every render: an echo of the
  // editor's own content must not be dispatched back into it, or the view
  // fights the user for the cursor on every keystroke.
  it('ignores a value identical to what it already shows', () => {
    const onChange = vi.fn();
    const { rerender } = render(<OntologyEditor value="id: same" onChange={onChange} />);
    onChange.mockClear();

    act(() => { rerender(<OntologyEditor value="id: same" onChange={onChange} />); });
    // A redundant dispatch would be a docChanged update, and the listener
    // would report it back out as an edit the user never made.
    expect(onChange).not.toHaveBeenCalled();
    expect(shownDoc()).toContain('id: same');
  });
});

// mapDiagnostic resolves a server (line, column) into CodeMirror (from, to)
// offsets, clamped to the CURRENT document — which may be shorter than the
// document the diagnostic was actually computed against, since validation is
// async and the user may keep typing while it's in flight. These pin the
// boundary cases the brief calls out: a stale diagnostic must never throw a
// range error, only land somewhere sane.
describe('mapDiagnostic', () => {
  // 3 lines, length 10: "a: 1\n"(0-4) "b: 2\n"(5-9) ""(10-10) — probed via
  // EditorState.create({ doc }).doc.{lines,length,line(n)}.
  const doc = EditorState.create({ doc: 'a: 1\nb: 2\n' });
  const empty = EditorState.create({ doc: '' });

  it('maps an in-range line/column to the expected offsets (sanity baseline)', () => {
    // line 2 ("b: 2", from=5), column 3 -> 5 + (3-1) = 7
    expect(mapDiagnostic(doc, { line: 2, column: 3, message: 'x' })).toEqual({
      from: 7, to: 8, severity: 'error', message: 'x',
    });
  });

  it('clamps a line beyond the document to the last line, then to doc length', () => {
    expect(mapDiagnostic(doc, { line: 99, column: 1, message: 'x' })).toEqual({
      from: 10, to: 10, severity: 'error', message: 'x',
    });
  });

  it('clamps a column beyond that line\'s length to the document length', () => {
    // line 1 ("a: 1", from=0, 4 chars), column 50 -> 0 + 49 = 49, clamped to 10
    expect(mapDiagnostic(doc, { line: 1, column: 50, message: 'x' })).toEqual({
      from: 10, to: 10, severity: 'error', message: 'x',
    });
  });

  it('never throws and stays at offset 0 for an empty document', () => {
    expect(() => mapDiagnostic(empty, { line: 5, column: 20, message: 'x' })).not.toThrow();
    expect(mapDiagnostic(empty, { line: 5, column: 20, message: 'x' })).toEqual({
      from: 0, to: 0, severity: 'error', message: 'x',
    });
  });
});
