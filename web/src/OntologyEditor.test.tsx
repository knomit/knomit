import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { OntologyEditor } from './OntologyEditor';

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
