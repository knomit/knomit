import { useReducer, useRef, useState } from 'react';
import { api, type CreateEvent, type ProbeResult } from './api';
import { wizardReducer, initialWizardState, currentStep, createBodyFor, authFor, isValidRepoName } from './wizardState';
import { WizardStepRail } from './WizardStepRail';
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
// The probe is asked TWICE for a remote that needs credentials, and that is
// the whole shape of this component's async logic. §3's outcome table models
// three outcomes (has-refs / empty / no-remote) and never models auth-required
// as a fourth — but it IS one, and it is the state where emptiness is not
// merely false but UNKNOWN. An anonymous probe of a private empty remote
// answers {auth_required:true, empty:false}, which stepsFor cannot tell from
// "has content"; taking it at face value is what produced mode 'clone' for the
// exact case seed mode exists to serve. So: probe #1 from the source step
// (anonymous, or with whatever is already typed), credentials on the access
// step, probe #2 before leaving it. Nothing advances past access on an
// auth_required answer.
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
  // Gates Next on the ontology step. Passed to StepOntology as setOntologyValid
  // directly (not a wrapping lambda) so its identity is stable across
  // renders — StepOntology's validity effects depend on it and a fresh
  // lambda every render would refire them for no reason.
  const [ontologyValid, setOntologyValid] = useState(false);
  // Held in a ref, not state: aborting a probe must not itself cause a render,
  // and the controller has to survive the render that `probing` triggers.
  const probeAbort = useRef<AbortController | null>(null);

  const step = currentStep(state);

  // runProbe carries the wizard's CREDENTIALS, always. Probing anonymously and
  // creating with a token would derive the step list — and therefore the mode —
  // from an answer about a different request: a private empty remote probes as
  // {auth_required:true, empty:false}, which stepsFor reads as "has content" and
  // createBodyFor turns into mode 'clone'. The clone then seeds the hardcoded
  // default ontology and pushes nothing, silently discarding the ontology the
  // user chose. authFor is shared with createBodyFor so the two cannot drift.
  const runProbe = async (): Promise<ProbeResult | null> => {
    // The server bounds the probe by Cfg.Git.NetworkTimeout, but that budget
    // is measured in minutes: without a client-side abort a typo'd host leaves
    // the step frozen for the whole of it. Design §6: "the step stays
    // interactive with a visible cancel".
    probeAbort.current?.abort();
    const ctl = new AbortController();
    probeAbort.current = ctl;
    setProbeError(''); setProbing(true);
    try {
      const probe = await api.probeOrigin(
        { url: state.url, branch: state.branch || undefined, ...authFor(state) }, ctl.signal);
      dispatch({ type: 'PROBE_DONE', probe });
      // A probe that came back but couldn't reach the remote is not an
      // exception — it's a normal 200 with reachable: false — so the error
      // is read off the result, not a catch block.
      if (!probe.reachable) setProbeError(probe.detail || 'Could not reach that remote.');
      return probe;
    } catch (e) {
      // A cancel the user asked for is not a failure to report back to them.
      if (ctl.signal.aborted) return null;
      setProbeError(e instanceof Error ? e.message : String(e));
      return null;
    } finally {
      setProbing(false);
    }
  };

  const cancelProbe = () => probeAbort.current?.abort();

  // The source step's Connect. auth_required is NOT an error here — it is the
  // next question, and StepAccess asks it.
  const handleProbe = () => { void runProbe(); };

  // The access step's re-probe. Same call, different reading of the same
  // answer: the user has now supplied credentials, so a remote that still
  // reports auth_required has REFUSED them, and that is a failure worth
  // saying out loud.
  const reprobeWithCredentials = async (): Promise<ProbeResult | null> => {
    const probe = await runProbe();
    if (probe?.reachable && probe.auth_required) {
      setProbeError(probe.detail
        ? `Those credentials did not give access to this remote: ${probe.detail}`
        : 'Those credentials did not give access to this remote.');
    }
    return probe;
  };

  // Advancing off the access step re-probes FIRST whenever the last probe came
  // back auth_required, because that probe never established whether the remote
  // is empty — it could not look. Advancing on it is what routed a private
  // empty remote to mode 'clone'. A re-probe that succeeds may add an
  // 'ontology' step to stepsFor, so PROBE_DONE's own "land on access" rule
  // (stepIndex 1, which is 'access' in every remote list) runs first and the
  // NEXT below then steps into whichever step the NEW list has there.
  const handleAccessNext = async () => {
    if (!state.probe?.auth_required) { dispatch({ type: 'NEXT' }); return; }
    const probe = await reprobeWithCredentials();
    if (!probe || !probe.reachable || probe.auth_required) return;
    dispatch({ type: 'NEXT' });
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
  // Mirrors the backend rule rather than merely "non-empty": "My KB" used to
  // sail through every step and come back as a 400 from POST /repos — the very
  // 400 the deleted form's own comment called confusing.
  const nameOk = isValidRepoName(state.name);
  // The generic "Next" footer button covers every step that has nothing
  // more specific to say about advancing: the local-only 'name' step,
  // 'access', and 'ontology' (gated on ontologyValid — StepOntology's own
  // verification round trip, not this button, decides when the chosen
  // ontology is safe to carry forward). 'source' advances via its own two
  // choice controls (CHOOSE_LOCAL, or a successful probe); 'review' submits
  // instead of advancing.
  const showGenericNext = step === 'name' || step === 'access' || step === 'ontology';
  const nextDisabled = creating || probing || !nameOk || (step === 'access' && basicMissingUser) ||
    (step === 'ontology' && !ontologyValid);

  return (
    <div>
      <h3 style={heading}>New repository</h3>
      <WizardStepRail state={state} dispatch={dispatch} />

      {step === 'source' && (
        <StepSource state={state} dispatch={dispatch} onProbe={handleProbe} onCancelProbe={cancelProbe}
          probing={probing} probeError={probeError} />
      )}
      {/* Local-only's dedicated 'name' step (stepsFor: ['source','name',
          'ontology','review']). There is no StepName.tsx per the brief's file
          list — this is small enough to stay inline, and Task 9 has no reason
          to touch it. A reachable remote never lands here: its name field
          lives on StepAccess instead, per stepsFor's remote-path lists. */}
      {step === 'name' && (
        <div>
          <label style={label}>Name</label>
          {/* Same WKWebView guard as StepAccess's name field — see CreateRepoForm.tsx:81-83. */}
          <input data-testid="create-name" style={input} placeholder="e.g. work (a–z, 0–9, -, _)" value={state.name}
            autoCapitalize="off" autoCorrect="off" spellCheck={false}
            onChange={e => dispatch({ type: 'SET_NAME', name: e.target.value })} />
          {state.name !== '' && !nameOk && (
            <div data-testid="name-invalid" style={warnText}>Use lowercase letters, digits, - and _ only.</div>
          )}
          <div style={hint}>Local-only — nothing leaves this machine until you connect a remote later.</div>
        </div>
      )}
      {step === 'access' && (
        <StepAccess state={state} dispatch={dispatch} onCancelProbe={cancelProbe}
          onProbe={() => { void reprobeWithCredentials(); }} probing={probing} probeError={probeError} />
      )}
      {step === 'ontology' && <StepOntology state={state} onDispatch={dispatch} onValidityChange={setOntologyValid} />}
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
          <button type="button" style={btn(nextDisabled, 'primary')} disabled={nextDisabled}
            onClick={step === 'access' ? () => { void handleAccessNext(); } : () => dispatch({ type: 'NEXT' })}>
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
const label: React.CSSProperties = { fontSize: 12, color: '#888', marginBottom: 4, marginTop: 12, display: 'block' };
const input: React.CSSProperties = { width: '100%', boxSizing: 'border-box', background: '#111', border: '1px solid #333', color: '#eee', padding: '6px 8px', borderRadius: 4, fontSize: 13 };
const hint: React.CSSProperties = { fontSize: 12, color: '#666', marginTop: 8, lineHeight: 1.5 };
const warnText: React.CSSProperties = { color: '#d2a24c', fontSize: 12, marginTop: 6 };
