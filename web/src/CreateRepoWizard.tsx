import { useReducer, useRef, useState } from 'react';
import { api, type CreateEvent, type ProbeResult } from './api';
import { wizardReducer, initialWizardState, currentStep, stepsFor, branchCheckBlocked, probeIsCurrent, createBodyFor, authFor, isValidRepoName, type WizardAction } from './wizardState';
import { WizardStepRail } from './WizardStepRail';
import { StepSource } from './StepSource';
import { StepAccess } from './StepAccess';
import { StepBranch } from './StepBranch';
import { StepOntology } from './StepOntology';
import { StepReview } from './StepReview';
import { CreateProgress } from './CreateProgress';
import { btn } from './manageStyles';
import type { ProbeFailure } from './StepAccess';

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
  // WHICH failure probeError describes. Kept alongside the message rather than
  // inferred from it: the access step's card says something different for "the
  // host refused what you supplied" than for "the check never got there", and
  // sniffing that out of an error string would break the moment the wording
  // changed. '' means the last check did not fail.
  const [probeFailure, setProbeFailure] = useState<ProbeFailure>('');
  // Bumped every time a check the USER asked for comes back clean. The outcome
  // card is already green in that state, so pressing "Check access" again
  // changed nothing on screen and read as a dead button. The counter gives the
  // step something to acknowledge the click with, without inventing a state
  // the probe did not report.
  const [checkedOk, setCheckedOk] = useState(0);
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
  // The branch step's own in-flight check, tracked separately from `probing`
  // because it is a different request with a different cancel button, and the
  // two steps can never be on screen at once anyway.
  const [checkingBranch, setCheckingBranch] = useState(false);
  const branchAbort = useRef<AbortController | null>(null);

  const step = currentStep(state);

  // Navigating retires the last attempt's report.
  //
  // createErr and the event log describe ONE press of Create. They were
  // rendered by the shell, unconditionally, so going Back carried the failure
  // onto every other step — an access step reading "✓ Access confirmed"
  // directly above "you are not allowed to push" is the wizard contradicting
  // itself. And a reader who goes back is going back to CHANGE something, so
  // by the time they return the report may describe a request they no longer
  // intend to make.
  //
  // Wrapping dispatch rather than clearing from an effect keeps this on the
  // one path that can cause it — the footer's Back and the rail's GOTO are the
  // only actions that move between steps — and avoids a setState during
  // render. Step components get the plain dispatch: they only set fields.
  const navigate = (a: WizardAction) => {
    // probeError/probeFailure are deliberately NOT cleared: they belong to the
    // access step's own check, which is still true when you navigate back to it.
    setCreateErr('');
    setEvents([]);
    dispatch(a);
  };

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
    setProbeError(''); setProbeFailure(''); setProbing(true);
    try {
      const probe = await api.probeOrigin(
        { url: state.url, branch: state.branch || undefined, ...authFor(state) }, ctl.signal);
      dispatch({ type: 'PROBE_DONE', probe });
      // A probe that came back but couldn't reach the remote is not an
      // exception — it's a normal 200 with reachable: false — so the error
      // is read off the result, not a catch block.
      if (!probe.reachable) {
        setProbeError(probe.detail || 'Could not reach that remote.');
        setProbeFailure('unreachable');
      }
      return probe;
    } catch (e) {
      // A cancel the user asked for is not a failure to report back to them.
      if (ctl.signal.aborted) return null;
      setProbeError(e instanceof Error ? e.message : String(e));
      setProbeFailure('unreachable');
      return null;
    } finally {
      setProbing(false);
    }
  };

  const cancelProbe = () => probeAbort.current?.abort();

  // runInitializedCheck asks the ONE question that decides the rest of the
  // wizard: does the chosen branch already carry a knomit ontology?
  //
  // It carries the wizard's credentials for the same reason runProbe does — an
  // answer obtained anonymously is an answer about a different request — and it
  // reports its outcome through the reducer, including the failure, because a
  // check that did not complete is a STATE the branch step renders rather than
  // an error message floating beside a step that has already moved on.
  //
  // A thrown request and a 200 that could not establish anything are the same
  // outcome here, so the catch dispatches the same action with a detail rather
  // than leaving `initialized` untouched at a value from a previous attempt.
  const runInitializedCheck = async (): Promise<'yes' | 'no' | '' | null> => {
    branchAbort.current?.abort();
    const ctl = new AbortController();
    branchAbort.current = ctl;
    setCheckingBranch(true);
    try {
      const result = await api.probeInitialized(
        { url: state.url, branch: state.branch || undefined, ...authFor(state) }, ctl.signal);
      dispatch({ type: 'INITIALIZED_DONE', result });
      return result.initialized ?? '';
    } catch (e) {
      // A cancel the user asked for is not a failure to report back to them,
      // and must not overwrite whatever the last completed check established.
      if (ctl.signal.aborted) return null;
      dispatch({
        type: 'INITIALIZED_DONE',
        result: { detail: e instanceof Error ? e.message : String(e) },
      });
      return '';
    } finally {
      setCheckingBranch(false);
    }
  };

  const cancelBranchCheck = () => branchAbort.current?.abort();

  // Advancing off the branch step RUNS the check first, then steps into
  // whichever step the new list has next — which is 'ontology' for a branch
  // that is not yet a knowledge base and 'review' for one that is.
  //
  // Nothing advances on an unestablished answer: stepsFor ends the list at
  // 'branch' while `initialized` is '', so NEXT would be a no-op anyway. The
  // explicit guard is here so the intent survives a future change to that list.
  const handleBranchNext = async () => {
    const answer = await runInitializedCheck();
    if (answer !== 'yes' && answer !== 'no') return;
    dispatch({ type: 'NEXT' });
  };

  // The source step's Connect. auth_required is NOT an error here — it is the
  // next question, and StepAccess asks it.
  const handleProbe = () => { void runProbe(); };

  // The access step's re-probe. Same call, different reading of the same
  // answer: the user has now supplied credentials, so a remote that still
  // reports auth_required has REFUSED them, and that is a failure worth
  // saying out loud.
  const reprobeWithCredentials = async (): Promise<ProbeResult | null> => {
    // What was actually SENT decides the wording. Saying "those credentials did
    // not give access" when authFor sent none is a claim about something that
    // never happened — and it is what the user saw after typing a token against
    // an SSH URL, where the token is dropped and knomit's own key is used
    // instead. The check still failed; it just failed at something else.
    const sent = authFor(state);
    const suppliedSomething = sent.auth_token !== '';
    const probe = await runProbe();
    if (probe?.reachable && !probe.auth_required) setCheckedOk(n => n + 1);
    if (probe?.reachable && probe.auth_required) {
      const lead = suppliedSomething
        ? 'Those credentials did not give access to this remote'
        : "knomit's own key did not give access to this remote, and no credential was supplied to try instead";
      setProbeError(probe.detail ? `${lead}: ${probe.detail}` : `${lead}.`);
      setProbeFailure(suppliedSomething ? 'refused' : 'no-credential');
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
    // Re-probe when the stored answer is no longer about this request — a
    // changed URL or credential — as well as when it never established the
    // shape (auth_required). `empty` and `branches` DERIVE the step list, and
    // routing on values obtained with a different credential is exactly how a
    // private empty remote was once classified non-empty and created as a
    // clone, silently discarding the ontology the user picked.
    if (probeIsCurrent(state) && !state.probe?.auth_required) { dispatch({ type: 'NEXT' }); return; }
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
  // The generic "Next" footer button covers every step that has nothing more
  // specific to say about advancing: 'access', 'ontology' (gated on
  // ontologyValid — StepOntology's own verification round trip, not this
  // button, decides when the chosen ontology is safe to carry forward), and the
  // source step's LOCAL pane, which now holds the name field that used to be a
  // step of its own.
  //
  // The source step's REMOTE pane advances on a successful probe instead, via
  // its own Connect button — there is nothing for a Next to do there that
  // Connect does not already do, and a second forward control would just be a
  // way to skip the check. 'branch' is the same shape: its Continue RUNS the
  // initialization check and advances on the answer, so a generic Next beside
  // it would be a way to reach the ontology step without ever establishing
  // whether one is needed. 'review' submits instead of advancing.
  //
  // And never on a step the derived list ENDS at. stepsFor truncates rather
  // than disabling — a remote with no branches stops at 'access', an
  // unestablished initialization check stops at 'branch' — and NEXT clamps to
  // the index it is already on, so a Next offered there is enabled, does
  // nothing when pressed, and says nothing about why. That reached a human: an
  // empty GitLab project, a blue Next, no response to three presses.
  //
  // Asking the list ("is there a step after this one?") rather than naming the
  // cases keeps the rule true for whatever truncation is added next. The step
  // itself states the reason it stopped — the access card asks for a `main`,
  // the branch card offers a retry — which is why the button is absent rather
  // than disabled: a disabled control with no stated cause is the failure this
  // wizard already fixed once, on the name field.
  const nothingFollows = stepsFor(state).at(-1) === step;
  const showGenericNext = !nothingFollows &&
    (step === 'access' || step === 'ontology' ||
      (step === 'source' && state.choice === 'local'));
  const nextDisabled = creating || probing || checkingBranch || !nameOk ||
    (step === 'access' && basicMissingUser) ||
    (step === 'ontology' && !ontologyValid);

  return (
    <div>
      <h3 style={heading}>New repository</h3>
      <WizardStepRail state={state} dispatch={navigate} />

      {step === 'source' && (
        <StepSource state={state} dispatch={dispatch} onProbe={handleProbe} onCancelProbe={cancelProbe}
          probing={probing} probeError={probeError} />
      )}
      {step === 'access' && (
        <StepAccess state={state} dispatch={dispatch} onCancelProbe={cancelProbe}
          onProbe={() => { void reprobeWithCredentials(); }} probing={probing}
          probeError={probeError} probeFailure={probeFailure} checkedOk={checkedOk} />
      )}
      {step === 'branch' && <StepBranch state={state} dispatch={dispatch} />}
      {step === 'ontology' && <StepOntology state={state} onDispatch={dispatch} onValidityChange={setOntologyValid} />}
      {step === 'review' && <StepReview state={state} />}

      {/* A create that failed left NOTHING in the repo list: every failure path
          in Manager.Create calls cleanup(), which drops the local .db and the
          registry row. Saying so is worth more than the error alone — the
          reader's first question is whether they now have a half-made
          repository. It does NOT claim the remote is untouched: a seed that
          failed after its push has already written there, and the server's own
          message explains that case.

          Amber, not red. Red is for data that is gone; a create that rolled
          back has lost nothing of the reader's. */}
      {/* Scoped to the step that produced them, as well as cleared on
          navigation: two guards, because a report bleeding onto another step
          is the kind of thing a later refactor reintroduces by accident. */}
      {step === 'review' && createErr && (
        <div data-testid="create-error" style={errText}>
          <div>{createErr}</div>
          <div style={errNote}>No repository was added. You can change something and try again.</div>
        </div>
      )}
      {step === 'review' && <CreateProgress events={events} />}

      <div style={footer}>
        {step !== 'source' && (
          <button type="button" style={btn(creating || checkingBranch)} disabled={creating || checkingBranch}
            onClick={() => navigate({ type: 'BACK' })}>
            Back
          </button>
        )}
        {showGenericNext && (
          <button type="button" style={btn(nextDisabled, 'primary')} disabled={nextDisabled}
            onClick={step === 'access' ? () => { void handleAccessNext(); } : () => dispatch({ type: 'NEXT' })}>
            Next
          </button>
        )}
        {/* The branch step's forward action. It is NOT the generic Next — it
            RUNS the initialization check and advances on the answer, which is
            why showGenericNext excludes this step — but it belongs in the same
            place as every other step's, which is what the footer is for. It
            used to sit in the step body, stacked directly above Back and
            Cancel, so the one screen with two rows of buttons was also the one
            whose primary action was somewhere new.

            It says what pressing it does NOW: "Try again" once a check has run
            and could not tell. */}
        {step === 'branch' && (
          <button type="button" data-testid="branch-check-button" style={btn(checkingBranch || !nameOk, 'primary')}
            disabled={checkingBranch || !nameOk} onClick={() => { void handleBranchNext(); }}>
            {checkingBranch ? 'Checking…' : branchCheckBlocked(state) ? 'Try again' : 'Continue'}
          </button>
        )}
        {/* This check CLONES — shallow and single-branch, but a transfer all
            the same — so unlike the ref listing on earlier steps it can take
            real time on a large repository. A visible stop is the difference
            between a slow step and a frozen one.

            "Stop checking", not "Cancel check": it stands next to Cancel, which
            abandons the whole wizard, and two adjacent buttons whose labels
            both start with the same verb make the reader work out which one
            throws away their work. */}
        {step === 'branch' && checkingBranch && (
          <button type="button" data-testid="branch-cancel-button" style={btn(false)} onClick={cancelBranchCheck}>
            Stop checking
          </button>
        )}
        {step === 'review' && (
          <button type="button" style={btn(creating || !nameOk, 'primary')} disabled={creating || !nameOk} onClick={handleCreate}>
            {/* "Create repository" after a failure invites the same press
                again; the label should say what pressing it does NOW. */}
            {creating ? 'Creating…' : createErr ? 'Try again' : 'Create repository'}
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
const errText: React.CSSProperties = {
  marginTop: 12, padding: '10px 12px', borderRadius: 6,
  background: '#262013', border: '1px solid #4a3f22',
  color: '#e2c07a', fontSize: 13, lineHeight: 1.55, maxWidth: '74ch',
  // The server's own words arrive with their line structure intact — hosts
  // wrap prose and format remedies as lists, and collapsing that would undo
  // the reason for carrying it back at all.
  whiteSpace: 'pre-wrap',
};
const errNote: React.CSSProperties = { color: '#a08a54', fontSize: 12, marginTop: 6 };
