import type { CreateRepoBody, ProbeResult } from './api';

export type StepId = 'source' | 'access' | 'name' | 'ontology' | 'review';
export type SourceChoice = 'remote' | 'local' | null;

export interface WizardState {
  choice: SourceChoice;
  url: string;
  probe: ProbeResult | null;
  name: string;
  branch: string;
  authMethod: string;
  authUser: string;
  authToken: string;
  /** 'default' | 'code' when a preset is selected, '' when custom yaml is used. */
  preset: string;
  yaml: string;
  stepIndex: number;
}

export const initialWizardState: WizardState = {
  choice: null, url: '', probe: null, name: '', branch: '',
  authMethod: '', authUser: '', authToken: '',
  preset: 'default', yaml: '', stepIndex: 0,
};

export type WizardAction =
  | { type: 'CHOOSE_REMOTE' }
  | { type: 'CHOOSE_LOCAL' }
  | { type: 'SET_URL'; url: string }
  | { type: 'PROBE_DONE'; probe: ProbeResult }
  | { type: 'SET_NAME'; name: string }
  | { type: 'SET_BRANCH'; branch: string }
  | { type: 'SET_AUTH_METHOD'; method: string }
  | { type: 'SET_AUTH_USER'; user: string }
  | { type: 'SET_TOKEN'; token: string }
  | { type: 'SET_PRESET'; preset: string }
  | { type: 'SET_YAML'; yaml: string }
  | { type: 'NEXT' }
  | { type: 'BACK' }
  | { type: 'GOTO'; step: StepId };

// wizardReducer applies the action, then clamps stepIndex into range for the
// RESULT's own stepsFor. This is belt-and-braces with currentStep() below:
// any action here — not just the two (CHOOSE_LOCAL, PROBE_DONE) that R2 gives
// an explicit advance rule — can change `choice`/`probe` and therefore shrink
// the derived list out from under a stepIndex that used to be valid. E.g.
// probe an empty remote (list grows to 4, land on 'access' at index 1), then
// correct a typo'd URL (SET_URL resets probe → list collapses to ['source']);
// without this clamp stepIndex would still read 1, one past the end.
export function wizardReducer(s: WizardState, a: WizardAction): WizardState {
  return clampStepIndex(applyAction(s, a));
}

function clampStepIndex(s: WizardState): WizardState {
  const max = Math.max(stepsFor(s).length - 1, 0);
  return s.stepIndex > max ? { ...s, stepIndex: max } : s;
}

function applyAction(s: WizardState, a: WizardAction): WizardState {
  switch (a.type) {
    case 'CHOOSE_REMOTE': return { ...s, choice: 'remote' };
    // Advances to the name step unconditionally — there is no probe to wait on
    // for a local-only repo, so choosing this branch is itself enough to move on.
    case 'CHOOSE_LOCAL':  return { ...s, choice: 'local', probe: null, stepIndex: 1 };
    case 'SET_URL':       return { ...s, choice: 'remote', url: a.url, probe: null };
    // Advances to the access step ONLY when the probe succeeded. An unreachable
    // probe leaves stepIndex where it is so the error renders in place on the
    // source step instead of the wizard silently moving on past it.
    case 'PROBE_DONE':    return {
      ...s, choice: 'remote', probe: a.probe, branch: s.branch || a.probe.upstream_branch,
      stepIndex: a.probe.reachable ? 1 : s.stepIndex,
    };
    case 'SET_NAME':      return { ...s, name: a.name };
    case 'SET_BRANCH':    return { ...s, branch: a.branch };
    case 'SET_AUTH_METHOD': return { ...s, authMethod: a.method };
    case 'SET_AUTH_USER': return { ...s, authUser: a.user };
    case 'SET_TOKEN':     return { ...s, authToken: a.token };
    // Preset and yaml are mutually exclusive: selecting one clears the other so
    // createBodyFor never has to guess which the user meant.
    case 'SET_PRESET':    return { ...s, preset: a.preset, yaml: '' };
    case 'SET_YAML':      return { ...s, yaml: a.yaml, preset: '' };
    case 'NEXT':          return { ...s, stepIndex: Math.min(s.stepIndex + 1, stepsFor(s).length - 1) };
    case 'BACK':          return { ...s, stepIndex: Math.max(s.stepIndex - 1, 0) };
    case 'GOTO': {
      const i = stepsFor(s).indexOf(a.step);
      return i < 0 ? s : { ...s, stepIndex: i };
    }
    default: return s;
  }
}

// currentStep is the read-time counterpart of the clamp above: it re-derives
// stepsFor(s) and clamps the index there too, so a consumer (Task 8) that
// indexes via this selector is safe even if some future action forgets to
// keep stepIndex in range. Never index `stepsFor(s)[s.stepIndex]` directly.
export function currentStep(s: WizardState): StepId {
  const steps = stepsFor(s);
  const i = Math.min(Math.max(s.stepIndex, 0), steps.length - 1);
  return steps[i];
}

/**
 * stepsFor DERIVES the step list from the probe result rather than storing it.
 *
 * A step is not "skipped" — it is simply not in the list for this case. Joining
 * a remote that already has content never asks about the ontology, because that
 * ontology comes from the remote (rejectOntologySpecForClone refuses one).
 */
export function stepsFor(s: WizardState): StepId[] {
  if (s.choice === 'local') return ['source', 'name', 'ontology', 'review'];
  // No probe yet, or the probe came back unreachable: there is no confirmed
  // remote to build the rest of the wizard around, so the list stays at
  // ['source'] — never advertise Access/Review chrome for a remote we could
  // not reach. auth_required alone does NOT disqualify a probe: it is still
  // reachable (the server saw the remote and reported it needs credentials),
  // so that case keeps its access step below.
  if (!s.probe || !s.probe.reachable) return ['source'];
  if (s.probe.empty) return ['source', 'access', 'ontology', 'review'];
  return ['source', 'access', 'review'];
}

/**
 * createBodyFor maps wizard state onto the create wire format.
 *
 * The mode is derived from the PROBE, never chosen by the user:
 *   local            → 'custom' when yaml was supplied, else 'preset'
 *   remote, empty    → 'seed'   (carries the chosen ontology)
 *   remote, has refs → 'clone'  (carries NO ontology — the backend refuses one)
 */
export function createBodyFor(s: WizardState): CreateRepoBody {
  if (s.choice === 'local') {
    return s.yaml
      ? { name: s.name, mode: 'custom', ontology_yaml: s.yaml }
      : { name: s.name, mode: 'preset', ontology_preset: s.preset || 'default' };
  }
  const origin = originFor(s);
  if (s.probe?.empty) {
    return s.yaml
      ? { name: s.name, mode: 'seed', ontology_yaml: s.yaml, origin }
      : { name: s.name, mode: 'seed', ontology_preset: s.preset || 'default', origin };
  }
  return { name: s.name, mode: 'clone', origin };
}

function originFor(s: WizardState): NonNullable<CreateRepoBody['origin']> {
  // Auto-detect ('') resolves to anonymous/SSH and ignores any token. A token
  // typed under auto-detect (the common private-HTTPS case) promotes to
  // explicit token auth so the credential is actually used — carried over from
  // the old CreateRepoForm, where dropping it silently broke private clones.
  const method = s.authMethod === '' && s.authToken.trim() !== '' ? 'token' : s.authMethod;
  // Assemble the token each method consumes, so a credential typed under
  // auto-detect and then abandoned (method switched to none/ssh) is neither
  // sent nor persisted. Basic carries "user:password" (matching the backend's
  // assembleAuthToken convention); token carries the raw secret.
  const token =
    method === 'token' ? s.authToken :
    method === 'basic' ? (s.authUser !== '' ? `${s.authUser}:${s.authToken}` : s.authToken) :
    '';
  return { url: s.url, branch: s.branch, auth_method: method, auth_token: token };
}
