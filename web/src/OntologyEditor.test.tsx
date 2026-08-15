import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
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
