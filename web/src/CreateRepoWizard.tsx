import { useReducer, useState } from 'react';
import { api, type CreateEvent } from './api';
import { wizardReducer, initialWizardState, currentStep, createBodyFor } from './wizardState';
import { StepSource } from './StepSource';
import { StepAccess } from './StepAccess';
import { StepOntology } from './StepOntology';
import { StepReview } from './StepReview';
import { CreateProgress } from './CreateProgress';
import { btn } from './manageStyles';

// CreateRepoWizard replaces CreateRepoForm's flat preset/custom/clone tabs
// with a guided flow: ask for a URL (or "keep it local"), let the REMOTE
// classify itself via api.probeOrigin, and only then show the steps that
// case actually needs — stepsFor (wizardState.ts) derives that list, this
// component never invents its own.
//
// This component owns the reducer and the two async calls (probe, submit);
// every Step* component below is presentational, taking `state` + a plain
// dispatch and rendering fields, same contract as CreateRepoForm's value/
// onChange props.
//
// onCancel stays optional and is omitted at zero repos: with no
// repositories the create surface IS the fallback selection, so Cancel
// would be a button that visibly does nothing (kb/invariants/repos/
// no-default-repo/1fd5dad8.md — zero repos is a valid steady state, not an
// edge case). See RepoManager.tsx's `repos.length === 0 ? undefined : …`
// call site, which this component's prop signature exists to serve.
export function CreateRepoWizard({ onDone, onCancel }: { onDone: (name: string) => void; onCancel?: () => void }) {
  const [state, dispatch] = useReducer(wizardReducer, initialWizardState);
  const [probing, setProbing] = useState(false);
  const [probeError, setProbeError] = useState('');
  const [creating, setCreating] = useState(false);
  const [createErr, setCreateErr] = useState('');
  const [events, setEvents] = useState<CreateEvent[]>([]);

  const step = currentStep(state);

  const handleProbe = async () => {
    setProbeError(''); setProbing(true);
    try {
      const probe = await api.probeOrigin({ url: state.url, branch: state.branch || undefined });
      dispatch({ type: 'PROBE_DONE', probe });
      // A probe that came back but couldn't reach the remote is not an
      // exception — it's a normal 200 with reachable: false — so the error
      // is read off the result, not a catch block.
      if (!probe.reachable) setProbeError(probe.detail || 'Could not reach that remote.');
    } catch (e) {
      setProbeError(e instanceof Error ? e.message : String(e));
    } finally {
      setProbing(false);
    }
  };

  const handleCreate = async () => {
    setCreateErr(''); setEvents([]); setCreating(true);
    const body = createBodyFor(state);
    let failed = false;
    let doneName = state.name;
    try {
      await api.createRepo(body, (e) => {
        setEvents(prev => [...prev, e]);
        if (e.type === 'done' && e.repo) doneName = e.repo.name;
        if (e.type === 'error') { failed = true; setCreateErr(e.detail || e.title || 'create failed'); }
      });
      if (!failed) onDone(doneName);
    } catch (e) {
      setCreateErr(e instanceof Error ? e.message : String(e));
    } finally {
      setCreating(false);
    }
  };

  // Keep the basic-auth username requirement from CreateRepoForm: a colon-
  // less token reads on the backend as Password with an empty Username, the
  // exact broken-credential case basic support exists to avoid.
  const basicMissingUser = state.authMethod === 'basic' && state.authUser.trim() === '';
  const nameOk = state.name.trim().length > 0;
  // The generic "Next" footer button covers every step that has nothing
  // more specific to say about advancing: the local-only confirmation
  // ('name' — StepSource in its confirmOnly rendering), 'access', and the
  // placeholder 'ontology' step. 'source' advances via its own two choice
  // controls (CHOOSE_LOCAL, or a successful probe); 'review' submits
  // instead of advancing.
  const showGenericNext = step === 'name' || step === 'access' || step === 'ontology';
  const nextDisabled = creating || !nameOk || (step === 'access' && basicMissingUser);

  return (
    <div>
      <h3 style={heading}>New repository</h3>

      {step === 'source' && (
        <StepSource state={state} dispatch={dispatch} onProbe={handleProbe} probing={probing} probeError={probeError} confirmOnly={false} />
      )}
      {step === 'name' && (
        <StepSource state={state} dispatch={dispatch} onProbe={handleProbe} probing={probing} probeError={probeError} confirmOnly={true} />
      )}
      {step === 'access' && <StepAccess state={state} dispatch={dispatch} />}
      {step === 'ontology' && <StepOntology state={state} dispatch={dispatch} />}
      {step === 'review' && <StepReview state={state} />}

      {createErr && <div style={errText}>{createErr}</div>}
      <CreateProgress events={events} />

      <div style={footer}>
        {step !== 'source' && (
          <button type="button" style={btn(creating)} disabled={creating} onClick={() => dispatch({ type: 'BACK' })}>
            Back
          </button>
        )}
        {showGenericNext && (
          <button type="button" style={btn(nextDisabled, 'primary')} disabled={nextDisabled} onClick={() => dispatch({ type: 'NEXT' })}>
            Next
          </button>
        )}
        {step === 'review' && (
          <button type="button" style={btn(creating || !nameOk, 'primary')} disabled={creating || !nameOk} onClick={handleCreate}>
            {creating ? 'Creating…' : 'Create repository'}
          </button>
        )}
        {onCancel && (
          <button type="button" style={btn(creating)} disabled={creating} onClick={onCancel}>Cancel</button>
        )}
      </div>
    </div>
  );
}

const heading: React.CSSProperties = { margin: '0 0 14px', fontSize: 16 };
const footer: React.CSSProperties = { display: 'flex', gap: 8, marginTop: 16 };
const errText: React.CSSProperties = { color: '#f88', fontSize: 13, marginTop: 8 };
