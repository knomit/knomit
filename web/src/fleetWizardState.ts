import type { FleetJoinCheck, ProbeResult } from './api';
import { hostOf, transportFor } from './wizardState';

// The New fleet wizard's state: Create (Start · Hosting · [Access] · Review)
// or Join (Start · Bundle · [Access] · Review). As in wizardState.ts, the
// step list is DERIVED from what the state has established (stepsFor), never
// fixed: the rail lists only steps that exist, and a step the state has not
// earned is not offered.

export type FleetStepId = 'start' | 'hosting' | 'access' | 'bundle' | 'review';
export type FleetChoice = 'create' | 'join';
export type FleetHosting = 'here' | 'forge';
export type FleetAuthMethod = '' | 'none' | 'token' | 'basic' | 'ssh';

export interface FleetWizardState {
  step: FleetStepId;
  choice: FleetChoice;
  // Create.
  hosting: FleetHosting;
  forgeUrl: string;
  /** The forge's probe (POST /repos:probe-origin), for forgeUrl as it was. */
  probe: ProbeResult | null;
  // Join.
  /** The bundle as loaded: sent with join:check and join, never rendered. */
  bundle: string;
  /** Where it came from: 'clipboard' or a file name. */
  bundleSource: string;
  /** join:check's answer for the bundle (and credentials) as they were. */
  check: FleetJoinCheck | null;
  // Access, on a forge (create's master credential, or the joiner's own).
  authMethod: FleetAuthMethod;
  authUser: string;
  authToken: string;
  /** A check WITH the credential above got in. Cleared when it changes. */
  accessChecked: boolean;
}

export const initialFleetWizardState: FleetWizardState = {
  step: 'start', choice: 'create', hosting: 'here', forgeUrl: '', probe: null,
  bundle: '', bundleSource: '', check: null,
  authMethod: '', authUser: '', authToken: '', accessChecked: false,
};

export type FleetWizardAction =
  | { type: 'CHOOSE'; choice: FleetChoice }
  | { type: 'SET_HOSTING'; hosting: FleetHosting }
  | { type: 'SET_FORGE_URL'; url: string }
  | { type: 'PROBED'; probe: ProbeResult }
  | { type: 'BUNDLE_LOADED'; bundle: string; source: string }
  | { type: 'CHECKED'; check: FleetJoinCheck }
  | { type: 'SET_AUTH_METHOD'; method: FleetAuthMethod }
  | { type: 'SET_AUTH_USER'; user: string }
  | { type: 'SET_TOKEN'; token: string }
  | { type: 'ACCESS_CHECKED' }
  | { type: 'GOTO'; step: FleetStepId }
  | { type: 'NEXT' }
  | { type: 'BACK' };

/** Every write is a pure field write except where a field invalidates what
 *  was established from it: a new forge URL drops its probe, a new bundle its
 *  check, a new credential the access confirmation. */
export function fleetWizardReducer(s: FleetWizardState, a: FleetWizardAction): FleetWizardState {
  const clamp = (n: FleetWizardState): FleetWizardState => {
    const steps = stepsFor(n);
    return steps.includes(n.step) ? n : { ...n, step: steps[steps.length - 1] };
  };
  switch (a.type) {
    case 'CHOOSE':
      return s.choice === a.choice ? s : { ...s, choice: a.choice, accessChecked: false };
    case 'SET_HOSTING':
      return s.hosting === a.hosting ? s : { ...s, hosting: a.hosting };
    case 'SET_FORGE_URL':
      return clamp({ ...s, forgeUrl: a.url, probe: null, accessChecked: false });
    case 'PROBED':
      // A forge that answered: Access comes next, where its credential is asked.
      return { ...s, probe: a.probe, step: 'access', accessChecked: false };
    case 'BUNDLE_LOADED':
      return clamp({ ...s, bundle: a.bundle, bundleSource: a.source, check: null, accessChecked: false });
    case 'CHECKED':
      // A checked bundle: a forge's credential next, else Review.
      return { ...s, check: a.check, step: a.check.hosting === 'forge' ? 'access' : 'review', accessChecked: false };
    case 'SET_AUTH_METHOD':
      return { ...s, authMethod: a.method, accessChecked: false };
    case 'SET_AUTH_USER':
      return { ...s, authUser: a.user, accessChecked: false };
    case 'SET_TOKEN':
      return { ...s, authToken: a.token, accessChecked: false };
    case 'ACCESS_CHECKED':
      return { ...s, accessChecked: true };
    case 'GOTO':
      return stepsFor(s).includes(a.step) ? { ...s, step: a.step } : s;
    case 'NEXT': {
      const steps = stepsFor(s);
      const i = steps.indexOf(s.step);
      return i >= 0 && i < steps.length - 1 ? { ...s, step: steps[i + 1] } : s;
    }
    case 'BACK': {
      const steps = stepsFor(s);
      const i = steps.indexOf(s.step);
      return i > 0 ? { ...s, step: steps[i - 1] } : s;
    }
  }
}

/**
 * stepsFor: the steps the state has established, in order.
 *   create, this instance hosts it: Start · Hosting · Review
 *   create, a forge not probed yet: Start · Hosting
 *   create, a forge that answered:  Start · Hosting · Access · Review
 *   join, no checked bundle:        Start · Bundle
 *   join, a fleet its master hosts: Start · Bundle · Review
 *   join, a fleet on a forge:       Start · Bundle · Access · Review
 */
export function stepsFor(s: FleetWizardState): FleetStepId[] {
  if (s.choice === 'create') {
    if (s.hosting === 'here') return ['start', 'hosting', 'review'];
    return s.probe ? ['start', 'hosting', 'access', 'review'] : ['start', 'hosting'];
  }
  if (!s.check) return ['start', 'bundle'];
  return s.check.hosting === 'forge' ? ['start', 'bundle', 'access', 'review'] : ['start', 'bundle', 'review'];
}

export const FLEET_STEP_LABEL: Record<FleetStepId, string> = {
  start: 'Start', hosting: 'Hosting', access: 'Access', bundle: 'Bundle', review: 'Review',
};

/** What a done step settled, for the rail's mono summary. */
export function stepSummary(s: FleetWizardState, id: FleetStepId): string {
  switch (id) {
    case 'start': return s.choice;
    case 'hosting': return s.hosting === 'here' ? 'this instance' : hostOf(s.forgeUrl) || 'forge';
    case 'access': return s.authMethod || 'auto-detect';
    case 'bundle': return s.check ? s.check.host : 'loaded';
    case 'review': return '';
  }
}

/** The forge the Access step is about: the create's forge URL, or the URL
 *  the checked bundle names. */
export function accessURL(s: FleetWizardState): string {
  return s.choice === 'create' ? s.forgeUrl.trim() : s.check?.url ?? '';
}

/** Whether the forge asked for a credential the user has not yet got in with. */
export function accessRequired(s: FleetWizardState): boolean {
  const asked = s.choice === 'create' ? !!s.probe?.auth_required : !!s.check?.auth_required;
  return asked && !s.accessChecked;
}

/** The credential as the fleet routes take it (auth_method, auth_token), as
 *  the repo wizard's authFor assembles it: a token typed under auto-detect is
 *  token auth (not on an SSH URL), basic carries "user:password". */
export function fleetAuth(s: FleetWizardState): { auth_method?: string; auth_token?: string } {
  return credentialFor(accessURL(s), s.authMethod, s.authUser, s.authToken);
}

/** credentialFor is fleetAuth's rule for any URL and fields: the Fleet
 *  Settings' Point at / Update access / Move flows send the same shape. */
export function credentialFor(url: string, authMethod: FleetAuthMethod, user: string, secret: string): { auth_method?: string; auth_token?: string } {
  const method = authMethod === '' && secret.trim() !== '' && transportFor(url) !== 'ssh' ? 'token' : authMethod;
  const token = method === 'basic' ? `${user}:${secret}` : method === 'token' ? secret : '';
  return { ...(method ? { auth_method: method } : {}), ...(token ? { auth_token: token } : {}) };
}
