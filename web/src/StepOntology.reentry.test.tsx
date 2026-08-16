import { useReducer, useState } from 'react';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import { act, render, screen, fireEvent, waitFor } from '@testing-library/react';
import { StepOntology } from './StepOntology';
import { createBodyFor, initialWizardState, wizardReducer } from './wizardState';
import { api } from './api';

// These tests drive the CARDS, not the internals: the review finding is a
// sequence of ordinary clicks ("code" → "Write your own" → edit → "Write your
// own" again), and the damage it did was to the ontology the repo is created
// with. So every assertion below is on what the editor shows and on what
// createBodyFor would send — never on whether some guard function ran.
//
// OntologyEditor is stubbed by a textarea because that IS StepOntology's
// contract with it (value in, onChange out — see StepOntology.tsx's header
// comment); the real CodeMirror widget adds nothing to these cases and cannot
// be typed into reliably under jsdom.
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
    ontologyPresets: vi.fn(async () => [
      { name: 'default', id: 'general', title: 'General', description: 'd', topics: ['people'] },
      { name: 'code', id: 'source-code', title: 'Code', description: 'd', topics: ['invariants'] },
    ]),
    ontologyPresetYAML: vi.fn(async (name: string) => (name === 'code' ? CODE_YAML : DEFAULT_YAML)),
    validateOntology: vi.fn(async () => ({ ok: true, id: 'x', name: 'X', topics: ['a'], rule_count: 1 })),
    ontologySchema: vi.fn(async () => []),
  },
}));

// Harness wires StepOntology to the REAL reducer, so SET_YAML/SET_PRESET have
// their real consequences (notably: SET_YAML clears state.preset, which is the
// mechanism that made the re-seed resolve to 'default'). The rendered JSON is
// the wire body the wizard would POST.
// `showing` mirrors CreateRepoWizard's `{step === 'ontology' && <StepOntology
// …>}`: leaving the step UNMOUNTS this component and coming back MOUNTS a
// fresh one, so anything it remembers in a ref is gone while the reducer state
// below survives. Tests that never leave the step simply never touch it.
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
const clickWriteOwn = () => fireEvent.click(screen.getByRole('button', { name: /write your own/i }));
const clickUpload = () => fireEvent.click(screen.getByRole('button', { name: /upload a file/i }));
// Back, then Next — the wizard's own way of unmounting and remounting the step.
const leaveAndReturn = () => {
  fireEvent.click(screen.getByTestId('toggle-step'));
  fireEvent.click(screen.getByTestId('toggle-step'));
};

describe('StepOntology re-entry into a choice already selected', () => {
  beforeEach(() => vi.clearAllMocks());

  // The finding, verbatim as a click sequence. Before the fix, step 5 refetched
  // — and because SET_YAML had cleared state.preset, `state.preset ||
  // 'default'` resolved to 'default', so the user who selected "code" and
  // edited it was silently given the DEFAULT ontology instead. The ontology is
  // immutable after repo creation: there is no repair short of deleting and
  // recreating the repo.
  it('keeps the edited YAML when "Write your own" is clicked again, and never refetches the default', async () => {
    render(<Harness />);
    await waitFor(() => expect(api.ontologyPresets).toHaveBeenCalled());

    fireEvent.click(screen.getByRole('button', { name: /^Code/i }));
    clickWriteOwn();
    await waitFor(() => expect(editor()).toHaveValue(CODE_YAML));

    fireEvent.change(editor(), { target: { value: EDITED_YAML } });
    expect(editor()).toHaveValue(EDITED_YAML);

    // The still-highlighted card, clicked a second time. Flush pending
    // microtasks first: a re-seed would land a tick later, so asserting
    // immediately would pass even against the unguarded version.
    clickWriteOwn();
    await act(async () => { await Promise.resolve(); await Promise.resolve(); });

    expect(editor()).toHaveValue(EDITED_YAML);
    expect(api.ontologyPresetYAML).toHaveBeenCalledTimes(1);
    expect(api.ontologyPresetYAML).not.toHaveBeenCalledWith('default');
    expect(createBody()).toEqual({ name: 'kb', mode: 'custom', ontology_yaml: EDITED_YAML });
  });

  // The other half of the same finding: when a re-seed IS legitimate (nothing
  // to preserve), it must use the preset the user actually chose. state.preset
  // cannot answer that — SET_YAML cleared it — so a fallback read off it would
  // hand back "default" here.
  it('re-seeds an emptied editor from the chosen preset, not from the default', async () => {
    render(<Harness />);
    await waitFor(() => expect(api.ontologyPresets).toHaveBeenCalled());

    fireEvent.click(screen.getByRole('button', { name: /^Code/i }));
    clickWriteOwn();
    await waitFor(() => expect(editor()).toHaveValue(CODE_YAML));

    // The user clears the editor themselves — now there is nothing to lose.
    fireEvent.change(editor(), { target: { value: '' } });
    clickWriteOwn();

    await waitFor(() => expect(editor()).toHaveValue(CODE_YAML));
    expect(api.ontologyPresetYAML).toHaveBeenCalledTimes(2);
    expect(api.ontologyPresetYAML).toHaveBeenLastCalledWith('code');
    expect(createBody()).toEqual({ name: 'kb', mode: 'custom', ontology_yaml: CODE_YAML });
  });

  // "Write your own" reached from an upload must not throw the upload away
  // either — it opens the editor ON the uploaded ontology. Same invariant: no
  // user-visible ontology content is replaced without being asked for.
  it('opens the editor on an uploaded ontology instead of overwriting it with a preset', async () => {
    render(<Harness />);
    await waitFor(() => expect(api.ontologyPresets).toHaveBeenCalled());

    clickUpload();
    const file = new File([EDITED_YAML], 'mine.yaml', { type: 'text/yaml' });
    fireEvent.change(screen.getByTestId('ontology-file'), { target: { files: [file] } });
    await waitFor(() => expect(createBody().ontology_yaml).toBe(EDITED_YAML));

    clickWriteOwn();
    await act(async () => { await Promise.resolve(); await Promise.resolve(); });
    expect(editor()).toHaveValue(EDITED_YAML);
    expect(api.ontologyPresetYAML).not.toHaveBeenCalled();
    expect(createBody()).toEqual({ name: 'kb', mode: 'custom', ontology_yaml: EDITED_YAML });
  });

  // Same finding, reached by LEAVING the step instead of re-clicking a card.
  // CreateRepoWizard renders `{step === 'ontology' && <StepOntology …>}`, so
  // Back-then-Next unmounts and remounts this component — and a preset
  // remembered only in a component-local ref does not survive that.
  it('re-seeds from the chosen preset even after the step is unmounted and remounted', async () => {
    render(<Harness />);
    await waitFor(() => expect(api.ontologyPresets).toHaveBeenCalled());

    fireEvent.click(screen.getByRole('button', { name: /^Code/i }));
    clickWriteOwn();
    await waitFor(() => expect(editor()).toHaveValue(CODE_YAML));
    fireEvent.change(editor(), { target: { value: '' } });

    leaveAndReturn();
    await waitFor(() => expect(screen.getByTestId('step-ontology')).toBeInTheDocument());
    clickWriteOwn();

    await waitFor(() => expect(editor()).toHaveValue(CODE_YAML));
    expect(api.ontologyPresetYAML).toHaveBeenLastCalledWith('code');
  });

  // The damage the remount did on its own, with no further clicks: mode falls
  // back to 'preset' because there is no yaml, but SET_YAML had already cleared
  // state.preset — so no card renders selected while Next reports valid, and
  // createBodyFor's `state.preset || 'default'` sends the DEFAULT ontology to a
  // user who chose "code".
  it('does not report valid on remount when the emptied editor left no selection', async () => {
    const onValidityChange = vi.fn();
    render(<Harness onValidityChange={onValidityChange} />);
    await waitFor(() => expect(api.ontologyPresets).toHaveBeenCalled());

    fireEvent.click(screen.getByRole('button', { name: /^Code/i }));
    clickWriteOwn();
    await waitFor(() => expect(editor()).toHaveValue(CODE_YAML));
    fireEvent.change(editor(), { target: { value: '' } });

    onValidityChange.mockClear();
    leaveAndReturn();
    await waitFor(() => expect(screen.getByTestId('step-ontology')).toBeInTheDocument());

    expect(onValidityChange).toHaveBeenLastCalledWith(false);
    expect(screen.getByText(/pick one of the choices above/i)).toBeInTheDocument();
  });

  // Re-clicking "Upload a file" while already on it used to reset the Next
  // gate to false while leaving the uploaded ontology (and its validation
  // summary) on screen — a wizard that refuses to advance for a file it is
  // still showing as valid.
  it('leaves an already-uploaded ontology and its Next gate alone when "Upload a file" is re-clicked', async () => {
    const onValidityChange = vi.fn();
    render(<Harness onValidityChange={onValidityChange} />);
    await waitFor(() => expect(api.ontologyPresets).toHaveBeenCalled());

    clickUpload();
    const file = new File([EDITED_YAML], 'mine.yaml', { type: 'text/yaml' });
    fireEvent.change(screen.getByTestId('ontology-file'), { target: { files: [file] } });
    await waitFor(() => expect(onValidityChange).toHaveBeenLastCalledWith(true));

    clickUpload();

    expect(createBody()).toEqual({ name: 'kb', mode: 'custom', ontology_yaml: EDITED_YAML });
    expect(onValidityChange).toHaveBeenLastCalledWith(true);
  });
});
