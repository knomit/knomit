import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import { OntologyEditor } from './OntologyEditor';

// SPIKE (Task 6): asserts the mechanical half of the WKWebView-corruption
// mitigation — the guard attributes actually land on CodeMirror's contentDOM,
// which is the element the WKWebView keyboard/autocorrect heuristics act on.
describe('OntologyEditor', () => {
  it('turns off autocapitalize, autocorrect and spellcheck on the CodeMirror content DOM', () => {
    render(<OntologyEditor value="topics: {}" onChange={() => {}} />);
    const content = screen.getByTestId('ontology-editor').querySelector('.cm-content');
    expect(content).not.toBeNull();
    expect(content).toHaveAttribute('autocapitalize', 'off');
    expect(content).toHaveAttribute('autocorrect', 'off');
    expect(content).toHaveAttribute('spellcheck', 'false');
  });

  it('renders the initial document text', () => {
    render(<OntologyEditor value="topics: {}" onChange={() => {}} />);
    expect(screen.getByTestId('ontology-editor').textContent).toContain('topics');
  });
});
