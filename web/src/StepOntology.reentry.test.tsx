import { useReducer, useState } from 'react';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { StepOntology } from './StepOntology';
import { createBodyFor, initialWizardState, wizardReducer } from './wizardState';
import { api } from './api';

// These tests drive the CONTROL, not the internals: the original review
// finding was a sequence of ordinary clicks, and the damage it did was to the
// ontology the repo gets created with. So every assertion is on what the
// editor shows and on what createBodyFor would send — never on whether some
// guard function ran.
//
// OntologyEditor is stubbed by a textarea because that IS StepOntology's
// contract with it (value in, onChange out); the real CodeMirror widget adds
// nothing to these cases and cannot be typed into reliably under jsdom.
const CODE_YAML = 'id: source-code\nname: Code\ntopics:\n  invariants:\n';
const DEFAULT_YAML = 'id: general\nname: General\ntopics:\n  people:\n';
const EDITED_YAML = 'id: source-code\nname: Code (mine)\ntopics:\n  invariants:\n  runbooks:\n';

vi.mock('./OntologyEditor', () => ({
  OntologyEditor: ({ value, onChange }: { value: string; onChange: (v: string) => void }) => (
    <textarea data-testid="ontology-editor" value={value} onChange={e => onChange(e.target.value)} />
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

// Harness wires StepOntology to the REAL reducer, so SET_YAML/SET_PRESET have
// their real consequences (notably: SET_YAML clears state.preset, which is the
// mechanism that once made a re-seed resolve to 'default'). The rendered JSON
// is the wire body the wizard would POST.
//
// `showing` mirrors CreateRepoWizard's `{step === 'ontology' && <StepOntology
// …>}`: leaving the step UNMOUNTS this component and coming back MOUNTS a
// fresh one, so anything it holds in local state is gone while the reducer
// state below survives. That is exactly the boundary these tests probe.
function Harness({ onValidityChange = () => {} }: { onValidityChange?: (v: boolean) => void }) {
  const [state, dispatch] = useReducer(wizardReducer, { ...initialWizardState, choice: 'local', name: 'kb' });
  const [showing, setShowing] = useState(true);
  return (
    <>
      {showing && <StepOntology state={state} onDispatch={dispatch} onValidityChange={onValidityChange} />}
      <button type="button" data-testid="toggle-step" onClick={() => setShowing(s => !s)}>toggle</button>
      <pre data-testid="create-body">{JSON.stringify(createBodyFor(state))}</pre>
    </>
  );
}

const createBody = () => JSON.parse(screen.getByTestId('create-body').textContent || '{}');
const editor = () => screen.getByTestId('ontology-editor') as HTMLTextAreaElement;
const chooseSource = (v: string) =>
  fireEvent.change(screen.getByTestId('ontology-source-select'), { target: { value: v } });
const type = (yaml: string) => fireEvent.change(editor(), { target: { value: yaml } });
// Back, then Next — the wizard's own way of unmounting and remounting the step.
const leaveAndReturn = () => {
  fireEvent.click(screen.getByTestId('toggle-step'));
  fireEvent.click(screen.getByTestId('toggle-step'));
};

describe('StepOntology across leaving and returning to the step', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    (api.ontologyPresets as ReturnType<typeof vi.fn>).mockImplementation(async () => [
      { name: 'default', id: 'general', title: 'General', description: 'd', topics: ['people'] },
      { name: 'code', id: 'source-code', title: 'Code', description: 'd', topics: ['invariants'] },
    ]);
    (api.ontologyPresetYAML as ReturnType<typeof vi.fn>).mockImplementation(
      async (name: string) => (name === 'code' ? CODE_YAML : DEFAULT_YAML));
    (api.validateOntology as ReturnType<typeof vi.fn>).mockImplementation(
      async () => ({ ok: true, id: 'x', name: 'X', topics: ['a'], rule_count: 1 }));
  });

  // The finding, as a click sequence. The editor's contents live in component
  // state, which the unmount destroys — so the reader's work only survives
  // because SET_YAML put it in the reducer, and the remount reads it back.
  it('keeps edited YAML across a round trip out of the step', async () => {
    render(<Harness />);
    // Wait for the starting preset to land: the select has no options until
    // the preset list resolves, so a change fired before that goes nowhere.
    await waitFor(() => expect(editor()).toHaveValue(DEFAULT_YAML));
    chooseSource('code');
    await waitFor(() => expect(editor()).toHaveValue(CODE_YAML));

    type(EDITED_YAML);
    expect(createBody()).toEqual({ name: 'kb', mode: 'custom', ontology_yaml: EDITED_YAML });

    leaveAndReturn();
    await waitFor(() => expect(screen.getByTestId('step-ontology')).toBeInTheDocument());
    expect(editor()).toHaveValue(EDITED_YAML);
    expect(createBody()).toEqual({ name: 'kb', mode: 'custom', ontology_yaml: EDITED_YAML });
  });

  // A returning reader must still be told their document is edited, and still
  // be offered the way back — the marker is derived from wizard state, not
  // from anything the unmounted component remembered.
  it('still shows the document as edited after returning', async () => {
    render(<Harness />);
    // Wait for the starting preset to land: the select has no options until
    // the preset list resolves, so a change fired before that goes nowhere.
    await waitFor(() => expect(editor()).toHaveValue(DEFAULT_YAML));
    chooseSource('code');
    await waitFor(() => expect(editor()).toHaveValue(CODE_YAML));
    type(EDITED_YAML);

    leaveAndReturn();
    await waitFor(() => expect(screen.getByTestId('step-ontology')).toBeInTheDocument());
    expect(screen.getByTestId('ontology-edited')).toBeInTheDocument();
    expect(screen.getByTestId('ontology-revert')).toBeInTheDocument();
  });

  // Reverting must restore the preset the reader actually chose. state.preset
  // cannot answer that — SET_YAML cleared it — so a fallback read off it would
  // hand back "default" here, which is the original finding.
  it('reverts to the chosen preset after a round trip, not to the default', async () => {
    render(<Harness />);
    // Wait for the starting preset to land: the select has no options until
    // the preset list resolves, so a change fired before that goes nowhere.
    await waitFor(() => expect(editor()).toHaveValue(DEFAULT_YAML));
    chooseSource('code');
    await waitFor(() => expect(editor()).toHaveValue(CODE_YAML));
    type(EDITED_YAML);

    leaveAndReturn();
    await waitFor(() => expect(screen.getByTestId('ontology-revert')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('ontology-revert'));

    await waitFor(() => expect(editor()).toHaveValue(CODE_YAML));
    expect(api.ontologyPresetYAML).toHaveBeenLastCalledWith('code');
    expect(createBody()).toEqual({ name: 'kb', mode: 'preset', ontology_preset: 'code' });
  });

  // Returning to an UNTOUCHED preset must not resend it as custom yaml: the
  // wire body stays mode 'preset', so the server uses its own copy.
  it('keeps an untouched preset on mode preset across a round trip', async () => {
    render(<Harness />);
    // Wait for the starting preset to land: the select has no options until
    // the preset list resolves, so a change fired before that goes nowhere.
    await waitFor(() => expect(editor()).toHaveValue(DEFAULT_YAML));
    chooseSource('code');
    await waitFor(() => expect(editor()).toHaveValue(CODE_YAML));
    expect(createBody()).toEqual({ name: 'kb', mode: 'preset', ontology_preset: 'code' });

    leaveAndReturn();
    await waitFor(() => expect(editor()).toHaveValue(CODE_YAML));
    expect(createBody()).toEqual({ name: 'kb', mode: 'preset', ontology_preset: 'code' });
    expect(screen.queryByTestId('ontology-edited')).not.toBeInTheDocument();
  });

  // An uploaded document is the reader's work too, and survives the same way.
  it('keeps an uploaded ontology across a round trip', async () => {
    render(<Harness />);
    await waitFor(() => expect(editor()).toHaveValue(DEFAULT_YAML));
    fireEvent.change(screen.getByTestId('ontology-file'), {
      target: { files: [new File([EDITED_YAML], 'mine.yaml', { type: 'text/yaml' })] },
    });
    await waitFor(() => expect(editor()).toHaveValue(EDITED_YAML));

    leaveAndReturn();
    await waitFor(() => expect(screen.getByTestId('step-ontology')).toBeInTheDocument());
    expect(editor()).toHaveValue(EDITED_YAML);
    expect(createBody()).toEqual({ name: 'kb', mode: 'custom', ontology_yaml: EDITED_YAML });
  });

  // An emptied editor blocks Next — and leaving the step recovers, rather than
  // stranding the reader.
  //
  // The old step's worst state lived here: an emptied editor left NO card
  // selected (SET_YAML had cleared state.preset) while Next still reported
  // valid, so createBodyFor's `state.preset || 'default'` shipped the default
  // ontology to someone who had chosen "code" — permanently, since the
  // ontology is immutable after creation. With one source field there is
  // always a selection, so returning re-seeds from the remembered starting
  // point and says so, instead of presenting an unselected dead end.
  it('blocks Next on an empty document, and recovers to the starting preset on return', async () => {
    const onValidityChange = vi.fn();
    render(<Harness onValidityChange={onValidityChange} />);
    await waitFor(() => expect(editor()).toHaveValue(DEFAULT_YAML));

    type('');
    await waitFor(() => expect(onValidityChange).toHaveBeenLastCalledWith(false));

    leaveAndReturn();
    await waitFor(() => expect(editor()).toHaveValue(DEFAULT_YAML));
    expect(screen.queryByTestId('ontology-edited')).not.toBeInTheDocument();
    expect(createBody()).toEqual({ name: 'kb', mode: 'preset', ontology_preset: 'default' });
  });
});
