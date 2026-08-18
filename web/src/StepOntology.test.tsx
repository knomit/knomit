import { useReducer } from 'react';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { StepOntology } from './StepOntology';
import { initialWizardState, wizardReducer } from './wizardState';
import { api } from './api';

const DEFAULT_SEED = 'id: general\nname: General\ntopics:\n  people:\n';
const CODE_SEED = 'id: source-code\nname: Code\ntopics:\n  invariants:\n';

// Stubbed on purpose: these tests are about StepOntology's own logic — where a
// document came from, when replacing it asks first, whether the summary can
// outlive its subject, and when Next may be pressed. The stub IS this
// component's contract with the editor (value in, onChange out). The real
// CodeMirror widget's own behaviour, including the value-sync a stub gives
// away for free, is pinned in OntologyEditor.test.tsx.
vi.mock('./OntologyEditor', () => ({
  OntologyEditor: ({ value, onChange }: { value: string; onChange: (v: string) => void }) => (
    <textarea data-testid="ontology-editor-proxy" value={value} onChange={e => onChange(e.target.value)} />
  ),
}));

vi.mock('./api', () => ({
  api: {
    ontologyPresets: vi.fn(),
    ontologyPresetYAML: vi.fn(),
    validateOntology: vi.fn(),
    ontologySchema: vi.fn(async () => []),
  },
}));

// StepOntology is a CONTROLLED component: it holds no private copy of the
// wizard's yaml, so its edits have to come back through onDispatch. These
// tests drive the real reducer — with a stub dispatch, state.yaml would never
// change and nothing downstream of it would ever run.
function Harness({ onValidityChange = () => {}, spy }: {
  onValidityChange?: (v: boolean) => void;
  spy?: (a: unknown) => void;
}) {
  const [state, dispatch] = useReducer(wizardReducer, initialWizardState);
  return (
    <StepOntology
      state={state}
      onDispatch={a => { spy?.(a); dispatch(a); }}
      onValidityChange={onValidityChange}
    />
  );
}

const sourceSelect = () => screen.getByTestId('ontology-source-select') as HTMLSelectElement;
const editor = () => screen.getByTestId('ontology-editor-proxy') as HTMLTextAreaElement;
const chooseSource = (v: string) => fireEvent.change(sourceSelect(), { target: { value: v } });
const type = (yaml: string) => fireEvent.change(editor(), { target: { value: yaml } });
const chooseFile = (file: File) =>
  fireEvent.change(screen.getByTestId('ontology-file'), { target: { files: [file] } });
const OK = { ok: true, id: 'x', name: 'X', topics: ['a'], rule_count: 1 };

describe('StepOntology', () => {
  // Implementations are RESTORED, not just cleared. mockResolvedValue and
  // mockImplementation install implementations, which clearAllMocks leaves in
  // place — so one test's deliberately never-resolving promise became every
  // later test's hung seed fetch, and the failures read as unrelated breaks in
  // components that were fine.
  beforeEach(() => {
    vi.clearAllMocks();
    (api.ontologyPresets as ReturnType<typeof vi.fn>).mockImplementation(async () => [
      { name: 'default', id: 'general', title: 'General', description: 'A broad shape', topics: ['people', 'technology'] },
      { name: 'code', id: 'source-code', title: 'Code', description: 'For a codebase', topics: ['invariants'] },
    ]);
    (api.ontologyPresetYAML as ReturnType<typeof vi.fn>).mockImplementation(
      async (name: string) => (name === 'code' ? CODE_SEED : DEFAULT_SEED));
    (api.validateOntology as ReturnType<typeof vi.fn>).mockImplementation(async () => OK);
  });

  // ── One field ──
  describe('a single source field', () => {
    it('opens on the starting preset, with its document already showing', async () => {
      render(<Harness />);
      await waitFor(() => expect(editor()).toHaveValue(DEFAULT_SEED));
      expect(sourceSelect()).toHaveValue('default');
      expect(screen.getByTestId('ontology-source')).toHaveTextContent('A broad shape');
    });

    it('offers presets, upload and blank from the one control', async () => {
      render(<Harness />);
      await waitFor(() => expect(api.ontologyPresets).toHaveBeenCalled());
      const values = Array.from(sourceSelect().options).map(o => o.value);
      expect(values).toEqual(['default', 'code', '__upload__', '__blank__']);
    });

    it('swaps the document when another preset is chosen', async () => {
      const spy = vi.fn();
      render(<Harness spy={spy} />);
      await waitFor(() => expect(editor()).toHaveValue(DEFAULT_SEED));

      chooseSource('code');
      await waitFor(() => expect(editor()).toHaveValue(CODE_SEED));
      expect(spy).toHaveBeenCalledWith({ type: 'SET_PRESET', preset: 'code' });
    });
  });

  // ── Editing is a fact about the document, not a mode ──
  describe('editing', () => {
    it('marks the document edited and offers a way back, without switching modes', async () => {
      render(<Harness />);
      await waitFor(() => expect(editor()).toHaveValue(DEFAULT_SEED));
      expect(screen.queryByTestId('ontology-edited')).not.toBeInTheDocument();

      type('id: mine\nname: Mine\ntopics:\n  a:\n');
      expect(screen.getByTestId('ontology-edited')).toBeInTheDocument();
      expect(screen.getByTestId('ontology-source')).toHaveTextContent('General');
      expect(screen.getByTestId('ontology-revert')).toBeInTheDocument();
    });

    // An untouched preset is server-supplied and already parses; sending it
    // back over the wire to be told so would be a pointless round trip, and
    // mode 'preset' exists precisely so the server uses its own copy.
    it('sends no yaml for an untouched preset, and yaml once edited', async () => {
      const spy = vi.fn();
      render(<Harness spy={spy} />);
      await waitFor(() => expect(editor()).toHaveValue(DEFAULT_SEED));
      expect(api.validateOntology).not.toHaveBeenCalled();
      expect(spy).not.toHaveBeenCalledWith(expect.objectContaining({ type: 'SET_YAML' }));

      type('id: mine\nname: Mine\ntopics:\n  a:\n');
      expect(spy).toHaveBeenCalledWith({ type: 'SET_YAML', yaml: 'id: mine\nname: Mine\ntopics:\n  a:\n' });
    });

    it('reverts to the preset it started from', async () => {
      render(<Harness />);
      await waitFor(() => expect(editor()).toHaveValue(DEFAULT_SEED));
      type('id: mine\nname: Mine\ntopics:\n  a:\n');

      fireEvent.click(screen.getByTestId('ontology-revert'));
      await waitFor(() => expect(editor()).toHaveValue(DEFAULT_SEED));
      expect(screen.queryByTestId('ontology-edited')).not.toBeInTheDocument();
    });
  });

  // ── Replacing asks only when it would destroy something ──
  describe('replacing the document', () => {
    it('swaps straight away when there is nothing to lose', async () => {
      render(<Harness />);
      await waitFor(() => expect(editor()).toHaveValue(DEFAULT_SEED));
      chooseSource('code');
      expect(screen.queryByTestId('ontology-replace-confirm')).not.toBeInTheDocument();
      await waitFor(() => expect(editor()).toHaveValue(CODE_SEED));
    });

    it('asks first when the document has been edited, and keeps it when refused', async () => {
      render(<Harness />);
      await waitFor(() => expect(editor()).toHaveValue(DEFAULT_SEED));
      type('id: mine\nname: Mine\ntopics:\n  a:\n');
      (api.ontologyPresetYAML as ReturnType<typeof vi.fn>).mockClear();

      chooseSource('code');
      expect(screen.getByTestId('ontology-replace-confirm')).toBeInTheDocument();
      expect(api.ontologyPresetYAML).not.toHaveBeenCalled();

      fireEvent.click(screen.getByTestId('ontology-replace-no'));
      expect(screen.queryByTestId('ontology-replace-confirm')).not.toBeInTheDocument();
      expect(editor()).toHaveValue('id: mine\nname: Mine\ntopics:\n  a:\n');
      expect(api.ontologyPresetYAML).not.toHaveBeenCalled();
    });

    it('replaces once confirmed', async () => {
      render(<Harness />);
      await waitFor(() => expect(editor()).toHaveValue(DEFAULT_SEED));
      type('id: mine\nname: Mine\ntopics:\n  a:\n');

      chooseSource('code');
      fireEvent.click(screen.getByTestId('ontology-replace-yes'));
      await waitFor(() => expect(editor()).toHaveValue(CODE_SEED));
      expect(screen.queryByTestId('ontology-edited')).not.toBeInTheDocument();
    });
  });

  // ── Upload is a source, not a mode ──
  describe('upload', () => {
    it('shows the file in the editor and names it as the source', async () => {
      render(<Harness />);
      await waitFor(() => expect(editor()).toHaveValue(DEFAULT_SEED));

      chooseFile(new File(['id: up\nname: Up\ntopics:\n  a:\n'], 'mine.yaml', { type: 'text/yaml' }));
      await waitFor(() => expect(editor()).toHaveValue('id: up\nname: Up\ntopics:\n  a:\n'));
      expect(screen.getByTestId('ontology-source')).toHaveTextContent('mine.yaml');
      // The select must be able to SHOW the file, or it snaps back to naming a
      // preset the editor is not holding.
      expect(sourceSelect().selectedOptions[0].textContent).toBe('mine.yaml');
    });

    it('reports the parsed summary and unlocks Next', async () => {
      (api.validateOntology as ReturnType<typeof vi.fn>).mockResolvedValue({
        ok: true, id: 'acme-eng', name: 'Acme', topics: ['invariants', 'runbooks'], rule_count: 12,
      });
      const onValidityChange = vi.fn();
      render(<Harness onValidityChange={onValidityChange} />);
      await waitFor(() => expect(editor()).toHaveValue(DEFAULT_SEED));

      chooseFile(new File(['id: acme-eng\nname: Acme\ntopics:\n  invariants:\n'], 'o.yaml', { type: 'text/yaml' }));
      const summary = await waitFor(() => screen.getByTestId('ontology-valid'));
      expect(summary).toHaveTextContent('acme-eng');
      expect(summary).toHaveTextContent(/2 topics/i);
      expect(summary).toHaveTextContent(/12 validation rules/i);
      await waitFor(() => expect(onValidityChange).toHaveBeenLastCalledWith(true));
    });

    // The point of folding upload into the one field: a file that does not
    // parse is editable where it failed, rather than needing to be fixed
    // outside knomit and uploaded again.
    it('leaves a file that fails validation in the editor, with diagnostics', async () => {
      (api.validateOntology as ReturnType<typeof vi.fn>).mockResolvedValue({
        ok: false, diagnostics: [{ line: 4, column: 3, message: 'must be lowercase kebab-case' }],
      });
      const onValidityChange = vi.fn();
      render(<Harness onValidityChange={onValidityChange} />);
      await waitFor(() => expect(editor()).toHaveValue(DEFAULT_SEED));

      chooseFile(new File(['id: x\nname: X\ntopics:\n  Runbooks:\n'], 'bad.yaml', { type: 'text/yaml' }));
      await waitFor(() => expect(screen.getByTestId('ontology-diagnostics')).toBeInTheDocument());
      expect(screen.getByText(/line 4/i)).toBeInTheDocument();
      expect(editor()).toHaveValue('id: x\nname: X\ntopics:\n  Runbooks:\n');
      await waitFor(() => expect(onValidityChange).toHaveBeenLastCalledWith(false));
    });

    it('rejects an oversize file before sending its contents to the server', async () => {
      render(<Harness />);
      await waitFor(() => expect(editor()).toHaveValue(DEFAULT_SEED));

      const oversize = 'x'.repeat(256 * 1024 + 1);
      chooseFile(new File([oversize], 'big.yaml', { type: 'text/yaml' }));
      await waitFor(() => expect(screen.getByTestId('ontology-file-error')).toHaveTextContent(/256 KiB/i));
      const sent = (api.validateOntology as ReturnType<typeof vi.fn>).mock.calls.map(c => c[0] as string);
      expect(sent.some(y => y.length > 256 * 1024)).toBe(false);
    });
  });

  // ── The summary describes what is on screen, and Next stays still ──
  describe('validation feedback', () => {
    it('never shows a summary for a document that has moved on', async () => {
      const validate = api.validateOntology as ReturnType<typeof vi.fn>;
      validate.mockResolvedValue({ ok: true, id: 'first', name: 'First', topics: ['a'], rule_count: 6 });
      render(<Harness />);
      await waitFor(() => expect(editor()).toHaveValue(DEFAULT_SEED));
      type('id: first\nname: First\ntopics:\n  a:\n');
      await waitFor(() => expect(screen.getByTestId('ontology-valid')).toHaveTextContent('6 validation rules'));

      validate.mockImplementation(() => new Promise(() => {})); // never resolves
      type('id: second\nname: Second\ntopics:\n  a:\n');
      await waitFor(() => expect(screen.getByTestId('ontology-checking')).toBeInTheDocument());
      expect(screen.queryByTestId('ontology-valid')).not.toBeInTheDocument();
    });

    it('holds Next steady while re-checking a document that was valid', async () => {
      const validate = api.validateOntology as ReturnType<typeof vi.fn>;
      const onValidityChange = vi.fn();
      render(<Harness onValidityChange={onValidityChange} />);
      await waitFor(() => expect(editor()).toHaveValue(DEFAULT_SEED));
      type('id: x\nname: X\ntopics:\n  a:\n');
      await waitFor(() => expect(onValidityChange).toHaveBeenLastCalledWith(true));

      onValidityChange.mockClear();
      validate.mockImplementation(() => new Promise(() => {})); // in flight
      type('id: x\nname: X\ntopics:\n  b:\n');
      await new Promise(r => setTimeout(r, 120));
      expect(onValidityChange).not.toHaveBeenCalledWith(false);
    });

    // A blank document is the reader's, so it is checked — and an empty one
    // cannot be valid.
    it('blocks Next on an empty document', async () => {
      const onValidityChange = vi.fn();
      render(<Harness onValidityChange={onValidityChange} />);
      await waitFor(() => expect(editor()).toHaveValue(DEFAULT_SEED));

      chooseSource('__blank__');
      await waitFor(() => expect(editor()).toHaveValue(''));
      expect(screen.getByTestId('ontology-source')).toHaveTextContent(/empty document/i);
      await waitFor(() => expect(onValidityChange).toHaveBeenLastCalledWith(false));
    });
  });

  // A seed that resolves after the reader moved on must never land: the
  // ontology is immutable once the repo exists.
  it('discards an abandoned seed fetch', async () => {
    let resolveSeed!: (yaml: string) => void;
    (api.ontologyPresetYAML as ReturnType<typeof vi.fn>).mockImplementation((name: string) => {
      if (name === 'code') return new Promise<string>(resolve => { resolveSeed = resolve; });
      return Promise.resolve(DEFAULT_SEED);
    });
    render(<Harness />);
    await waitFor(() => expect(editor()).toHaveValue(DEFAULT_SEED));

    chooseSource('code');
    await waitFor(() => expect(api.ontologyPresetYAML).toHaveBeenCalledWith('code'));
    // Back to the first preset before the second resolves.
    chooseSource('default');
    await waitFor(() => expect(editor()).toHaveValue(DEFAULT_SEED));

    resolveSeed(CODE_SEED);
    await new Promise(r => setTimeout(r, 20));
    expect(editor()).toHaveValue(DEFAULT_SEED);
  });

  it('links to the ontology documentation', () => {
    render(<Harness />);
    const link = screen.getByRole('link', { name: /what is an ontology/i });
    expect(link).toHaveAttribute('href', expect.stringContaining('knomit.io/docs'));
    expect(link).toHaveAttribute('target', '_blank');
  });
});
