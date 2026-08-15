import { describe, it, expect } from 'vitest';
import { initialWizardState, wizardReducer, stepsFor, createBodyFor, currentStep } from './wizardState';

describe('stepsFor', () => {
  it('omits the ontology step when the remote already has content', () => {
    const s = wizardReducer(initialWizardState, {
      type: 'PROBE_DONE',
      probe: { reachable: true, empty: false, auth_required: false, upstream_branch: 'main', branches: ['main'] },
    });
    expect(stepsFor(s)).toEqual(['source', 'access', 'review']);
  });

  it('includes the ontology step when the remote is empty', () => {
    const s = wizardReducer(initialWizardState, {
      type: 'PROBE_DONE',
      probe: { reachable: true, empty: true, auth_required: false, upstream_branch: 'main', branches: [] },
    });
    expect(stepsFor(s)).toEqual(['source', 'access', 'ontology', 'review']);
  });

  it('includes the ontology step and skips access for local-only', () => {
    const s = wizardReducer(initialWizardState, { type: 'CHOOSE_LOCAL' });
    expect(stepsFor(s)).toEqual(['source', 'name', 'ontology', 'review']);
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

  it('advances stepIndex to 1 unconditionally on CHOOSE_LOCAL', () => {
    const s = wizardReducer(initialWizardState, { type: 'CHOOSE_LOCAL' });
    expect(s.stepIndex).toBe(1);
  });
});

// Review finding 1: any action that changes choice/probe can shrink the
// derived step list out from under a stepIndex that used to be valid for the
// old list — not just the two actions R2 named an explicit advance rule for.
describe('stepIndex stays in range of stepsFor(state)', () => {
  it('clamps back to 0 when CHOOSE_REMOTE follows CHOOSE_LOCAL (repro a)', () => {
    let s = wizardReducer(initialWizardState, { type: 'CHOOSE_LOCAL' }); // stepIndex -> 1
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
    let s = wizardReducer(initialWizardState, {
      type: 'PROBE_DONE',
      probe: { reachable: true, empty: true, auth_required: false, upstream_branch: 'main', branches: [] },
    }); // list ['source','access','ontology','review']
    s = wizardReducer(s, { type: 'GOTO', step: 'ontology' });
    expect(s.stepIndex).toBe(2);
  });

  it('GOTO is a no-op for a step not in the current list', () => {
    let s = wizardReducer(initialWizardState, {
      type: 'PROBE_DONE',
      probe: { reachable: true, empty: false, auth_required: false, upstream_branch: 'main', branches: ['main'] },
    }); // list ['source','access','review'] — no 'ontology' or 'name'
    const before = s.stepIndex;
    s = wizardReducer(s, { type: 'GOTO', step: 'ontology' });
    expect(s.stepIndex).toBe(before);
  });
});

describe('currentStep', () => {
  it('reads the step at the (in-range) stepIndex', () => {
    const s = wizardReducer(initialWizardState, { type: 'CHOOSE_LOCAL' });
    expect(currentStep(s)).toBe('name');
  });

  it('clamps at read time even if stepIndex were somehow out of range', () => {
    // Construct a state a future action might produce if it forgot to clamp:
    // choice back to null/'remote' with no probe (list ['source']) but a
    // stale stepIndex left over from a richer list. currentStep must not
    // return undefined for this — that is exactly the blank-pane failure
    // the reviewer flagged.
    const stale = { ...initialWizardState, choice: 'remote' as const, stepIndex: 3 };
    expect(stepsFor(stale)).toEqual(['source']);
    expect(currentStep(stale)).toBe('source');
  });
});

describe('createBodyFor', () => {
  it('sends mode seed with the chosen ontology for an empty remote', () => {
    let s = wizardReducer(initialWizardState, { type: 'SET_URL', url: 'https://h/r.git' });
    s = wizardReducer(s, {
      type: 'PROBE_DONE',
      probe: { reachable: true, empty: true, auth_required: false, upstream_branch: 'main', branches: [] },
    });
    s = wizardReducer(s, { type: 'SET_NAME', name: 'kb' });
    s = wizardReducer(s, { type: 'SET_PRESET', preset: 'code' });
    expect(createBodyFor(s)).toMatchObject({
      name: 'kb', mode: 'seed', ontology_preset: 'code',
      origin: { url: 'https://h/r.git' },
    });
  });

  it('sends mode clone with NO ontology when the remote has content', () => {
    let s = wizardReducer(initialWizardState, { type: 'SET_URL', url: 'https://h/r.git' });
    s = wizardReducer(s, {
      type: 'PROBE_DONE',
      probe: { reachable: true, empty: false, auth_required: false, upstream_branch: 'main', branches: ['main'] },
    });
    s = wizardReducer(s, { type: 'SET_NAME', name: 'kb' });
    const body = createBodyFor(s);
    expect(body.mode).toBe('clone');
    // The backend REFUSES an ontology in clone mode (rejectOntologySpecForClone).
    expect(body.ontology_preset).toBeUndefined();
    expect(body.ontology_yaml).toBeUndefined();
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
});
