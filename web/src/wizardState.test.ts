import { describe, it, expect } from 'vitest';
import { initialWizardState, wizardReducer, stepsFor, createBodyFor, currentStep, authFor, isValidRepoName, transportFor, repoNameFromURL,
  probeIsCurrent, establishedAnswer } from './wizardState';
import type { ProbeResult } from './api';

describe('transportFor', () => {
  it('reads scp-syntax and ssh:// as SSH, mirroring resolveAuthWithOrigin', () => {
    expect(transportFor('git@github.com:knomit/arxiv-kb.git')).toBe('ssh');
    expect(transportFor('ssh://git@host:2222/org/repo.git')).toBe('ssh');
  });

  it('reads http and https as HTTP', () => {
    expect(transportFor('https://github.com/knomit/arxiv-kb.git')).toBe('http');
    expect(transportFor('http://internal/git/kb.git')).toBe('http');
  });

  it('reads anything else as a local path', () => {
    expect(transportFor('/srv/git/kb.git')).toBe('path');
  });
});

describe('repoNameFromURL', () => {
  it('takes the last segment of scp-syntax and real URLs alike', () => {
    expect(repoNameFromURL('git@github.com:knomit/arxiv-kb.git')).toBe('arxiv-kb');
    expect(repoNameFromURL('https://github.com/knomit/arxiv-kb.git')).toBe('arxiv-kb');
    expect(repoNameFromURL('ssh://git@host:2222/org/repo.git')).toBe('repo');
    expect(repoNameFromURL('/srv/git/kb')).toBe('kb');
  });

  it('tolerates a trailing slash and a missing .git', () => {
    expect(repoNameFromURL('https://github.com/knomit/arxiv-kb/')).toBe('arxiv-kb');
  });

  it('lowercases, because case is all that stands between it and a valid name', () => {
    expect(repoNameFromURL('git@host:org/ArXiv-KB.git')).toBe('arxiv-kb');
  });

  // Prefilling a name the backend rejects just moves the 400 to a field the
  // user never typed in.
  it('returns empty rather than a name isValidRepoName would refuse', () => {
    expect(repoNameFromURL('git@host:org/my.kb.git')).toBe('');
    expect(repoNameFromURL('')).toBe('');
  });
});

// probed builds the state a reachable remote leaves behind, so each case below
// says only what it is actually about.
function probed(over: Partial<ProbeResult> = {}) {
  return wizardReducer(initialWizardState, {
    type: 'PROBE_DONE',
    probe: {
      reachable: true, empty: false, auth_required: false,
      upstream_branch: 'main', branches: ['main'], ...over,
    },
  });
}

// checked runs the branch check to a definite answer on top of that.
function checked(answer: 'yes' | 'no') {
  return wizardReducer(probed(), { type: 'INITIALIZED_DONE', result: { initialized: answer, branch: 'main' } });
}

describe('stepsFor', () => {
  // THE THREE OUTCOMES of the one question that shapes this wizard.
  //
  // A remote is a knomit knowledge base if and only if the chosen branch
  // carries an ontology. Present → join it, and there is nothing to ask about
  // the ontology. Absent → the user picks one for knomit to write. Not
  // established → the list STOPS at the branch step, because routing on a
  // guess is unrecoverable either way: the ontology is fixed at create time,
  // so guessing "present" discards the ontology the user chose and guessing
  // "absent" writes one over a knowledge base that already had its own.
  it('ends at the branch step while the initialization check is unestablished', () => {
    expect(stepsFor(probed())).toEqual(['source', 'access', 'branch']);
  });

  it('omits the ontology step when the branch already holds a knowledge base', () => {
    expect(stepsFor(checked('yes'))).toEqual(['source', 'access', 'branch', 'review']);
  });

  it('includes the ontology step when the branch is not a knowledge base yet', () => {
    expect(stepsFor(checked('no'))).toEqual(['source', 'access', 'branch', 'ontology', 'review']);
  });

  // A remote with NO branches is a dead end, not a mode: knomit never creates a
  // branch on a remote other than its own, so there is nothing to cut that
  // branch from. The list stops at access, which is where the wizard says so —
  // and the branch step is kept OUT, since it would offer an empty list of
  // branches to choose between.
  //
  // This case used to be the "seed" path and grew the LONGEST list of all
  // (source · access · ontology · review). It is now the shortest remote list
  // there is, because the create it led to failed against every host that
  // protects a new project's default branch.
  it('stops at access for a remote with no branches at all', () => {
    expect(stepsFor(probed({ empty: true, branches: [] }))).toEqual(['source', 'access']);
  });

  // Changing the branch un-asks the question: the established answer was about
  // a different branch, and a repo can carry the ontology on main and not on
  // develop. The list must collapse back to the blocked shape rather than keep
  // routing on the stale answer.
  it('collapses back to the branch step when the branch changes', () => {
    const s = wizardReducer(checked('no'), { type: 'SET_BRANCH', branch: 'develop' });
    expect(stepsFor(s)).toEqual(['source', 'access', 'branch']);
  });

  // Same for the credential: the check runs with whatever is set, so an answer
  // obtained with a different one is an answer about a different request.
  it('collapses back to the branch step when the credential changes', () => {
    const s = wizardReducer(checked('yes'), { type: 'SET_TOKEN', token: 'ghp_x' });
    expect(stepsFor(s)).toEqual(['source', 'access', 'branch']);
  });

  // Local-only has no 'name' step: the source step's local pane collects the
  // name, so this list is one shorter than the remote-empty list.
  it('includes the ontology step and skips access for local-only', () => {
    const s = wizardReducer(initialWizardState, { type: 'CHOOSE_LOCAL' });
    expect(stepsFor(s)).toEqual(['source', 'ontology', 'review']);
  });

  // Review finding (round 2): an unreachable probe is a non-null probe with
  // reachable: false — it used to fall through stepsFor's `!s.probe` check
  // straight to the 3-step remote-with-content list, so Task 8 would render
  // Access/Review chrome for a remote the server never actually reached.
  it('yields only [source] when the probe is unreachable', () => {
    const s = wizardReducer(initialWizardState, {
      type: 'PROBE_DONE',
      probe: { reachable: false, empty: false, auth_required: false, upstream_branch: '', branches: [], detail: 'connection refused' },
    });
    expect(stepsFor(s)).toEqual(['source']);
  });

  // Pin against over-correcting the fix above: auth_required alone does NOT
  // mean unreachable. The server saw the remote and reported it needs
  // credentials, so the access step must still be offered.
  it('keeps the access step when the probe is reachable but requires auth', () => {
    const s = probed({ auth_required: true });
    expect(stepsFor(s)).toEqual(['source', 'access', 'branch']);
  });
});

// The unestablished state is a THIRD state, and the reducer must keep it one.
// Every way of coercing it into 'no' routes the wizard to `initialize`, which
// writes an ontology — permanently, since it cannot be changed after creation.
describe('INITIALIZED_DONE', () => {
  it('records a definite yes', () => {
    const s = wizardReducer(probed(), { type: 'INITIALIZED_DONE', result: { initialized: 'yes' } });
    expect(s.initialized).toBe('yes');
    expect(s.initializedDetail).toBe('');
  });

  it('records a definite no', () => {
    const s = wizardReducer(probed(), { type: 'INITIALIZED_DONE', result: { initialized: 'no' } });
    expect(s.initialized).toBe('no');
  });

  // An ABSENT `initialized` is the backend saying "the check did not complete".
  // It must land as '' with the reason kept, never as 'no'.
  it('keeps an absent answer unestablished and keeps the reason', () => {
    const s = wizardReducer(probed(), {
      type: 'INITIALIZED_DONE',
      result: { detail: 'repository not found' },
    });
    expect(s.initialized).toBe('');
    expect(s.initializedDetail).toBe('repository not found');
    expect(stepsFor(s)).toEqual(['source', 'access', 'branch']);
  });

  // A failed re-check must not leave a previous definite answer standing:
  // whatever it was about may no longer be true, and the wizard would route on
  // it while showing the reader a failure.
  it('un-establishes a previously definite answer when a later check fails', () => {
    const s = wizardReducer(checked('yes'), {
      type: 'INITIALIZED_DONE',
      result: { detail: 'timed out' },
    });
    expect(s.initialized).toBe('');
  });
});

describe('wizardReducer step advancement', () => {
  it('advances stepIndex to 1 when a probe resolves reachable', () => {
    const s = wizardReducer(initialWizardState, {
      type: 'PROBE_DONE',
      probe: { reachable: true, empty: false, auth_required: false, upstream_branch: 'main', branches: ['main'] },
    });
    expect(s.stepIndex).toBe(1);
  });

  it('does NOT advance stepIndex when a probe resolves unreachable', () => {
    const s = wizardReducer(initialWizardState, {
      type: 'PROBE_DONE',
      probe: { reachable: false, empty: false, auth_required: false, upstream_branch: '', branches: [], detail: 'connection refused' },
    });
    expect(s.stepIndex).toBe(0);
  });

  // The source step is a segmented control, so neither choice navigates: both
  // discloses the fields their branch needs in place and leave advancing to
  // the footer's Next. CHOOSE_LOCAL used to jump to stepIndex 1 because the
  // name lived on a step of its own.
  it('does NOT advance stepIndex on CHOOSE_LOCAL', () => {
    const s = wizardReducer(initialWizardState, { type: 'CHOOSE_LOCAL' });
    expect(s.stepIndex).toBe(0);
    expect(currentStep(s)).toBe('source');
  });

  it('does NOT advance stepIndex on CHOOSE_REMOTE', () => {
    const s = wizardReducer(initialWizardState, { type: 'CHOOSE_REMOTE' });
    expect(s.stepIndex).toBe(0);
    expect(currentStep(s)).toBe('source');
  });
});

describe('name prefill on PROBE_DONE', () => {
  const probe = { reachable: true, empty: true, auth_required: false, upstream_branch: 'main', branches: [] };

  it('fills an empty name from the URL', () => {
    let s = wizardReducer(initialWizardState, { type: 'SET_URL', url: 'git@github.com:knomit/arxiv-kb.git' });
    s = wizardReducer(s, { type: 'PROBE_DONE', probe });
    expect(s.name).toBe('arxiv-kb');
  });

  // The prefill is a default offered once, not a value re-asserted on every
  // re-probe — the access step re-probes whenever credentials are supplied.
  it('never overwrites a name the user typed', () => {
    let s = wizardReducer(initialWizardState, { type: 'SET_URL', url: 'git@github.com:knomit/arxiv-kb.git' });
    s = wizardReducer(s, { type: 'SET_NAME', name: 'papers' });
    s = wizardReducer(s, { type: 'PROBE_DONE', probe });
    expect(s.name).toBe('papers');
  });

  it('leaves the name empty when the URL yields no valid one', () => {
    let s = wizardReducer(initialWizardState, { type: 'SET_URL', url: 'git@host:org/my.kb.git' });
    s = wizardReducer(s, { type: 'PROBE_DONE', probe });
    expect(s.name).toBe('');
  });
});

// Review finding 1: any action that changes choice/probe can shrink the
// derived step list out from under a stepIndex that used to be valid for the
// old list — not just the two actions R2 named an explicit advance rule for.
describe('stepIndex stays in range of stepsFor(state)', () => {
  it('clamps back to 0 when CHOOSE_REMOTE follows CHOOSE_LOCAL (repro a)', () => {
    // Walk the local branch forward first — CHOOSE_LOCAL itself no longer
    // advances, so the stale index this guards against has to be earned.
    let s = wizardReducer(initialWizardState, { type: 'CHOOSE_LOCAL' });
    s = wizardReducer(s, { type: 'NEXT' }); // -> 1 ('ontology')
    expect(s.stepIndex).toBe(1);
    s = wizardReducer(s, { type: 'CHOOSE_REMOTE' }); // list collapses to ['source']
    expect(stepsFor(s)).toEqual(['source']);
    expect(s.stepIndex).toBe(0);
    expect(stepsFor(s)[s.stepIndex]).toBeDefined();
  });

  it('clamps back to 0 when a corrected SET_URL resets the probe (repro b)', () => {
    let s = wizardReducer(initialWizardState, { type: 'SET_URL', url: 'https://h/typo.git' });
    s = wizardReducer(s, {
      type: 'PROBE_DONE',
      probe: { reachable: true, empty: false, auth_required: false, upstream_branch: 'main', branches: ['main'] },
    }); // stepIndex -> 1, list ['source','access','review']
    expect(s.stepIndex).toBe(1);
    // User spots the typo and retypes the URL — probe resets to null, list
    // collapses back to ['source']. Without the clamp, stepIndex would still
    // read 1: one past the end of a 1-element list.
    s = wizardReducer(s, { type: 'SET_URL', url: 'https://h/fixed-typo.git' });
    expect(stepsFor(s)).toEqual(['source']);
    expect(s.stepIndex).toBe(0);
  });

  it('NEXT never advances past the end of the derived list', () => {
    let s = wizardReducer(initialWizardState, {
      type: 'PROBE_DONE',
      probe: { reachable: true, empty: false, auth_required: false, upstream_branch: 'main', branches: ['main'] },
    }); // list ['source','access','review'], stepIndex 1
    s = wizardReducer(s, { type: 'NEXT' }); // -> 2 ('review')
    s = wizardReducer(s, { type: 'NEXT' }); // stays at 2, nothing past 'review'
    expect(s.stepIndex).toBe(2);
  });

  it('BACK never goes below 0', () => {
    const s = wizardReducer(initialWizardState, { type: 'BACK' });
    expect(s.stepIndex).toBe(0);
  });

  it('GOTO jumps to a step present in the current list', () => {
    // ['source','access','branch','ontology','review']
    const s = wizardReducer(checked('no'), { type: 'GOTO', step: 'ontology' });
    expect(s.stepIndex).toBe(3);
  });

  it('GOTO is a no-op for a step not in the current list', () => {
    // ['source','access','branch','review'] — no 'ontology'
    const s0 = checked('yes');
    const s = wizardReducer(s0, { type: 'GOTO', step: 'ontology' });
    expect(s.stepIndex).toBe(s0.stepIndex);
  });
});

describe('currentStep', () => {
  it('reads the step at the (in-range) stepIndex', () => {
    let s = wizardReducer(initialWizardState, { type: 'CHOOSE_LOCAL' });
    s = wizardReducer(s, { type: 'NEXT' });
    expect(currentStep(s)).toBe('ontology');
  });

  it('clamps at read time even if stepIndex were somehow out of range', () => {
    // Construct a state a future action might produce if it forgot to clamp:
    // choice back to 'remote' with no probe (list ['source']) but a stale
    // stepIndex left over from a richer list. currentStep must not
    // return undefined for this — that is exactly the blank-pane failure
    // the reviewer flagged.
    const stale = { ...initialWizardState, choice: 'remote' as const, stepIndex: 3 };
    expect(stepsFor(stale)).toEqual(['source']);
    expect(currentStep(stale)).toBe('source');
  });
});

describe('createBodyFor', () => {
  // remoteState walks the wizard the way a user does — URL, probe, branch
  // check — so the mode below is derived from the same state machine the app
  // runs rather than from a hand-built object.
  function remoteState(answer: 'yes' | 'no') {
    let s = wizardReducer(initialWizardState, { type: 'SET_URL', url: 'https://h/r.git' });
    s = wizardReducer(s, {
      type: 'PROBE_DONE',
      probe: { reachable: true, empty: false, auth_required: false, upstream_branch: 'main', branches: ['main'] },
    });
    s = wizardReducer(s, { type: 'SET_NAME', name: 'kb' });
    return wizardReducer(s, { type: 'INITIALIZED_DONE', result: { initialized: answer, branch: 'main' } });
  }

  it('sends mode initialize with the chosen ontology when the branch is not a knowledge base', () => {
    const s = wizardReducer(remoteState('no'), { type: 'SET_PRESET', preset: 'code' });
    expect(createBodyFor(s)).toMatchObject({
      name: 'kb', mode: 'initialize', ontology_preset: 'code',
      origin: { url: 'https://h/r.git' },
    });
  });

  it('sends mode clone with NO ontology when the branch already holds one', () => {
    const body = createBodyFor(remoteState('yes'));
    expect(body.mode).toBe('clone');
    // The backend REFUSES an ontology in clone mode (rejectOntologySpecForClone).
    expect(body.ontology_preset).toBeUndefined();
    expect(body.ontology_yaml).toBeUndefined();
  });

  // The conservative fallback. stepsFor never puts a review step in front of an
  // unestablished check, so this is unreachable from the UI — but if it ever
  // becomes reachable it must land on the mode that CANNOT write an ontology.
  // A wrong 'clone' is refused by the backend; a wrong 'initialize' writes over
  // a knowledge base, and the ontology is immutable after creation.
  it('never sends mode initialize while the check is unestablished', () => {
    let s = wizardReducer(initialWizardState, { type: 'SET_URL', url: 'https://h/r.git' });
    s = wizardReducer(s, {
      type: 'PROBE_DONE',
      probe: { reachable: true, empty: false, auth_required: false, upstream_branch: 'main', branches: ['main'] },
    });
    s = wizardReducer(s, { type: 'SET_NAME', name: 'kb' });
    s = wizardReducer(s, { type: 'SET_PRESET', preset: 'code' });
    const body = createBodyFor(s);
    expect(body.mode).toBe('clone');
    expect(body.ontology_preset).toBeUndefined();
  });

  it('sends mode custom with raw yaml for a local-only repo with an uploaded ontology', () => {
    let s = wizardReducer(initialWizardState, { type: 'CHOOSE_LOCAL' });
    s = wizardReducer(s, { type: 'SET_NAME', name: 'scratch' });
    s = wizardReducer(s, { type: 'SET_YAML', yaml: 'id: x\nname: X\ntopics:\n  a:\n' });
    expect(createBodyFor(s)).toMatchObject({ mode: 'custom', ontology_yaml: 'id: x\nname: X\ntopics:\n  a:\n' });
  });

  it('promotes auto-detect + token to explicit token auth', () => {
    let s = wizardReducer(initialWizardState, { type: 'SET_URL', url: 'https://h/private.git' });
    s = wizardReducer(s, {
      type: 'PROBE_DONE',
      probe: { reachable: true, empty: false, auth_required: true, upstream_branch: 'main', branches: ['main'] },
    });
    s = wizardReducer(s, { type: 'SET_NAME', name: 'kb' });
    s = wizardReducer(s, { type: 'SET_TOKEN', token: 'ghp_secret' });
    expect(createBodyFor(s).origin).toMatchObject({ auth_method: 'token', auth_token: 'ghp_secret' });
  });

  // Regression (R1): the old CreateRepoForm.test.tsx covered this pairing —
  // a private-HTTPS clone once silently dropped its credential when
  // auto-detect auth was left in place with no token typed. Auto-detect with
  // NO token must send auth_method '' and auth_token '' — it must NOT get
  // promoted to 'token'. The promotion case (with a token) is covered above;
  // this is its counterpart.
  it('sends auth_method "" and auth_token "" for auto-detect auth with no token', () => {
    let s = wizardReducer(initialWizardState, { type: 'SET_URL', url: 'https://h/private.git' });
    s = wizardReducer(s, {
      type: 'PROBE_DONE',
      probe: { reachable: true, empty: false, auth_required: true, upstream_branch: 'main', branches: ['main'] },
    });
    s = wizardReducer(s, { type: 'SET_NAME', name: 'kb' });
    expect(createBodyFor(s).origin).toMatchObject({ auth_method: '', auth_token: '' });
  });

  // Ported from the old CreateRepoForm.test.tsx (removed in Task 10): the
  // local-only, no-yaml branch of createBodyFor was otherwise untested here —
  // every other createBodyFor case covers 'seed', 'clone', or 'custom', but
  // not the default 'preset' mode a local-only repo takes when the user
  // never touches "write your own"/upload.
  it('sends mode preset for a local-only repo using the selected preset (no yaml)', () => {
    let s = wizardReducer(initialWizardState, { type: 'CHOOSE_LOCAL' });
    s = wizardReducer(s, { type: 'SET_NAME', name: 'scratch' });
    expect(createBodyFor(s)).toMatchObject({ name: 'scratch', mode: 'preset', ontology_preset: 'default' });
  });

  // Ported from the old CreateRepoForm.test.tsx: explicitly selecting "none"
  // must still send an empty token, not just auto-detect's empty method.
  it('sends auth_method "none" and empty token when None is explicitly selected', () => {
    let s = wizardReducer(initialWizardState, { type: 'SET_URL', url: 'https://h/r.git' });
    s = wizardReducer(s, {
      type: 'PROBE_DONE',
      probe: { reachable: true, empty: false, auth_required: true, upstream_branch: 'main', branches: ['main'] },
    });
    s = wizardReducer(s, { type: 'SET_NAME', name: 'kb' });
    s = wizardReducer(s, { type: 'SET_AUTH_METHOD', method: 'none' });
    expect(createBodyFor(s).origin).toMatchObject({ auth_method: 'none', auth_token: '' });
  });

  // Ported from the old CreateRepoForm.test.tsx. Regression: a token typed
  // under auto-detect and then abandoned (method switched to none) must NOT
  // be shipped — only methods that consume a token send one, so no stale
  // credential is persisted server-side.
  it('drops a stale token when the method is switched away from token/basic', () => {
    let s = wizardReducer(initialWizardState, { type: 'SET_URL', url: 'https://h/r.git' });
    s = wizardReducer(s, {
      type: 'PROBE_DONE',
      probe: { reachable: true, empty: false, auth_required: true, upstream_branch: 'main', branches: ['main'] },
    });
    s = wizardReducer(s, { type: 'SET_NAME', name: 'kb' });
    s = wizardReducer(s, { type: 'SET_TOKEN', token: 'ghp_secret' });
    s = wizardReducer(s, { type: 'SET_AUTH_METHOD', method: 'none' });
    expect(createBodyFor(s).origin).toMatchObject({ auth_method: 'none', auth_token: '' });
  });

  // Ported from the old CreateRepoForm.test.tsx. Regression: basic auth needs
  // a username. The wizard assembles "user:password" into auth_token — the
  // convention the backend (assembleAuthToken / remoteAuthFromRecord /
  // authConfigFromSpec) splits on. Previously a single field sent only the
  // password, producing BasicAuth{Username:"", ...} which fails on real hosts.
  it('assembles user:password into auth_token for basic auth', () => {
    let s = wizardReducer(initialWizardState, { type: 'SET_URL', url: 'https://h/r.git' });
    s = wizardReducer(s, {
      type: 'PROBE_DONE',
      probe: { reachable: true, empty: false, auth_required: true, upstream_branch: 'main', branches: ['main'] },
    });
    s = wizardReducer(s, { type: 'SET_NAME', name: 'kb' });
    s = wizardReducer(s, { type: 'SET_AUTH_METHOD', method: 'basic' });
    s = wizardReducer(s, { type: 'SET_AUTH_USER', user: 'alice' });
    s = wizardReducer(s, { type: 'SET_TOKEN', token: 's3cret' });
    expect(createBodyFor(s).origin).toMatchObject({ auth_method: 'basic', auth_token: 'alice:s3cret' });
  });
});

describe('authFor', () => {
  // The probe and the create MUST resolve credentials identically. When the
  // probe went out anonymously and the create carried a token, the step list —
  // and therefore the mode — was derived from an answer about a different
  // request: a private empty remote probed as {auth_required:true,
  // empty:false}, which stepsFor reads as "has content" and createBodyFor
  // turned into mode 'clone'.
  it('is the same resolution createBodyFor sends on the origin', () => {
    let s = wizardReducer(initialWizardState, { type: 'SET_URL', url: 'https://h/r.git' });
    s = wizardReducer(s, {
      type: 'PROBE_DONE',
      probe: { reachable: true, empty: true, auth_required: false, upstream_branch: 'main', branches: [] },
    });
    s = wizardReducer(s, { type: 'SET_NAME', name: 'kb' });
    s = wizardReducer(s, { type: 'SET_TOKEN', token: 'ghp_x' });
    expect(authFor(s)).toEqual({ auth_method: 'token', auth_token: 'ghp_x' });
    expect(createBodyFor(s).origin).toMatchObject(authFor(s));
  });

  it('sends nothing when no credential was typed, so an anonymous probe stays anonymous', () => {
    expect(authFor(initialWizardState)).toEqual({ auth_method: '', auth_token: '' });
  });
});

describe('isValidRepoName', () => {
  // Mirrors internal/repos/manager.go's isValidRepoName exactly: non-empty,
  // lowercase alphanumerics, '-' and '_'. Anything else is a 400 from POST
  // /repos, and the point of having this client-side is that the user never
  // meets that 400.
  it('accepts what the backend accepts', () => {
    for (const ok of ['work', 'my-kb', 'my_kb', 'a1', '1', '_', '-']) {
      expect(isValidRepoName(ok)).toBe(true);
    }
  });

  it('rejects what the backend rejects', () => {
    for (const bad of ['', ' ', 'My KB', 'My-KB', 'kb.sessions', 'kb/x', 'kb ', 'ünï']) {
      expect(isValidRepoName(bad)).toBe(false);
    }
  });
});

describe('the branch the answer is ABOUT', () => {
  // A create does not read the consensus branch — it reads whatever
  // InitFromRemote adopts, which is this machine's agent branch when the remote
  // already carries one. The server reports which branch it inspected; the
  // reducer used to drop it, so a "yes" established on agent/<host> was
  // rendered as "main already holds a knowledge base" — about the one branch
  // that provably does not.
  it('keeps the branch the server actually inspected', () => {
    const s = wizardReducer(
      { ...initialWizardState, branch: 'main' },
      { type: 'INITIALIZED_DONE', result: { initialized: 'yes', branch: 'agent/box-1' } });
    expect(s.initialized).toBe('yes');
    expect(s.initializedBranch).toBe('agent/box-1');
  });

  // It is part of the answer, so it is unmade with the answer. A branch name
  // left over from the previous check would name the wrong branch just as
  // confidently.
  it('forgets it when the question changes', () => {
    const answered = wizardReducer(
      { ...initialWizardState, branch: 'main' },
      { type: 'INITIALIZED_DONE', result: { initialized: 'yes', branch: 'agent/box-1' } });
    expect(wizardReducer(answered, { type: 'SET_BRANCH', branch: 'develop' }).initializedBranch).toBe('');
    expect(wizardReducer(answered, { type: 'SET_TOKEN', token: 'x' }).initializedBranch).toBe('');
  });
});

// AN ANSWER MUST CARRY THE QUESTION IT ANSWERED.
//
// Every probe result is an answer about one remote, reached with one
// credential, about one branch. Change any of those three and the answer is
// about a request nobody made — but it stayed on screen and kept routing the
// wizard, because invalidation was a list of reset paths that each action had
// to remember to spread. It leaked in four places at once: `probe` survived a
// credential change, `checkedOk` was never reset at all, `branch` and `name`
// survived a URL change.
//
// So an answer now carries a fingerprint of its request, and the selectors
// refuse to speak for it when the fingerprint no longer matches. Nothing has to
// remember anything.
describe('answers are bound to the request that produced them', () => {
  const probed = { reachable: true, empty: false, auth_required: false, upstream_branch: 'main', branches: ['main'] };

  const afterProbe = () => wizardReducer(
    wizardReducer(initialWizardState, { type: 'SET_URL', url: 'https://h/a.git' }),
    { type: 'PROBE_DONE', probe: probed });

  it('a probe speaks for the request it was made with', () => {
    expect(probeIsCurrent(afterProbe())).toBe(true);
  });

  it('and stops speaking when the credential changes', () => {
    const s = wizardReducer(afterProbe(), { type: 'SET_TOKEN', token: 'ghp_x' });
    expect(probeIsCurrent(s)).toBe(false);
    // NOT deleted. Throwing the probe away would collapse the step list and
    // rewind the reader to the first screen, taking the access step — the only
    // place a credential can be typed — with it.
    expect(s.probe).not.toBeNull();
    expect(stepsFor(s)).toContain('access');
  });

  it('and stops speaking when the auth method changes', () => {
    expect(probeIsCurrent(wizardReducer(afterProbe(), { type: 'SET_AUTH_METHOD', method: 'basic' }))).toBe(false);
  });

  it('and stops speaking when the URL changes', () => {
    expect(probeIsCurrent(wizardReducer(afterProbe(), { type: 'SET_URL', url: 'https://h/b.git' }))).toBe(false);
  });

  // The initialization answer is per-branch as well, so its fingerprint carries
  // the branch: a repo can hold the ontology on main and not on develop.
  it('an initialization answer is about one branch of one remote', () => {
    const answered = wizardReducer(
      wizardReducer(afterProbe(), { type: 'SET_BRANCH', branch: 'main' }),
      { type: 'INITIALIZED_DONE', result: { initialized: 'yes', branch: 'main' } });
    expect(establishedAnswer(answered)).toBe('yes');

    const moved = wizardReducer(answered, { type: 'SET_BRANCH', branch: 'develop' });
    expect(establishedAnswer(moved)).toBe('');
    expect(stepsFor(moved)).toEqual(['source', 'access', 'branch']);
  });

  // #15: a corrected URL must not be created against the previous remote's
  // branch. `branch` decides which ref is inspected and cloned.
  it('forgets the branch when the remote changes', () => {
    const onA = wizardReducer(afterProbe(), { type: 'SET_BRANCH', branch: 'develop' });
    expect(wizardReducer(onA, { type: 'SET_URL', url: 'https://h/b.git' }).branch).toBe('');
  });

  // createBodyFor must never route on an answer that is no longer current:
  // 'clone' is the conservative half — the backend refuses it when the branch
  // has no ontology, where a wrong 'initialize' would WRITE one.
  it('never derives initialize from a stale answer', () => {
    const answered = wizardReducer(
      wizardReducer(afterProbe(), { type: 'SET_BRANCH', branch: 'main' }),
      { type: 'INITIALIZED_DONE', result: { initialized: 'no', branch: 'main' } });
    expect(createBodyFor({ ...answered, name: 'kb' }).mode).toBe('initialize');

    const restaled = wizardReducer(answered, { type: 'SET_TOKEN', token: 'other' });
    expect(createBodyFor({ ...restaled, name: 'kb' }).mode).toBe('clone');
  });
});
