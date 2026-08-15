import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { StepOntology } from './StepOntology';
import { initialWizardState } from './wizardState';
import { api } from './api';

vi.mock('./api', () => ({
  api: {
    ontologyPresets: vi.fn(async () => [
      { name: 'default', id: 'general', title: 'General', description: 'd', topics: ['people', 'technology'] },
      { name: 'code', id: 'source-code', title: 'Code', description: 'd', topics: ['invariants'] },
    ]),
    ontologyPresetYAML: vi.fn(async () => 'id: general\nname: General\ntopics:\n  people:\n'),
    validateOntology: vi.fn(),
    ontologySchema: vi.fn(async () => [{ struct: 'Ontology', field: 'id', doc: 'Identifier' }]),
  },
}));

describe('StepOntology', () => {
  beforeEach(() => vi.clearAllMocks());

  it('shows an uploaded file’s parsed summary and reports it valid', async () => {
    (api.validateOntology as ReturnType<typeof vi.fn>).mockResolvedValue({
      ok: true, id: 'acme-eng', name: 'Acme', topics: ['invariants', 'runbooks'], rule_count: 12,
    });
    const onValidityChange = vi.fn();
    render(<StepOntology state={initialWizardState} onDispatch={() => {}} onValidityChange={onValidityChange} />);

    fireEvent.click(screen.getByRole('button', { name: /upload a file/i }));
    const file = new File(['id: acme-eng\nname: Acme\ntopics:\n  invariants:\n'], 'o.yaml', { type: 'text/yaml' });
    fireEvent.change(screen.getByTestId('ontology-file'), { target: { files: [file] } });

    await waitFor(() => expect(screen.getByText(/acme-eng/)).toBeInTheDocument());
    expect(screen.getByText(/2 topics/i)).toBeInTheDocument();
    expect(screen.getByText(/12 validation rules/i)).toBeInTheDocument();
    await waitFor(() => expect(onValidityChange).toHaveBeenLastCalledWith(true));
  });

  it('blocks progress and shows the parser diagnostic when an upload fails to parse', async () => {
    (api.validateOntology as ReturnType<typeof vi.fn>).mockResolvedValue({
      ok: false,
      diagnostics: [{ line: 4, column: 3, message: 'parse ontology: invalid key "Runbooks" in topic: must be lowercase kebab-case' }],
    });
    const onValidityChange = vi.fn();
    render(<StepOntology state={initialWizardState} onDispatch={() => {}} onValidityChange={onValidityChange} />);

    fireEvent.click(screen.getByRole('button', { name: /upload a file/i }));
    const file = new File(['id: x\nname: X\ntopics:\n  Runbooks:\n'], 'bad.yaml', { type: 'text/yaml' });
    fireEvent.change(screen.getByTestId('ontology-file'), { target: { files: [file] } });

    await waitFor(() => expect(screen.getByText(/line 4/i)).toBeInTheDocument());
    expect(screen.getByText(/lowercase kebab-case/i)).toBeInTheDocument();
    await waitFor(() => expect(onValidityChange).toHaveBeenLastCalledWith(false));
  });

  it('a preset is valid immediately without a round trip', async () => {
    const onValidityChange = vi.fn();
    render(<StepOntology state={initialWizardState} onDispatch={() => {}} onValidityChange={onValidityChange} />);
    await waitFor(() => expect(api.ontologyPresets).toHaveBeenCalled());
    // Presets are server-supplied and already parse; validating them would be
    // a pointless round trip on the default path.
    expect(api.validateOntology).not.toHaveBeenCalled();
    await waitFor(() => expect(onValidityChange).toHaveBeenLastCalledWith(true));
  });

  it('seeds the editor from the selected preset when writing your own', async () => {
    render(<StepOntology state={initialWizardState} onDispatch={() => {}} onValidityChange={() => {}} />);
    fireEvent.click(screen.getByRole('button', { name: /write your own/i }));
    await waitFor(() => expect(api.ontologyPresetYAML).toHaveBeenCalledWith('default'));
  });

  it('links to the ontology documentation', () => {
    render(<StepOntology state={initialWizardState} onDispatch={() => {}} onValidityChange={() => {}} />);
    const link = screen.getByRole('link', { name: /what is an ontology/i });
    expect(link).toHaveAttribute('href', expect.stringContaining('knomit.io/docs'));
    expect(link).toHaveAttribute('target', '_blank');
  });
});
