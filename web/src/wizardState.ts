import type { CreateRepoBody, InitializedResult, ProbeResult } from './api';

export type StepId = 'source' | 'access' | 'branch' | 'ontology' | 'review';
// There is no "not chosen yet": the source step is a segmented control, and a
// segmented control always has a selection. `choice` therefore starts at
// 'remote' rather than null — see initialWizardState.
export type SourceChoice = 'remote' | 'local';

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
  /**
   * The preset CARD the user last clicked. Never cleared — this is not the wire
   * value (`preset` is), it is the answer to "which preset did they choose?",
   * which `preset` stops being able to give the moment SET_YAML clears it.
   *
   * It lives in the reducer rather than in StepOntology because the ontology
   * step is conditionally rendered (CreateRepoWizard.tsx): Back-then-Next
   * unmounts and remounts it, and a component-local ref re-initialises from
   * `state.preset` — which is '' by then — and silently degrades to 'default'.
   * A user who picked "code" would get the default ontology, permanently,
   * because the ontology is immutable after repo creation.
   */
  seedPreset: string;
  yaml: string;
  /**
   * The per-branch answer to "is `branch` on this remote already a knomit
   * knowledge base?", from api.probeInitialized.
   *
   * THREE STATES. '' is not "no" — it is "we did not establish it", which is
   * where this starts, where SET_BRANCH returns it, and where a failed check
   * leaves it. stepsFor BLOCKS on '' rather than routing, because both ways of
   * guessing are unrecoverable: a repo's ontology is fixed at create time, so
   * guessing 'yes' permanently discards the ontology the user chose and
   * guessing 'no' permanently writes one over a knowledge base that already had
   * its own.
   */
  initialized: '' | 'yes' | 'no';
  /** Why `initialized` is '' — the server's own words. Shown on the branch step. */
  initializedDetail: string;
  /**
   * The branch the server actually INSPECTED, which is not always the one the
   * user picked.
   *
   * A create reads whatever InitFromRemote adopts: this machine's agent branch
   * when the remote already carries one, otherwise the chosen consensus branch
   * (internal/repos/initialized.go mirrors that rule). So a remote this machine
   * initialized earlier answers 'yes' about `agent/<host>` while `main` — the
   * branch on screen — has no ontology and never will.
   *
   * Carried so the step can NAME the branch it is talking about. Dropping it
   * produced a card that said "main already holds a knowledge base" about the
   * one branch that provably does not.
   */
  initializedBranch: string;
  /**
   * Fingerprints of the REQUESTS the two answers above were made with.
   *
   * An answer is about one remote, reached with one credential, about one
   * branch. Change any of those and the stored answer is about a request nobody
   * made. Invalidation used to be a list of reset paths that each action had to
   * remember to spread — and it leaked in four places at once: `probe` survived
   * a credential change, the green "access confirmed" line was never reset at
   * all, and `branch` and `name` survived a change of remote.
   *
   * Carrying the question with the answer inverts that: nothing has to remember
   * to forget. The selectors below compare and simply decline to speak for an
   * answer whose request no longer matches.
   */
  probeKey: string;
  initializedKey: string;
  stepIndex: number;
}

// choice starts at 'remote', which is a soft lead for the remote path and is
// acknowledged as one. It is the same lead StepSource already gave by ordering
// and by carrying the fuller description — not a badge, which is the thing that
// would make the other option read as wrong.
export const initialWizardState: WizardState = {
  choice: 'remote', url: '', probe: null, name: '', branch: '',
  authMethod: '', authUser: '', authToken: '',
  preset: 'default', seedPreset: 'default', yaml: '',
  initialized: '', initializedDetail: '', initializedBranch: '',
  probeKey: '', initializedKey: '', stepIndex: 0,
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
  | { type: 'INITIALIZED_DONE'; result: InitializedResult }
  | { type: 'NEXT' }
  | { type: 'BACK' }
  | { type: 'GOTO'; step: StepId };

// wizardReducer applies the action, then clamps stepIndex into range for the
// RESULT's own stepsFor. This is belt-and-braces with currentStep() below:
// any action here — not just PROBE_DONE, the one action left with an explicit
// advance rule — can change `choice`/`probe` and therefore shrink the derived
// list out from under a stepIndex that used to be valid. E.g.
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

// uncheckedBranch returns the branch answer to ITS UNKNOWN STATE, and every
// action that could invalidate it spreads this rather than writing the two
// fields by hand.
//
// It is a reset to '' — never to 'no'. The answer is about one branch of one
// remote reached with one credential, so changing any of those three unmakes
// it, and a stale 'yes'/'no' carried across such a change is an answer to a
// question nobody asked. '' is also the only safe direction to fail: stepsFor
// blocks there, where 'no' would silently route to `initialize` and write an
// ontology over whatever is actually on the new branch.
const uncheckedBranch = { initialized: '' as const, initializedDetail: '', initializedBranch: '' };

function applyAction(s: WizardState, a: WizardAction): WizardState {
  switch (a.type) {
    // Neither choice ADVANCES. Both are the segmented control on the source
    // step, and a segment that navigates is not a segment — it discloses the
    // fields its branch needs (local's name, remote's URL) in place, and the
    // footer's Next moves on. CHOOSE_LOCAL used to jump to stepIndex 1 because
    // the name lived on its own step; folding that step into the source step is
    // what this control replaces.
    case 'CHOOSE_REMOTE': return { ...s, choice: 'remote' };
    case 'CHOOSE_LOCAL':  return { ...s, choice: 'local', probe: null, ...uncheckedBranch };
    // The branch goes with the remote. A branch chosen on the PREVIOUS remote
    // decides which ref the next check inspects and which the create clones, so
    // carrying it across a change of URL creates against something nobody
    // picked — and it is invisible, because the chip for it may not even exist
    // on the new remote. The NAME is deliberately kept: it is a label, the
    // reader may have typed it, and PROBE_DONE only ever fills it when empty.
    case 'SET_URL':       return { ...s, choice: 'remote', url: a.url, probe: null, branch: '', ...uncheckedBranch };
    // Advances to the access step ONLY when the probe succeeded. An unreachable
    // probe leaves stepIndex where it is so the error renders in place on the
    // source step instead of the wizard silently moving on past it.
    // `s.name || …` never overwrites a name the user typed: the prefill is a
    // default offered once, not a value the wizard keeps re-asserting every
    // time the remote is re-probed.
    //
    // A FAILED re-check never replaces a probe that succeeded. stepsFor
    // collapses an unreachable probe to ['source'], and clampStepIndex then
    // drags the user off whichever step they were on — so before this guard,
    // pressing "Check access" and getting any unclassified error threw the
    // wizard back to the first screen and took the access step (the only place
    // a credential can be entered) with it. That is the same dead end
    // internal/repos/probe.go documents for the unresolvable-credential case,
    // arriving from the client side instead.
    //
    // The failure is still reported — CreateRepoWizard surfaces it as
    // probeError on the step the user is on — it just no longer un-establishes
    // a remote that was already reached. A first probe has no earlier result to
    // keep, so an unreachable one still holds the wizard on 'source'.
    case 'PROBE_DONE': {
      const keepsShape = !a.probe.reachable && !!s.probe?.reachable;
      const probe = keepsShape ? s.probe : a.probe;
      return {
        ...s, choice: 'remote', probe, branch: s.branch || a.probe.upstream_branch,
        name: s.name || repoNameFromURL(s.url),
        ...uncheckedBranch,
        probeKey: remoteKey(s),
        stepIndex: a.probe.reachable ? 1 : s.stepIndex,
      };
    }
    case 'SET_NAME':      return { ...s, name: a.name };
    // Changing the branch un-answers the question, because the answer was
    // about the OTHER branch. A repo can carry .knomit/ontology.yaml on main
    // and not on develop — that asymmetry is the entire reason this is a
    // per-branch check and its own step.
    case 'SET_BRANCH':    return { ...s, branch: a.branch, ...uncheckedBranch };
    // So does changing the credential: the check runs with whatever is set
    // here, and an answer obtained with a different one is an answer about a
    // different request — the same rule authFor exists to enforce for the
    // probe and the create.
    case 'SET_AUTH_METHOD': return { ...s, authMethod: a.method, ...uncheckedBranch };
    case 'SET_AUTH_USER': return { ...s, authUser: a.user, ...uncheckedBranch };
    case 'SET_TOKEN':     return { ...s, authToken: a.token, ...uncheckedBranch };
    // The ONLY action that establishes it. An absent `initialized` is the
    // unestablished state and is stored as '' with the reason alongside —
    // never coerced into 'no', which stepsFor would route straight to
    // `initialize`.
    case 'INITIALIZED_DONE': {
      const v = a.result.initialized;
      return v === 'yes' || v === 'no'
        ? { ...s, initialized: v, initializedDetail: '', initializedBranch: a.result.branch || '', initializedKey: branchKey(s) }
        : { ...s, initialized: '', initializedDetail: a.result.detail || '', initializedBranch: '', initializedKey: '' };
    }
    // Preset and yaml are mutually exclusive: selecting one clears the other so
    // createBodyFor never has to guess which the user meant. seedPreset is the
    // deliberate exception — it records the CHOICE, not the payload, and so
    // survives SET_YAML (see its doc on WizardState).
    case 'SET_PRESET':    return { ...s, preset: a.preset, seedPreset: a.preset, yaml: '' };
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
 * remoteKey fingerprints "which remote, reached how" — the question every
 * origin probe answers. authFor is reused rather than the raw fields so the
 * fingerprint changes exactly when what is SENT changes: a token typed under
 * auto-detect against an SSH URL is dropped before it is sent, and a change
 * that alters nothing on the wire must not invalidate a good answer.
 */
function remoteKey(s: WizardState): string {
  const { auth_method, auth_token } = authFor(s);
  return [s.url.trim(), auth_method, auth_token].join('\u0000');
}

/** branchKey adds the branch: the initialization answer is per-branch. */
function branchKey(s: WizardState): string {
  return [remoteKey(s), s.branch].join('\u0000');
}

/**
 * probeIsCurrent reports whether the stored probe still answers the request the
 * wizard would make now.
 *
 * A stale probe is NOT discarded — throwing it away collapses the step list and
 * rewinds the reader to the first screen, taking the access step (the only
 * place a credential can be typed) with it. It simply stops being quotable: the
 * access step shows no confirmation and no push-access verdict for it, and
 * advancing re-probes first.
 */
export function probeIsCurrent(s: WizardState): boolean {
  return s.probe !== null && s.probeKey === remoteKey(s);
}

/**
 * establishedAnswer is the initialization answer, or '' when the question has
 * moved on since it was given.
 *
 * Every consumer routes on THIS, never on `initialized` directly. '' is the
 * unestablished state, which blocks — the safe direction, since guessing either
 * way is unrecoverable once the repo exists.
 */
export function establishedAnswer(s: WizardState): '' | 'yes' | 'no' {
  return s.initializedKey === branchKey(s) ? s.initialized : '';
}

/**
 * stepsFor DERIVES the step list from what has been established rather than
 * storing it.
 *
 * A step is not "skipped" — it is simply not in the list for this case. Joining
 * a branch that is already a knowledge base never asks about the ontology,
 * because that ontology comes from the remote (rejectOntologySpecForClone
 * refuses one).
 *
 * The list ENDS at 'branch' while `initialized` is '', and that truncation is
 * the blocking mechanism: with no step after it there is nowhere to advance to,
 * so a check that failed cannot be walked past. It is deliberately not a
 * disabled Next on a full-length rail — a rail that shows Ontology and Review
 * for a remote we know nothing about is advertising a shape we have not
 * established.
 */
export function stepsFor(s: WizardState): StepId[] {
  // Local-only has no dedicated name step: the source step's local pane
  // collects it, so the list is one shorter than the remote list rather
  // than the same length.
  if (s.choice === 'local') return ['source', 'ontology', 'review'];
  // No probe yet, or the probe came back unreachable: there is no confirmed
  // remote to build the rest of the wizard around, so the list stays at
  // ['source'] — never advertise Access/Review chrome for a remote we could
  // not reach. auth_required alone does NOT disqualify a probe: it is still
  // reachable (the server saw the remote and reported it needs credentials),
  // so that case keeps its access step below.
  if (!s.probe || !s.probe.reachable) return ['source'];
  // A remote with NO branches is a dead end, not a mode. knomit never creates
  // a branch on a remote other than its own agent branch, so there is nothing
  // to cut that branch from; the access step says so and asks for a `main`.
  // Stopping the list here is what keeps the branch step — which would offer
  // an empty list of branches to choose from — out of it.
  if (s.probe.empty) return ['source', 'access'];
  const base: StepId[] = ['source', 'access', 'branch'];
  // THE THIRD STATE BLOCKS. Not established means not established: routing on
  // it either discards the ontology the user chose or writes one over a
  // knowledge base that already had its own, and the ontology is immutable
  // after creation, so neither is correctable afterwards.
  if (establishedAnswer(s) === '') return base;
  // Already a knowledge base → join it, and its ontology governs.
  if (establishedAnswer(s) === 'yes') return [...base, 'review'];
  // Not one yet → the user picks the ontology knomit will write.
  return [...base, 'ontology', 'review'];
}

/**
 * branchCheckBlocked answers "has the initialization check RUN and failed?".
 *
 * `initialized === ''` alone does not mean failure — it is also where the
 * wizard starts, before anything has been pressed. `initializedDetail` is what
 * separates "asked and could not tell" from "not asked yet", and conflating
 * them puts a failure card in front of a reader who has done nothing.
 *
 * It lives here, exported, because two places need the same answer: the branch
 * step renders the block, and the footer's action reads "Try again" instead of
 * "Continue". Written twice, they would eventually disagree about what a
 * failed check looks like.
 */
export function branchCheckBlocked(s: WizardState): boolean {
  return s.initialized === '' && s.initializedDetail !== '';
}

/**
 * createBodyFor maps wizard state onto the create wire format.
 *
 * The mode is DERIVED, never chosen by the user:
 *   local                → 'custom' when yaml was supplied, else 'preset'
 *   remote, initialized  → 'clone'      (carries NO ontology — the backend refuses one)
 *   remote, not yet      → 'initialize' (carries the chosen ontology)
 *
 * There is deliberately no arm for `initialized === ''`. stepsFor never puts a
 * review step in front of an unestablished check, so this is unreachable from
 * the wizard; it falls through to 'clone', the mode that CANNOT write an
 * ontology — the conservative half of the pair, since a wrong 'clone' is
 * refused by the backend while a wrong 'initialize' would write over a
 * knowledge base.
 */
export function createBodyFor(s: WizardState): CreateRepoBody {
  // `s.preset || s.seedPreset`, never `|| 'default'`: the fallback fires
  // exactly when SET_YAML has cleared s.preset, and hardcoding 'default' there
  // hands a user who chose "code" an ontology they did not pick — permanently,
  // since it is immutable after creation. seedPreset is the same question
  // asked of state that SET_YAML does not clear.
  if (s.choice === 'local') {
    return s.yaml
      ? { name: s.name, mode: 'custom', ontology_yaml: s.yaml }
      : { name: s.name, mode: 'preset', ontology_preset: s.preset || s.seedPreset };
  }
  const origin = originFor(s);
  if (establishedAnswer(s) === 'no') {
    return s.yaml
      ? { name: s.name, mode: 'initialize', ontology_yaml: s.yaml, origin }
      : { name: s.name, mode: 'initialize', ontology_preset: s.preset || s.seedPreset, origin };
  }
  return { name: s.name, mode: 'clone', origin };
}

/**
 * authFor resolves the wizard's credential fields into the {auth_method,
 * auth_token} pair the backend consumes.
 *
 * It is EXPORTED because the probe and the create must agree: probing
 * anonymously and then creating with a token means the step list — and
 * therefore the MODE — was derived from an answer about a different request.
 * That is precisely how a private empty remote used to be classified
 * `empty:false` and created as `clone`, silently discarding the ontology the
 * user picked. One function, both call sites.
 */
export function authFor(s: WizardState): { auth_method: string; auth_token: string } {
  // Auto-detect ('') resolves to anonymous/SSH and ignores any token. A token
  // typed under auto-detect (the common private-HTTPS case) promotes to
  // explicit token auth so the credential is actually used — carried over from
  // the old CreateRepoForm, where dropping it silently broke private clones.
  //
  // NOT for an SSH URL. Promoting there sends auth_method 'token', which stops
  // the backend's resolveAuthWithOrigin from auto-detecting ssh and hands
  // go-git a githttp.BasicAuth for a git@/ssh:// endpoint — rejected as
  // "invalid auth method" before any network call. Leaving auto-detect in
  // place lets the backend promote to ssh, which is the only credential that
  // transport can use. Typing a token against an SSH URL is a mistake the
  // access step names out loud; it must not also produce a broken request.
  const method = s.authMethod === '' && s.authToken.trim() !== '' && transportFor(s.url) !== 'ssh'
    ? 'token'
    : s.authMethod;
  // Assemble the token each method consumes, so a credential typed under
  // auto-detect and then abandoned (method switched to none/ssh) is neither
  // sent nor persisted. Basic carries "user:password" (matching the backend's
  // assembleAuthToken convention); token carries the raw secret.
  const token =
    method === 'token' ? s.authToken :
    method === 'basic' ? (s.authUser !== '' ? `${s.authUser}:${s.authToken}` : s.authToken) :
    '';
  return { auth_method: method, auth_token: token };
}

function originFor(s: WizardState): NonNullable<CreateRepoBody['origin']> {
  return { url: s.url, branch: s.branch, ...authFor(s) };
}

/**
 * isValidRepoName mirrors the backend's isValidRepoName (internal/repos/
 * manager.go): non-empty, and lowercase alphanumerics, '-' and '_' only.
 *
 * Kept client-side so "My KB" is refused where it was typed rather than
 * becoming a 400 from POST /repos — the deleted CreateRepoForm's own comment
 * called that 400 "confusing", and the wizard reintroduced it by gating Next
 * on nothing but `name.trim().length > 0`.
 *
 * The backend rule stays authoritative; this only refuses earlier. If the two
 * ever disagree, the create still fails safely on the server.
 */
export function isValidRepoName(name: string): boolean {
  return /^[a-z0-9_-]+$/.test(name);
}

/**
 * Transport names which credential a URL can actually use.
 *
 * Mirrors resolveAuthWithOrigin (internal/repos/auth.go): a `git@` or `ssh://`
 * URL is promoted to SSH auth under auto-detect, and go-git then dispatches by
 * SCHEME — so a token typed against one of those is assembled into HTTP basic
 * auth that the SSH transport never consults. The access step says so out loud
 * rather than offering a field that silently does nothing.
 *
 * 'path' is a local origin (validateLocalOrigin gates those server-side); it
 * needs no credential at all.
 */
export type Transport = 'ssh' | 'http' | 'path';

export function transportFor(url: string): Transport {
  const u = url.trim();
  if (u.startsWith('git@') || u.startsWith('ssh://')) return 'ssh';
  if (/^https?:\/\//i.test(u)) return 'http';
  return 'path';
}

/**
 * repoNameFromURL derives a default repository name from a remote URL.
 *
 * The name was the one field the access step demanded and the URL already
 * answered: git@host:org/arxiv-kb.git is going to be called "arxiv-kb", and
 * making the reader retype it is the reason Next sat disabled with no stated
 * cause. Splitting on both '/' and ':' covers scp-syntax (git@host:org/repo)
 * and real URLs with one expression.
 *
 * It returns '' rather than a guess whenever the derived name would not pass
 * isValidRepoName — prefilling a name the backend will reject just moves the
 * 400 to a field the user did not type in. Lowercasing first is deliberate and
 * safe: "ArXiv-KB" is a name the user plainly meant, and case is the only thing
 * standing between it and a valid one.
 */
/**
 * hostOf names the remote the way the user typed it, so a card can say
 * "github.com wants credentials" for ANY host — GitLab, Bitbucket, Gitea, a
 * bare SSH box — without this codebase knowing a single vendor. Every string
 * the UI puts in front of a reader about "who refused" comes from here.
 *
 * Returns '' when there is no host to name (a local path). Callers fall back
 * to a generic noun rather than printing an empty string.
 */
export function hostOf(url: string): string {
  const u = url.trim();
  // scp syntax first: git@host:org/repo has no scheme for a URL parser to find.
  const scp = /^[^@/]+@([^:]+):/.exec(u);
  if (scp) return scp[1];
  const scheme = /^[a-z][a-z0-9+.-]*:\/\/(?:[^@/]+@)?([^/:]+)/i.exec(u);
  if (scheme) return scheme[1];
  return '';
}

/**
 * httpsFromSSH rewrites an SSH remote as its HTTPS equivalent, or returns ''
 * when it cannot do so faithfully.
 *
 * This exists because "use a token" and "use an SSH URL" are mutually
 * exclusive, and the access step should not leave a reader holding a token
 * they have no way to spend. Offering the same repository's HTTPS address is
 * the one move that turns their token into something usable.
 *
 * Returns '' for a URL carrying an explicit port: git@host:2222/org/repo is
 * ambiguous with scp syntax, and a rewrite that quietly drops or keeps the
 * port would be a guess about someone's infrastructure. Callers offer nothing
 * rather than offering something wrong.
 */
export function httpsFromSSH(url: string): string {
  const u = url.trim();
  // scp syntax: git@host:org/repo.git — the part after ':' is a PATH, unless
  // it starts with digits, which means a port and puts us in guessing territory.
  const scp = /^[^@/]+@([^:/]+):(.+)$/.exec(u);
  if (scp) {
    const [, host, path] = scp;
    if (/^\d+\//.test(path)) return '';
    return `https://${host}/${path.replace(/^\/+/, '')}`;
  }
  const ssh = /^ssh:\/\/(?:[^@/]+@)?([^/:]+)(?::(\d+))?\/(.+)$/.exec(u);
  if (ssh) {
    const [, host, port, path] = ssh;
    if (port) return '';
    return `https://${host}/${path}`;
  }
  return '';
}

export function repoNameFromURL(url: string): string {
  const trimmed = url.trim().replace(/\/+$/, '');
  if (trimmed === '') return '';
  const segment = trimmed.split(/[/:]/).pop() ?? '';
  const bare = segment.replace(/\.git$/i, '').toLowerCase();
  return isValidRepoName(bare) ? bare : '';
}
